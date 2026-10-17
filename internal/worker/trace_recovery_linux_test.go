//go:build linux

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	eventlog "pwnmesh/internal/worker/internal/journal"
)

func TestWorkerTraceOwnCommittedObservations(t *testing.T) {
	f := newTraceFixture(t, "saved script checks.py", "FAIL missing fixture")
	f.job.RunID = f.run.RunID
	page := traceCall(t, f.tool(t), traceReadRequest{RunID: f.job.RunID, Query: "checks.py"})
	if !strings.Contains(page, "saved script checks.py") || strings.Contains(page, "FAIL") {
		t.Fatalf("own committed observation unavailable: %s", page)
	}
	// Registration still authorizes the own-run read; a changed session cannot
	// grant access merely because the model supplied its current run id.
	raw, err := os.ReadFile(filepath.Join(f.dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved session
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	saved.Identity.JobDigest = strings.Repeat("0", 64)
	raw, _ = json.Marshal(saved)
	if err := os.WriteFile(filepath.Join(f.dir, "session.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(traceReadRequest{RunID: f.job.RunID})
	if _, err := f.tool(t).Execute(context.Background(), input); err == nil {
		t.Fatal("own-run lookup bypassed saved identity verification")
	}
}

func TestWorkerTraceLargeCommittedJournalPages(t *testing.T) {
	f := newTraceFixture(t, "first needle")
	file, err := os.OpenFile(filepath.Join(f.dir, "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	digest.Write(f.raw)
	writer := io.MultiWriter(file, digest)
	padding, _ := json.Marshal(agent.Event{Type: "text_delta", Text: strings.Repeat("x", 32<<10)})
	padding = append(padding, '\n')
	size := int64(len(f.raw))
	for size <= 64<<20 {
		n, err := writer.Write(padding)
		if err != nil {
			t.Fatal(err)
		}
		size += int64(n)
	}
	text, _ := json.Marshal("last needle")
	m := agent.Message{Role: "user", Sequence: 2, Content: []agent.Block{{Type: "tool_result", ToolUseID: "last", Content: text}}}
	for _, event := range []agent.Event{{Type: "tool_start", ToolID: "last", ToolName: "bash"}, {Type: "message_end", Message: &m}} {
		raw, _ := json.Marshal(event)
		n, err := writer.Write(append(raw, '\n'))
		if err != nil {
			t.Fatal(err)
		}
		size += int64(n)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(f.dir, "session.json"))
	var saved session
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	saved.Log = eventlog.Checkpoint{Offset: size, SHA256: hex.EncodeToString(digest.Sum(nil))}
	raw, _ = json.Marshal(saved)
	if err := os.WriteFile(filepath.Join(f.dir, "session.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Items   []traceObservation `json:"items"`
		Next    *int               `json:"next_offset"`
		Version string             `json:"trace_version"`
	}
	first := traceCall(t, f.tool(t), traceReadRequest{RunID: f.run.RunID, Query: "needle", Limit: 1})
	if err := json.Unmarshal([]byte(first), &page); err != nil || len(page.Items) != 1 || page.Next == nil {
		t.Fatalf("large journal lost first page: %s %v", first, err)
	}
	last := traceCall(t, f.tool(t), traceReadRequest{RunID: f.run.RunID, Offset: *page.Next, Limit: 1, TraceVersion: page.Version})
	if !strings.Contains(last, "last needle") || strings.Contains(last, "first needle") {
		t.Fatalf("large journal continuation lost bound observations: %s", last)
	}
	// A short matching first page must still validate the entire committed
	// prefix. Corruption after the first match cannot release a partial page.
	file, err = os.OpenFile(filepath.Join(f.dir, "events.jsonl"), os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte("!"), size-2); err != nil {
		t.Fatal(err)
	}
	file.Close()
	input, _ := json.Marshal(traceReadRequest{RunID: f.run.RunID, Query: "first needle", Limit: 1})
	if raw, err := f.tool(t).Execute(context.Background(), input); err == nil || raw != "" {
		t.Fatalf("late corruption released a page: %q %v", raw, err)
	}
}

func TestWorkerTraceOwnSubjectsRetainFailureAndExcludeCrashTail(t *testing.T) {
	f := newTraceFixture(t)
	f.job.RunID = f.run.RunID
	appendEvent := func(event agent.Event) { raw, _ := json.Marshal(event); f.raw = append(f.raw, append(raw, '\n')...) }
	call := recallCall("bash", "check", map[string]string{"command": "python scripts/verify.py; AUTH_TOKEN=private-value"})
	call.Sequence = 1
	text, _ := json.Marshal("FAIL missing fixture")
	result := agent.Message{Role: "user", Sequence: 2, Content: []agent.Block{{Type: "tool_result", ToolUseID: "check", Content: text, IsError: true}}}
	appendEvent(agent.Event{Type: "message_end", Message: &call})
	appendEvent(agent.Event{Type: "tool_start", ToolID: "check", ToolName: "bash"})
	appendEvent(agent.Event{Type: "message_end", Message: &result})
	raw, _ := os.ReadFile(filepath.Join(f.dir, "session.json"))
	var saved session
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	saved.Log = eventlog.Checkpoint{Offset: int64(len(f.raw)), SHA256: fmt.Sprintf("%x", sha256.Sum256(f.raw))}
	raw, _ = json.Marshal(saved)
	if err := os.WriteFile(filepath.Join(f.dir, "session.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	tail := call
	tail.Sequence = 3
	appendEvent(agent.Event{Type: "message_end", Message: &tail})
	text, _ = json.Marshal("SUCCESS never committed")
	result.Sequence, result.Content[0].Content, result.Content[0].IsError = 4, text, false
	appendEvent(agent.Event{Type: "message_end", Message: &result})
	if err := os.WriteFile(filepath.Join(f.dir, "events.jsonl"), f.raw, 0600); err != nil {
		t.Fatal(err)
	}
	page := traceCall(t, f.tool(t), traceReadRequest{RunID: f.run.RunID, Query: "scripts/verify.py"})
	if !strings.Contains(page, "FAIL missing fixture") || !strings.Contains(page, `"is_error":true`) || strings.Contains(page, "private-value") || strings.Contains(page, "never committed") {
		t.Fatalf("own trace lost projected provenance: %s", page)
	}
	f.job.RunID = "other"
	page = traceCall(t, f.tool(t), traceReadRequest{RunID: f.run.RunID, Query: "scripts/verify.py"})
	if !strings.Contains(page, `"total":0`) {
		t.Fatalf("own command arguments reached another run: %s", page)
	}
}

func TestWorkerTraceKeepsRecoveredCrashTailExcluded(t *testing.T) {
	f := newTraceFixture(t, "original committed observation")
	var original struct {
		Version string `json:"trace_version"`
	}
	if err := json.Unmarshal([]byte(traceCall(t, f.tool(t), traceReadRequest{RunID: f.run.RunID})), &original); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved session
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	log, err := eventlog.Open(f.dir, &saved.Log, nil)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal("crash-only observation")
	message := agent.Message{Role: "user", Sequence: 9, Content: []agent.Block{{Type: "tool_result", ToolUseID: "crash", Content: content}}}
	start := agent.Event{Type: "tool_start", ToolID: "crash", ToolName: "bash"}
	startRaw, _ := json.Marshal(start)
	crashOffset := saved.Log.Offset + int64(len(startRaw)+1)
	for _, event := range []agent.Event{start, {Type: "message_end", Message: &message}} {
		if err := log.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	log.Close()
	log, err = eventlog.Open(f.dir, &saved.Log, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	saved.Log, err = log.SyncCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(saved)
	if err := os.WriteFile(filepath.Join(f.dir, "session.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	page := traceCall(t, f.tool(t), traceReadRequest{RunID: f.run.RunID})
	var result struct {
		Items   []traceObservation `json:"items"`
		Version string             `json:"trace_version"`
	}
	if err := json.Unmarshal([]byte(page), &result); err != nil || len(result.Items) != 1 || strings.Contains(page, "crash-only") {
		t.Fatalf("recovered trace promoted crash tail: %s %v", page, err)
	}
	detail := traceCall(t, f.tool(t), traceReadRequest{RunID: f.run.RunID, TraceVersion: result.Version, Record: result.Items[0].Record})
	if !strings.Contains(detail, "original committed observation") {
		t.Fatalf("bound trace lost original evidence: %s", detail)
	}
	old := traceCall(t, f.tool(t), traceReadRequest{RunID: f.run.RunID, TraceVersion: original.Version})
	if !strings.Contains(old, "original committed observation") || strings.Contains(old, "crash-only") {
		t.Fatalf("recovery changed a prior bound prefix: %s", old)
	}
	input, _ := json.Marshal(traceReadRequest{RunID: f.run.RunID, TraceVersion: result.Version, Record: fmt.Sprintf("%d:0", crashOffset)})
	if got, err := f.tool(t).Execute(context.Background(), input); err == nil || got != "" {
		t.Fatalf("detail lookup bypassed recovery exclusion: %q %v", got, err)
	}
}
