//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
)

func recallCall(name, id string, args any) agent.Message {
	raw, _ := json.Marshal(args)
	return agent.Message{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: id, Name: name, Input: raw}}}
}

func TestWorkerRecallSurvivesCompactionAndRecoveryWithoutCrashTail(t *testing.T) {
	j, dir := outcomeJob(t, "explore"), t.TempDir()
	j.Budget.Timeout = 60
	turns, effects, summaries := 0, 0, 0
	resuming := false
	p := scenarioProvider(func(_ context.Context, history []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
		if len(defs) == 0 {
			summaries++
			return agent.Text("assistant", `{"notes":"Continue the investigation.","quotes":[]}`), nil
		}
		if resuming {
			found := false
			for _, message := range history {
				if !strings.HasPrefix(message.Text(), recallPrefix) {
					continue
				}
				found = true
				if !strings.Contains(message.Text(), "scripts/check.py") || !strings.Contains(message.Text(), "FAIL missing fixture") || !strings.Contains(message.Text(), `"is_error":true`) || strings.Contains(message.Text(), "crash-only.py") {
					t.Fatalf("recovery index lost committed work or admitted crash tail: %s", message.Text())
				}
			}
			if !found {
				t.Fatal("resumed model did not receive retained work index")
			}
			return agent.Text("assistant", completedOutput), nil
		}
		turns++
		switch turns {
		case 1:
			return recallCall("write", "script", map[string]string{"path": "scripts/check.py", "content": "print('check')"}), nil
		case 2:
			return recallCall("bash", "test", map[string]string{"command": "python scripts/check.py"}), nil
		default:
			found := false
			for _, message := range history {
				if strings.HasPrefix(message.Text(), recallPrefix) && strings.Contains(message.Text(), "scripts/check.py") {
					found = true
				}
			}
			if !found || summaries == 0 {
				t.Fatal("first compaction erased the deterministic index")
			}
			return agent.Message{}, &agent.ModelError{Kind: agent.ErrorTransport, Err: errors.New("connection reset")}
		}
	})
	tools := []agent.Tool{
		{Definition: agent.Definition{Name: "write", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`)}, Execute: func(context.Context, json.RawMessage) (string, error) {
			effects++
			return `{"path":"scripts/check.py","sha256":"historical-hash"}`, nil
		}},
		{Definition: agent.Definition{Name: "bash", Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)}, Execute: func(context.Context, json.RawMessage) (string, error) {
			effects++
			return "FAIL missing fixture\n" + strings.Repeat("retained diagnostic\n", 10000), errors.New("exit status 1")
		}},
	}
	opts := Options{Provider: p, Tools: tools, RunDir: dir, ContextBytes: 64000}
	first, err := runTestWorker(context.Background(), j, opts)
	if err != nil || !first.Retryable || effects != 2 {
		t.Fatalf("fixture did not interrupt after committed work: %+v %v effects=%d", first, err, effects)
	}
	saved := outcomeSession(t, dir)
	if saved.ContextCheckpoint.CompactionCount == 0 {
		t.Fatal("fixture did not compact")
	}
	file, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	call := recallCall("write", "crash", map[string]string{"path": "crash-only.py", "content": "uncommitted"})
	call.Sequence = saved.ContextCheckpoint.LastSequence + 1
	content, _ := json.Marshal(`{"path":"crash-only.py"}`)
	result := agent.Message{Role: "user", Sequence: call.Sequence + 1, Content: []agent.Block{{Type: "tool_result", ToolUseID: "crash", Content: content}}}
	for _, event := range []agent.Event{{Type: "message_end", Message: &call}, {Type: "tool_start", ToolID: "crash", ToolName: "write"}, {Type: "message_end", Message: &result}} {
		raw, _ := json.Marshal(event)
		if _, err := file.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	file.Close()
	resuming = true
	last, err := runTestWorker(context.Background(), j, opts)
	if err != nil || last.Status != "success" || effects != 2 {
		t.Fatalf("recovery repeated work: %+v %v effects=%d", last, err, effects)
	}
	// Recovery saves a newer physical journal boundary. That must not turn
	// the earlier crash-only records into authoritative work on the next read.
	saved = outcomeSession(t, dir)
	restored := restoreWorkerRecall(context.Background(), j, dir, saved.Log)
	if strings.Contains(restored.contextData(), "crash-only.py") || !strings.Contains(restored.contextData(), "scripts/check.py") {
		t.Fatalf("later checkpoint promoted crash-only work: %s", restored.contextData())
	}
}

func TestWorkerRecallBoundsSeparateFilesFromCommandChurn(t *testing.T) {
	index := &workerRecall{RunID: "own", projection: traceProjector{Own: true}}
	observe := func(sequence uint64, name, subject, content string, failed bool) {
		args := map[string]string{"path": subject, "command": subject}
		call := recallCall(name, fmt.Sprint(sequence), args)
		call.Sequence = sequence
		raw, _ := json.Marshal(content)
		result := agent.Message{Role: "user", Sequence: sequence + 1, Content: []agent.Block{{Type: "tool_result", ToolUseID: fmt.Sprint(sequence), Content: raw, IsError: failed}}}
		for _, event := range []agent.Event{{Type: "message_end", Message: &call}, {Type: "message_end", Message: &result}} {
			if err := index.observe(event); err != nil {
				t.Fatal(err)
			}
		}
	}
	observe(1, "write", "scripts/keep.py", `{"path":"scripts/keep.py"}`, false)
	for n := 0; n < 20; n++ {
		observe(uint64(n*2+3), "bash", "pytest scripts/keep.py", strings.Repeat("<failed>\n", 100), true)
	}
	observe(50, "read_worker_trace", "", strings.Repeat("recursive projection", 100), false)
	data := index.contextData()
	if len(data) > maxRecallBytes || !strings.Contains(data, "scripts/keep.py") || strings.Contains(data, "recursive projection") || index.OmittedCommands != 14 || len(index.Files) != 1 || len(index.Commands) != 6 {
		t.Fatalf("index lost bounded independent retention: %d bytes %s", len(data), data)
	}
	loop := &agent.Loop{ContextData: []string{"shared update", data}}
	index.pin(loop)
	index.pin(loop)
	if len(loop.ContextData) != 2 || loop.ContextData[0] != "shared update" {
		t.Fatal("index duplicated itself or lost runtime updates")
	}
	loop.ContextData = []string{"refreshed update"}
	index.pin(loop)
	if len(loop.ContextData) != 2 || !strings.Contains(loop.ContextData[1], "scripts/keep.py") {
		t.Fatal("runtime update refresh erased retained files")
	}
}

func TestWorkerRecallRejectsCorruptCommittedPrefix(t *testing.T) {
	f := newTraceFixture(t, "FAIL observed")
	raw, _ := os.ReadFile(filepath.Join(f.dir, "session.json"))
	var saved session
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	changed := append([]byte{}, f.raw...)
	changed[len(changed)-3] ^= 1
	if err := os.WriteFile(filepath.Join(f.dir, "events.jsonl"), changed, 0600); err != nil {
		t.Fatal(err)
	}
	index := restoreWorkerRecall(context.Background(), f.job, f.dir, saved.Log)
	if index.Unavailable == "" || len(index.Files)+len(index.Commands) != 0 {
		t.Fatal("unverified observations survived index restore")
	}
}

func TestWorkerRecallRepeatedCompactionExcludesOldIndexSnapshots(t *testing.T) {
	index := &workerRecall{RunID: "own", projection: traceProjector{Own: true}}
	turns, summaries, views := 0, 0, 0
	loop := &agent.Loop{TaskPrompt: "Check the saved script.", ContextBytes: 12000, RecentBytes: 128, SummaryBytes: 2048, ContextDataPrefixes: []string{recallPrefix}}
	loop.BeforeRequest = func(ctx context.Context, l *agent.Loop) (context.Context, error) { index.pin(l); return ctx, nil }
	loop.Emit = func(event agent.Event) {
		if err := index.observe(event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "context_compacted" {
			views++
			found := 0
			for _, m := range event.Compaction.View {
				if strings.HasPrefix(m.Text(), recallPrefix) {
					found++
				}
			}
			if found != 1 {
				t.Fatalf("compacted view contains %d retained indices", found)
			}
		}
	}
	loop.Tools = []agent.Tool{
		{Definition: agent.Definition{Name: "write", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(context.Context, json.RawMessage) (string, error) { return `{"path":"scripts/keep.py"}`, nil }},
		{Definition: agent.Definition{Name: "bash", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(context.Context, json.RawMessage) (string, error) {
			return "FAIL local fixture\n" + strings.Repeat("diagnostic\n", 5000), errors.New("exit status 1")
		}},
	}
	loop.Provider = scenarioProvider(func(_ context.Context, messages []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
		if len(defs) == 0 {
			summaries++
			if strings.Contains(messages[0].Text(), recallPrefix) {
				t.Fatal("derived index became source material in a later summary")
			}
			return agent.Text("assistant", `{"notes":"Continue pending checks.","quotes":[]}`), nil
		}
		turns++
		switch turns {
		case 1:
			return recallCall("write", "script", map[string]string{"path": "scripts/keep.py"}), nil
		case 2, 3:
			return recallCall("bash", fmt.Sprint(turns), map[string]string{"command": fmt.Sprintf("python scripts/keep.py --case %d", turns)}), nil
		default:
			found := false
			for _, m := range messages {
				if strings.HasPrefix(m.Text(), recallPrefix) && strings.Contains(m.Text(), "--case 3") && strings.Contains(m.Text(), "scripts/keep.py") {
					found = true
				}
			}
			if !found {
				t.Fatal("latest deterministic index did not survive repeated compaction")
			}
			return agent.Text("assistant", "done"), nil
		}
	})
	if _, err := loop.Run(context.Background(), loop.TaskPrompt); err != nil || summaries != 2 || views != 2 {
		t.Fatalf("repeated compaction fixture: summaries=%d views=%d err=%v", summaries, views, err)
	}
}
