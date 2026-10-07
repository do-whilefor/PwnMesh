package board

import (
	"encoding/json"
	"strings"
	"testing"

	"pwnmesh/internal/artifactcheck"
)

func TestRepairStepPersistenceIdentityAndInputBinding(t *testing.T) {
	f := newOrchestrationFixture(t)
	spec := &artifactcheck.Spec{Path: "/workspace/report.json", SHA256: strings.Repeat("a", 64), Rules: []artifactcheck.Rule{{Kind: "json_valid"}}}
	add := func(key string, repair *artifactcheck.Spec) StateActionResult {
		return f.action(f.planner, "step", key, map[string]any{"action": "add", "description": "Repair report", "from": []string{"origin"}, "repair": repair}, "")
	}
	first := add("first", spec)
	if add("same", spec).ID != first.ID {
		t.Fatal("identical repair was not deduplicated")
	}
	changed := *spec
	changed.SHA256 = strings.Repeat("b", 64)
	if add("changed", &changed).ID == first.ID {
		t.Fatal("changed target version was silently deduplicated")
	}
	step := dependencyStepState(t, f.state(), first.ID)
	if step.Repair == nil || step.Repair.SHA256 != spec.SHA256 {
		t.Fatalf("contract lost: %+v", step)
	}
	run := dependencyRun(f, "repair-worker", first.ID)
	f.do(func(tx *Tx) error { requireAPIStatus(t, tx.RegisterExecution(run), 409); return nil })
	var job map[string]any
	if err := json.Unmarshal(run.Job, &job); err != nil {
		t.Fatal(err)
	}
	job["repair"] = spec
	run.Job, _ = json.Marshal(job)
	f.do(func(tx *Tx) error { return tx.RegisterExecution(run) })
	job["repair"] = &changed
	run.Job, _ = json.Marshal(job)
	f.do(func(tx *Tx) error { requireAPIStatus(t, tx.CheckExecutionDependencies(run), 409); return nil })
}

func TestRepairContractCannotMutateAnExistingStep(t *testing.T) {
	f := newOrchestrationFixture(t)
	id := f.step("ordinary", "")
	raw, _ := json.Marshal(map[string]any{"action": "priority", "id": id, "priority": 1, "reason": "later", "repair": map[string]any{"path": "/workspace/a", "sha256": strings.Repeat("a", 64), "rules": []map[string]string{{"kind": "json_valid"}}}})
	f.do(func(tx *Tx) error {
		_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "invalid-mutation", Payload: raw})
		requireAPIStatus(t, err, 422)
		return nil
	})
}

func TestStaleRepairCannotRetryObsoleteInput(t *testing.T) {
	f := newOrchestrationFixture(t)
	e := f.worker("stale", "")
	f.do(func(tx *Tx) error {
		return tx.ExecutionStatus(e, "failed", json.RawMessage(`{"status":"failed","failure_kind":"repair_stale","error":"artifact changed"}`))
	})
	f.do(func(tx *Tx) error {
		_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "retry-stale", Payload: retryStepAction(e.Intent, e.ID)})
		requireAPIStatus(t, err, 409)
		return nil
	})
}
