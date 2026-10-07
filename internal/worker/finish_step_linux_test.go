//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func finishStepInput(t *testing.T, path string) json.RawMessage {
	t.Helper()
	var result map[string]json.RawMessage
	if err := json.Unmarshal([]byte(finalEvidenceOutput(path)), &result); err != nil {
		t.Fatal(err)
	}
	return result["data"]
}

func TestFinishStepStopsWithoutConfirmationAndFreezesOriginal(t *testing.T) {
	j := graphWrapperJob(t)
	j.ResultContractVersion = 2
	dir := t.TempDir()
	path := filepath.Join(j.Workspace, "result.txt")
	if err := os.WriteFile(path, []byte("observed output\n"), 0600); err != nil {
		t.Fatal(err)
	}
	turns, sideEffects := 0, 0
	provider := scenarioProvider(func(_ context.Context, _ []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turns++
		if turns > 1 {
			t.Fatal("finish_step requested another model turn")
		}
		found := false
		for _, d := range definitions {
			found = found || d.Name == "finish_step"
		}
		if !found {
			t.Fatal("finish_step not offered")
		}
		return agent.Message{Role: "assistant", Content: []agent.Block{
			{Type: "tool_use", ID: "finish", Name: "finish_step", Input: finishStepInput(t, path)},
			{Type: "tool_use", ID: "after", Name: "progress", Input: json.RawMessage(`{}`)},
		}}, nil
	})
	r, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Provider: provider, Tools: []agent.Tool{progressTool(&sideEffects, false)}})
	if err != nil || r.Status != "success" || turns != 1 || sideEffects != 0 {
		t.Fatalf("explicit finish did not stop: result=%+v turns=%d sideEffects=%d err=%v", r, turns, sideEffects, err)
	}
	refs := finalEvidenceRefs(t, j, r)
	if len(refs) != 1 || refs[0].Excerpt != "observed output\n" || refs[0].Path == path {
		t.Fatalf("original output was not frozen: %+v", refs)
	}
	if err := os.WriteFile(path, []byte("changed after handoff\n"), 0600); err != nil {
		t.Fatal(err)
	}
	again, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Provider: provider})
	if err != nil || again.Text != r.Text || turns != 1 {
		t.Fatalf("completed handoff replay changed or called model: %+v %v", again, err)
	}
}

func TestFinishStepRecoversLostToolResponseWithOriginalEvidence(t *testing.T) {
	j := graphWrapperJob(t)
	j.ResultContractVersion = 2
	dir := t.TempDir()
	path := filepath.Join(j.Workspace, "result.txt")
	if err := os.WriteFile(path, []byte("original evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The durable tool receipt may precede the Loop's tool-result/session
	// write. Recovery must use it without asking the model to finish again.
	f := &stepFinish{job: j, runDir: dir}
	if _, err := f.tool().Execute(context.Background(), finishStepInput(t, path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("later bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	provider := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		t.Fatal("durable finish receipt requested another model call")
		return agent.Message{}, errors.New("unexpected model call")
	})
	r, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Provider: provider})
	if err != nil || r.Status != "success" {
		t.Fatalf("handoff recovery failed: %+v %v", r, err)
	}
	refs := finalEvidenceRefs(t, j, r)
	if len(refs) != 1 || refs[0].Excerpt != "original evidence\n" {
		t.Fatalf("recovery refreshed mutable evidence: %+v", refs)
	}
	other := j
	other.RunID = "different-run"
	if err := (&stepFinish{job: other, runDir: dir}).recover(context.Background()); err == nil {
		t.Fatal("finish receipt escaped its immutable run identity")
	}
}

func savedFinishPublication(t *testing.T, j Job, dir, variant string) board.FactRecord {
	t.Helper()
	ref, err := freezeEvidenceBytes(context.Background(), j.RunID, dir, []byte("original published observation\n"))
	if err != nil {
		t.Fatal(err)
	}
	fact := board.FactRecord{ID: "f001", Status: "valid", SourceStepID: j.Intent.ID, RunID: "worker@" + j.RunID, Evidence: []board.EvidenceRef{ref}}
	switch variant {
	case "other_run":
		fact.RunID = "another-run"
	case "other_step":
		fact.SourceStepID = "another-step"
	case "invalid":
		fact.Status = "superseded"
	case "legacy":
		fact.Legacy = true
	case "no_evidence":
		fact.Evidence = nil
	}
	factJSON, _ := json.Marshal(fact)
	receipt := board.StateActionResult{Op: "fact", ID: fact.ID, Revision: 1, Result: factJSON}
	if variant == "omitted_receipt" {
		receipt.Result = nil
	}
	receiptJSON, _ := json.Marshal(receipt)
	content, _ := json.Marshal(string(receiptJSON))
	name := "graph_action"
	if variant == "wrong_tool" {
		name = "bash"
	}
	history := []agent.Message{
		{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "publish", Name: name, Input: json.RawMessage(`{"op":"fact","idempotency_key":"published","payload":{}}`)}}},
		{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "publish", Content: content, IsError: variant == "failed_receipt"}}},
	}
	identity, err := identityFor(j, dir)
	if err != nil {
		t.Fatal(err)
	}
	saved := session{SchemaVersion: sessionSchemaVersion, Identity: identity, RunID: j.RunID, Kind: j.Kind, StartedAt: time.Now(), History: history}
	journal, err := openJournal(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.file.Close()
	for _, message := range history {
		if err := journal.append(agent.Event{Type: "message_end", Message: &message}); err != nil {
			t.Fatal(err)
		}
	}
	if variant == "compacted" {
		saved.History = []agent.Message{agent.Text("user", "A publication was compacted; consult original receipts.")}
	}
	if err := saved.save(dir, journal); err != nil {
		t.Fatal(err)
	}
	return fact
}

func TestFinishStepPublishedFactReusesReceiptAndRecoversWithoutPublishing(t *testing.T) {
	for _, variant := range []string{"normal", "compacted"} {
		t.Run(variant, func(t *testing.T) {
			j, dir := graphWrapperJob(t), t.TempDir()
			j.ResultContractVersion = 2
			published := savedFinishPublication(t, j, dir, variant)
			before, err := os.ReadFile(filepath.Join(dir, "session.json"))
			if err != nil {
				t.Fatal(err)
			}
			f := &stepFinish{job: j, runDir: dir}
			if _, err := f.tool().Execute(context.Background(), json.RawMessage(`{"fact_id":"f001"}`)); err != nil {
				t.Fatal(err)
			}
			// Validation must not publish a new observation or prematurely turn
			// its in-memory result projection into a terminal session.
			after, err := os.ReadFile(filepath.Join(dir, "session.json"))
			if err != nil || string(after) != string(before) {
				t.Fatal("finish validation modified its source session")
			}
			entries, err := os.ReadDir(filepath.Join(dir, "evidence"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("fact reuse generated a second evidence/publication manifest: %v %v", entries, err)
			}
			// Simulate a lost finish tool response: the old session has only
			// the first publication, while the explicit handoff is durable.
			provider := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
				t.Fatal("published-fact handoff recovery called the model")
				return agent.Message{}, errors.New("unexpected model call")
			})
			result, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Provider: provider})
			if err != nil || result.Status != "success" {
				t.Fatalf("published-fact recovery failed: %+v %v", result, err)
			}
			parsed, err := parseOutput(j, result.Conclude, result.Text)
			if err != nil || parsed.FactID != published.ID || len(parsed.FactPayload) != 0 {
				t.Fatalf("published fact was replaced or duplicated: %+v %v", parsed, err)
			}
			again, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Provider: provider})
			if err != nil || again.Text != result.Text {
				t.Fatalf("replay changed the published fact selection: %+v %v", again, err)
			}
		})
	}
}

func TestFinishStepPublishedFactRejectsInvalidBindings(t *testing.T) {
	for _, variant := range []string{"other_run", "other_step", "invalid", "legacy", "no_evidence", "omitted_receipt", "wrong_tool", "failed_receipt", "wrong_id", "tampered"} {
		t.Run(variant, func(t *testing.T) {
			j, dir := graphWrapperJob(t), t.TempDir()
			j.ResultContractVersion = 2
			fact := savedFinishPublication(t, j, dir, variant)
			id := fact.ID
			if variant == "wrong_id" {
				id = "unknown"
			}
			if variant == "tampered" {
				if err := os.Chmod(fact.Evidence[0].Path, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fact.Evidence[0].Path, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(map[string]string{"fact_id": id})
			f := &stepFinish{job: j, runDir: dir}
			if _, err := f.tool().Execute(context.Background(), raw); err == nil {
				t.Fatal("invalid published fact accepted")
			}
			if _, done := f.result(); done {
				t.Fatal("invalid published fact ended the Worker")
			}
			if _, err := os.Stat(filepath.Join(dir, "finish-step.json")); !os.IsNotExist(err) {
				t.Fatal("invalid published fact persisted a terminal handoff")
			}
		})
	}
}

func TestFinishStepSchemaAndRuntimeRequireExclusiveFactSelection(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	j.ResultContractVersion = 2
	f := &stepFinish{job: j, runDir: dir}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		OneOf      []struct {
			Required []string `json:"required"`
		} `json:"oneOf"`
	}
	if err := json.Unmarshal(f.tool().Schema, &schema); err != nil || len(schema.Properties) != 2 || len(schema.OneOf) != 2 || len(schema.OneOf[0].Required) != 1 || len(schema.OneOf[1].Required) != 1 || schema.OneOf[0].Required[0] != "fact_id" || schema.OneOf[1].Required[0] != "fact" {
		t.Fatalf("schema does not express exclusive fact selection: %+v %v", schema, err)
	}
	path := filepath.Join(j.Workspace, "output.txt")
	if err := os.WriteFile(path, []byte("observation"), 0600); err != nil {
		t.Fatal(err)
	}
	var both map[string]json.RawMessage
	if err := json.Unmarshal(finishStepInput(t, path), &both); err != nil {
		t.Fatal(err)
	}
	both["fact_id"] = json.RawMessage(`"f001"`)
	bothRaw, _ := json.Marshal(both)
	// The generic argument validator intentionally supports a small schema
	// vocabulary; parseOutput remains the authority for mutual exclusion.
	for _, raw := range []json.RawMessage{bothRaw, json.RawMessage(`{}`), json.RawMessage(`{"fact_id":null}`), json.RawMessage(`{"fact_id":""}`)} {
		if _, err := f.tool().Execute(context.Background(), raw); err == nil {
			t.Fatalf("nonexclusive/empty selection accepted: %s", raw)
		}
	}
	if _, done := f.result(); done {
		t.Fatal("invalid selection stopped the Worker")
	}
}

func TestFinishStepRejectsMissingEvidenceAndCancellationWithoutStopping(t *testing.T) {
	for _, name := range []string{"missing", "empty", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			j := graphWrapperJob(t)
			j.ResultContractVersion = 2
			dir := t.TempDir()
			path := filepath.Join(j.Workspace, "evidence.txt")
			if name != "missing" {
				if err := os.WriteFile(path, []byte{}, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "cancelled" {
				cancel()
			}
			f := &stepFinish{job: j, runDir: dir}
			if _, err := f.tool().Execute(ctx, finishStepInput(t, path)); err == nil {
				t.Fatal("invalid handoff accepted")
			}
			if _, done := f.result(); done {
				t.Fatal("invalid handoff stopped the Loop")
			}
			if _, err := os.Stat(filepath.Join(dir, "finish-step.json")); !os.IsNotExist(err) {
				t.Fatal("invalid handoff persisted a completion receipt")
			}
		})
	}
}

func TestFinishStepRemainsDisabledDuringConclusionAndRepair(t *testing.T) {
	for _, phase := range []string{"conclude", "repair"} {
		t.Run(phase, func(t *testing.T) {
			j := graphWrapperJob(t)
			j.ResultContractVersion = 2
			dir := t.TempDir()
			path := filepath.Join(j.Workspace, "result.txt")
			if err := os.WriteFile(path, []byte("evidence\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f := &stepFinish{job: j, runDir: dir}
			turn := 0
			provider := scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				turn++
				if turn == 1 {
					return agent.Message{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "finish", Name: "finish_step", Input: finishStepInput(t, path)}}}, nil
				}
				last := history[len(history)-1]
				if len(last.Content) != 1 || !last.Content[0].IsError || !strings.Contains(string(last.Content[0].Content), "disabled") {
					t.Fatalf("phase allowed finish_step: %+v", last)
				}
				return agent.Text("assistant", incompleteOutput), nil
			})
			loop := agent.Loop{Provider: provider, Tools: []agent.Tool{f.tool()}, StopResult: f.result, Concluding: phase == "conclude", Repairing: phase == "repair"}
			if _, err := loop.Run(context.Background(), "phase fixture"); err != nil {
				t.Fatal(err)
			}
			if _, done := f.result(); done {
				t.Fatal("phase restriction bypassed")
			}
		})
	}
}
