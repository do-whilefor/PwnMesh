package board

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func retryStepAction(step, run string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"action": "retry", "id": step, "latest_run_id": run, "reason": "A fresh authorized attempt can complete the same bounded task"})
	return raw
}

func grantMainRetry(f *orchestrationFixture, key string, e Execution) StateActionResult {
	f.t.Helper()
	var result StateActionResult
	f.do(func(tx *Tx) error {
		var err error
		result, err = tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: key, Payload: retryStepAction(e.Intent, e.ID)})
		return err
	})
	return result
}

func retrySuccessor(f *orchestrationFixture, previous Execution, label string) Execution {
	f.t.Helper()
	// Simulate the dispatcher releasing the old lease only after cleanup.
	f.do(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE intents SET worker=NULL WHERE project_id=? AND id=? AND worker=?", previous.ProjectID, previous.Intent, previous.Lease)
		return err
	})
	e := dependencyRun(f, label, previous.Intent)
	var job map[string]any
	_ = json.Unmarshal(e.Job, &job)
	job["previous_run_id"] = previous.ID
	e.Job, _ = json.Marshal(job)
	f.do(func(tx *Tx) error { return tx.RegisterExecution(e) })
	return e
}

func TestMainRetryRetainsLeaseAndInputsUntilCleanupThenConsumesOnce(t *testing.T) {
	f := newOrchestrationFixture(t)
	failed := f.worker("first-attempt", "")
	fact := f.fact(failed, "partial-observation")
	failure := json.RawMessage(`{"status":"failed","error":"temporary fixture issue"}`)
	f.do(func(tx *Tx) error { return tx.ExecutionStatus(failed, "failed", failure) })
	before := f.state()
	if step := dependencyStepState(t, before, failed.Intent); step.Status != "failed" || step.LatestRunID != failed.ID {
		t.Fatalf("failed attempt not exposed: %+v", step)
	}
	first := grantMainRetry(f, "main-retry", failed)
	if first.Unchanged {
		t.Fatal("first authorization was not committed")
	}
	again := grantMainRetry(f, "main-retry", failed)
	if again.Revision != first.Revision {
		t.Fatal("lost response replay created another grant")
	}
	duplicate := grantMainRetry(f, "duplicate-suggestion", failed)
	if !duplicate.Unchanged || duplicate.Revision != first.Revision {
		t.Fatal("duplicate retry suggestion changed the plan")
	}
	f.do(func(tx *Tx) error {
		e, err := tx.Execution("p", failed.ID)
		if err != nil {
			return err
		}
		if e.Status != "retry_requested" || string(e.Job) != string(failed.Job) || string(e.Result) != string(failure) {
			t.Fatal("grant rewrote the previous attempt")
		}
		g, err := tx.Load("p")
		if err != nil {
			return err
		}
		for _, i := range g.Intents {
			if i.ID == failed.Intent && Value(i.Worker) != failed.Lease {
				t.Fatal("grant released a lease before process cleanup")
			}
		}
		requireAPIStatus(t, tx.CheckExecution(g, failed.Fence()), 409)
		// A new run cannot register while the previous lease still owns the Step.
		premature := failed
		premature.ID, premature.Lease = "premature", "worker@premature"
		var job map[string]any
		_ = json.Unmarshal(premature.Job, &job)
		job["run_id"], job["previous_run_id"] = premature.ID, failed.ID
		premature.Job, _ = json.Marshal(job)
		requireAPIStatus(t, tx.RegisterExecution(premature), 409)
		return nil
	})
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	if err := f.state().ValidateFactSources([]string{fact}, true); err != nil {
		t.Fatal("retry lost original observation")
	}
	next := retrySuccessor(f, failed, "second-attempt")
	f.do(func(tx *Tx) error {
		old, err := tx.Execution("p", failed.ID)
		if err == nil && old.Status != "retried" {
			t.Fatal("successor did not consume authorization")
		}
		return err
	})
	if step := dependencyStepState(t, f.state(), failed.Intent); step.LatestRunID != "" {
		t.Fatal("ordinary prepared run exposed a retry ID")
	}
	var job map[string]any
	_ = json.Unmarshal(next.Job, &job)
	job["run_id"] = "third-attempt"
	third := next
	third.ID, third.Lease = "third-attempt", "worker@third-attempt"
	third.Job, _ = json.Marshal(job)
	f.do(func(tx *Tx) error {
		// Even with a new claim, the consumed previous_run_id is unusable.
		_, err := tx.Exec("UPDATE intents SET worker=? WHERE project_id=? AND id=?", third.Lease, "p", failed.Intent)
		if err != nil {
			return err
		}
		requireAPIStatus(t, tx.RegisterExecution(third), 409)
		_, err = tx.Exec("UPDATE intents SET worker=? WHERE project_id=? AND id=?", next.Lease, "p", failed.Intent)
		return err
	})
	newFact := f.fact(next, "new-success")
	f.finish(next, newFact)
	if step := dependencyStepState(t, f.state(), failed.Intent); !step.SupportValid || step.LatestRunID != "" {
		t.Fatalf("new success not accepted: %+v", step)
	}
}

func TestMainRetryBatchPreviewRollbackAndNewDirectionBudget(t *testing.T) {
	f := newOrchestrationFixture(t)
	failed := f.worker("failed", "") // Consumes the planner's sole new direction.
	f.do(func(tx *Tx) error {
		return tx.ExecutionStatus(failed, "rejected", json.RawMessage(`{"status":"failed"}`))
	})
	s := f.state()
	planner := Execution{ProjectID: "p", ID: "plan", Namespace: "test", Backend: "planner", Kind: "reason", Lease: f.planner.Run, RetryKey: "reason:retry"}
	planner.Job, _ = json.Marshal(map[string]any{"kind": "reason", "run_id": planner.ID, "graph": s.Graph, "state": s, "graph_rpc": true, "result_contract_version": 2, "decision": map[string]any{"version": 2, "state_version": DecisionStateVersion(s)}, "budget": map[string]int{"max_intents": 1}})
	f.do(func(tx *Tx) error { return tx.RegisterExecution(planner) })
	batch := DecisionBatch{ExpectedVersion: DecisionStateVersion(s), Actions: []DecisionAction{{Op: "step", Payload: retryStepAction(failed.Intent, failed.ID)}}}
	f.do(func(tx *Tx) error {
		if _, err := tx.PreviewDecision("p", f.planner, batch); err != nil {
			return err
		}
		e, err := tx.Execution("p", failed.ID)
		if err != nil {
			return err
		}
		if e.Status != "rejected" {
			t.Fatal("preview granted a retry")
		}
		bad := batch
		bad.Actions = append(append([]DecisionAction{}, batch.Actions...), DecisionAction{Op: "step", Payload: json.RawMessage(`{"action":"abandon","id":"missing","reason":"force rollback"}`)})
		if _, err = tx.CommitDecision("p", f.planner, bad); err == nil {
			t.Fatal("invalid trailing action committed a retry")
		}
		e, err = tx.Execution("p", failed.ID)
		if err != nil {
			return err
		}
		if e.Status != "rejected" {
			t.Fatal("failed batch retained the earlier retry grant")
		}
		receipt, err := tx.CommitDecision("p", f.planner, batch)
		if err != nil {
			return err
		}
		if !receipt.Committed || receipt.ChangedActions != 1 {
			t.Fatal("retry was counted as a new direction or lost")
		}
		replay, err := tx.CommitDecision("p", f.planner, batch)
		if err == nil && replay.StateVersion != receipt.StateVersion {
			t.Fatal("batch receipt replay changed grant")
		}
		return err
	})
}

func TestMainRetryRequiresCurrentTerminalRunAndValidPrerequisites(t *testing.T) {
	for _, mode := range []string{"running", "result_pending", "succeeded", "abandoned", "wrong-run", "wrong-generation", "invalid-source", "closed-goal"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			upstream := f.worker("upstream", "")
			input := f.fact(upstream, "upstream-result")
			f.finish(upstream, input)
			step := dependentStep(f, "bounded-work", upstream.Intent)
			e := dependencyRun(f, "attempt", step)
			f.do(func(tx *Tx) error { return tx.RegisterExecution(e) })
			run := e.ID
			switch mode {
			case "running":
				f.do(func(tx *Tx) error { return tx.ExecutionStatus(e, "running", nil) })
			case "result_pending":
				f.do(func(tx *Tx) error {
					return tx.ExecutionStatus(e, "result_pending", json.RawMessage(`{"status":"success"}`))
				})
			case "succeeded":
				result := f.fact(e, "result")
				f.finish(e, result)
			default:
				f.do(func(tx *Tx) error {
					return tx.ExecutionStatus(e, "failed", json.RawMessage(`{"status":"failed","error":"fixture failure"}`))
				})
				if mode == "abandoned" {
					f.action(f.planner, "step", "abandon", map[string]any{"action": "abandon", "id": step, "reason": "Task no longer authorized"}, "")
				}
				if mode == "wrong-run" {
					run = "unrelated-run"
				}
				if mode == "wrong-generation" {
					f.do(func(tx *Tx) error {
						_, err := tx.Exec("UPDATE xloom_executions SET generation=generation+1 WHERE project_id=? AND id=?", "p", e.ID)
						return err
					})
				}
				if mode == "invalid-source" {
					invalidateDependencyFact(f, input)
				}
				if mode == "closed-goal" {
					f.do(func(tx *Tx) error {
						d, _, _, err := tx.stateData("p")
						if err != nil {
							return err
						}
						d.Goals = append(d.Goals, Goal{ID: "withdrawn", Status: "withdrawn"})
						for n := range d.Steps {
							if d.Steps[n].ID == step {
								d.Steps[n].GoalID = "withdrawn"
							}
						}
						raw, _ := json.Marshal(d)
						_, err = tx.Exec("UPDATE xloom_state SET data=? WHERE project_id=?", string(raw), "p")
						return err
					})
				}
			}
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "retry", Payload: retryStepAction(step, run)})
				return err
			})
			requireAPIStatus(t, err, 409)
		})
	}
}

func TestMainRetryRejectsRoleAndTaskInputChanges(t *testing.T) {
	f := newOrchestrationFixture(t)
	failed := f.worker("failed", "")
	f.do(func(tx *Tx) error {
		return tx.ExecutionStatus(failed, "cancelled", json.RawMessage(`{"status":"failed"}`))
	})
	worker := f.worker("unrelated-active", "")
	curator, snapshot := f.curator("curator")
	for _, fence := range []ExecutionFence{worker.Fence(), curator.Fence()} {
		err := f.store.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.StateAction("p", fence, StateAction{Op: "step", IdempotencyKey: "forbidden", Payload: retryStepAction(failed.Intent, failed.ID), ExpectedVersion: DecisionStateVersion(snapshot)})
			return err
		})
		requireAPIStatus(t, err, 403)
	}
	for _, field := range []string{"from", "depends_on", "goal_id", "priority", "description", "dispute_id"} {
		var payload map[string]any
		_ = json.Unmarshal(retryStepAction(failed.Intent, failed.ID), &payload)
		payload[field] = "changed"
		if field == "from" || field == "depends_on" {
			payload[field] = []string{"origin"}
		}
		if field == "priority" {
			payload[field] = 9
		}
		raw, _ := json.Marshal(payload)
		err := f.store.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "bad-field", Payload: raw})
			return err
		})
		requireAPIStatus(t, err, 422)
	}
	legacy := newPlanFixture(t)
	err := legacy.store.Do(context.Background(), func(tx *Tx) error {
		s, err := tx.State("proj_001")
		if err != nil {
			return err
		}
		d, _, _, err := tx.stateData("proj_001")
		if err != nil {
			return err
		}
		_, _, _, err = tx.retryStep(s, &d, "step", "run", "reason")
		return err
	})
	requireAPIStatus(t, err, 422)
}

func TestRetryRunIdentityChangesVersionButOrdinaryStartDoesNot(t *testing.T) {
	f := newOrchestrationFixture(t)
	step := f.step("bounded-work", "")
	before := f.state()
	first := f.worker("first", step)
	if DecisionStateVersion(before) != DecisionStateVersion(f.state()) {
		t.Fatal("ordinary execution registration changed the semantic version")
	}
	f.do(func(tx *Tx) error {
		return tx.ExecutionStatus(first, "failed", json.RawMessage(`{"status":"failed","error":"identical failure"}`))
	})
	failed := f.state()
	grantMainRetry(f, "first-retry", first)
	authorized := f.state()
	second := retrySuccessor(f, first, "second")
	if DecisionStateVersion(authorized) == DecisionStateVersion(f.state()) {
		t.Fatal("consuming an explicit retry grant was hidden from the planning version")
	}
	f.do(func(tx *Tx) error {
		return tx.ExecutionStatus(second, "failed", json.RawMessage(`{"status":"failed","error":"identical failure"}`))
	})
	current := f.state()
	if DecisionStateVersion(failed) == DecisionStateVersion(current) {
		t.Fatal("different failed attempts have interchangeable decision versions")
	}
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "stale-run", Payload: retryStepAction(step, first.ID)})
		return err
	})
	requireAPIStatus(t, err, 409)
	raw, err := ContextView(current, "", 4096)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Overview *contextOverview `json:"overview"`
	}
	if err = json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, section := range view.Overview.Sections {
		for _, node := range section.Items {
			if section.Section == "steps" && node.ID == step {
				found = node.LatestRunID == second.ID
			}
		}
	}
	if !found {
		t.Fatalf("bounded retry overview lost latest run identity: %s", raw)
	}
}

func TestMainRetryCannotBypassIndependentReviewLimit(t *testing.T) {
	f := newOrchestrationFixture(t)
	dispute, _, _ := f.conflict()
	step := f.step("independent review", dispute.ID)
	first := f.worker("first-review", step)
	f.do(func(tx *Tx) error { return tx.ExecutionStatus(first, "failed", json.RawMessage(`{"status":"failed"}`)) })
	grantMainRetry(f, "retry-review", first)
	if f.state().Disputes[0].Status != "pending_review" {
		t.Fatal("authorized review retry did not expose its waiting status")
	}
	second := retrySuccessor(f, first, "second-review")
	f.do(func(tx *Tx) error {
		return tx.ExecutionStatus(second, "failed", json.RawMessage(`{"status":"failed"}`))
	})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "third-review", Payload: retryStepAction(step, second.ID)})
		return err
	})
	requireAPIStatus(t, err, 409)
	if !strings.Contains(err.Error(), "review execution limit") {
		t.Fatalf("wrong review limit rejection: %v", err)
	}
	if f.state().Disputes[0].Status != "uncertain" {
		t.Fatal("exhausted review hid uncertainty")
	}
}

func TestMainRetryWaitsForOtherPendingAttemptAcrossNamespaces(t *testing.T) {
	f := newOrchestrationFixture(t)
	old := f.worker("older-attempt", "")
	f.do(func(tx *Tx) error { return tx.ExecutionStatus(old, "failed", json.RawMessage(`{"status":"failed"}`)) })
	grantMainRetry(f, "authorized-first", old)
	next := retrySuccessor(f, old, "latest-attempt")
	f.do(func(tx *Tx) error { return tx.ExecutionStatus(next, "failed", json.RawMessage(`{"status":"failed"}`)) })
	// Reproduce pre-existing overlapping data left by an older dispatcher or
	// manual lease release. A new grant must fail closed across namespaces.
	f.do(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE xloom_executions SET status='running',namespace='other-dispatcher' WHERE project_id=? AND id=?", "p", old.ID)
		return err
	})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "overlap", Payload: retryStepAction(next.Intent, next.ID)})
		return err
	})
	requireAPIStatus(t, err, 409)
	if !strings.Contains(err.Error(), "still pending") {
		t.Fatalf("wrong overlap rejection: %v", err)
	}
}
