package board

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStateProjectsLatestStepRuntimeWithoutArchivedInputs(t *testing.T) {
	f := newPlanFixture(t)
	step := f.action("step", "step", stepInput("Check fixture", []string{"origin"}, "goal", 0)).ID
	f.tx(func(tx *Tx) error {
		for _, e := range []Execution{
			{ID: "old", Kind: "explore", Status: "failed", Result: json.RawMessage(`{"error":"obsolete failure"}`)},
			{ID: "latest", Kind: "explore", Status: "rejected", Result: json.RawMessage(`{"text":"{\"reason\":\"fixture declined\"}"}`)},
			{ID: "planner", Kind: "reason", Status: "failed", Result: json.RawMessage(`{"error":"unrelated planner"}`)},
		} {
			e.ProjectID, e.Intent, e.Job = "proj_001", step, json.RawMessage("archived, intentionally not JSON")
			putQueryExecution(t, tx, e, 0, "")
		}
		return nil
	})
	state := f.state()
	if len(state.Steps) != 1 || state.Steps[0].Status != "failed" || state.Steps[0].Reason != "execution rejected" {
		t.Fatalf("wrong latest execution projection: %+v", state.Steps)
	}
	f.tx(func(tx *Tx) error {
		putQueryExecution(t, tx, Execution{ProjectID: "proj_001", ID: "retry", Kind: "explore", Intent: step, Status: "running", Job: json.RawMessage("archived")}, 0, "")
		return nil
	})
	if step := f.state().Steps[0]; step.Status == "failed" || step.Reason != "" {
		t.Fatalf("old failure leaked into the retry: %+v", step)
	}
}

func TestStateFailureUsesEventsWithoutReadingRetainedResults(t *testing.T) {
	f := newPlanFixture(t)
	step := f.action("step", "step", stepInput("Check fixture", []string{"origin"}, "goal", 0)).ID
	f.tx(func(tx *Tx) error {
		result, _ := json.Marshal(map[string]string{"error": strings.Repeat("证据", 4000), "failure_kind": "request_timeout", "text": strings.Repeat("unused", 1<<18)})
		putQueryExecution(t, tx, Execution{ProjectID: "proj_001", ID: "failure", Kind: "explore", Intent: step, Status: "failed", Result: result}, 0, "")
		return nil
	})
	if reason := f.state().Steps[0].Reason; reason != "execution failed" {
		t.Fatalf("missing event read a retained result: %q", reason)
	}
	f.tx(func(tx *Tx) error {
		_, err := tx.transactionQueries.Exec(`UPDATE pwnmesh_executions SET result='unavailable' WHERE id='failure'`)
		return err
	})
	if reason := f.state().Steps[0].Reason; reason != "execution failed" {
		t.Fatalf("missing event should use status: %q", reason)
	}
	f.tx(func(tx *Tx) error {
		event, _ := json.Marshal(StateEvent{Revision: 2, Op: "execution_failed", ID: step, RunID: "planner@failure", Payload: json.RawMessage(`{"reason":"recorded graph failure"}`)})
		_, err := tx.transactionQueries.Exec(`INSERT INTO pwnmesh_state_events(project_id,revision,event) VALUES('proj_001',2,?)`, string(event))
		return err
	})
	if reason := f.state().Steps[0].Reason; reason != "recorded graph failure" {
		t.Fatalf("graph event did not remain authoritative: %q", reason)
	}
}
