package board

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func registerProgressDecision(t *testing.T, f *orchestrationFixture) (State, Execution) {
	t.Helper()
	state := f.state()
	e := Execution{ProjectID: "p", ID: "plan", Namespace: "test", Backend: "planner", Kind: "reason", Lease: f.planner.Run, RetryKey: DecisionRetryKey(state.Graph, state.DecisionRevision)}
	e.Job, _ = json.Marshal(map[string]any{"kind": "reason", "run_id": e.ID, "graph": state.Graph, "state": state,
		"graph_rpc": true, "result_contract_version": 2, "decision": map[string]any{"version": 2, "state_version": DecisionStateVersion(state)}, "budget": map[string]int{"max_intents": 8}})
	f.do(func(tx *Tx) error { return tx.RegisterExecution(e) })
	return state, e
}

func TestDecisionBatchRequiresProgressAfterNonemptyActions(t *testing.T) {
	for _, mode := range []string{"goal_only", "abandon_last", "blocked_only", "abandoned_only", "unchanged_failed", "existing_work", "unchanged_existing", "queued_pipeline"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			actions := []DecisionAction{{Op: "goal", Payload: json.RawMessage(`{"action":"add","condition":"Investigate the target"}`)}}
			switch mode {
			case "abandon_last", "abandoned_only":
				id := f.step("Existing work", "")
				payload := map[string]any{"action": "abandon", "id": id, "reason": "Direction ruled out"}
				if mode == "abandoned_only" {
					f.action(f.planner, "step", "abandon", payload, "")
				} else {
					raw, _ := json.Marshal(payload)
					actions = []DecisionAction{{Op: "step", Payload: raw}}
				}
			case "blocked_only", "unchanged_failed":
				failed := f.worker("Existing work", "")
				f.do(func(tx *Tx) error {
					return tx.ExecutionStatus(failed, "failed", json.RawMessage(`{"status":"failed","error":"fixture failure"}`))
				})
				if mode == "blocked_only" {
					dependentStep(f, "Waiting for failed prerequisite", failed.Intent)
				} else {
					actions = []DecisionAction{{Op: "step", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"Existing work"}`)}}
				}
			case "existing_work", "unchanged_existing", "queued_pipeline":
				id := f.step("Existing work", "")
				if mode == "unchanged_existing" {
					actions = []DecisionAction{{Op: "step", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"Existing work"}`)}}
				}
				if mode == "queued_pipeline" {
					dependentStep(f, "Waiting for executable prerequisite", id)
				}
			}
			before, execution := registerProgressDecision(t, f)
			batch := DecisionBatch{ExpectedVersion: DecisionStateVersion(before), Actions: actions}
			allowed := mode == "existing_work" || mode == "unchanged_existing" || mode == "queued_pipeline"
			f.do(func(tx *Tx) error {
				var writes int
				if err := tx.QueryRow("SELECT (SELECT COUNT(*) FROM xloom_state_actions)+(SELECT COUNT(*) FROM xloom_state_events)").Scan(&writes); err != nil {
					return err
				}
				for _, commit := range []bool{false, true} {
					receipt, err := tx.decisionBatch("p", f.planner, batch, commit)
					if allowed {
						if err != nil || receipt.Committed != commit {
							t.Fatalf("existing viable work rejected: commit=%t receipt=%+v err=%v", commit, receipt, err)
						}
						continue
					}
					requireAPIStatus(t, err, 422)
					current, err := tx.State("p")
					if err != nil {
						return err
					}
					saved, err := tx.Execution("p", execution.ID)
					if err != nil {
						return err
					}
					var afterWrites int
					if err := tx.QueryRow("SELECT (SELECT COUNT(*) FROM xloom_state_actions)+(SELECT COUNT(*) FROM xloom_state_events)").Scan(&afterWrites); err != nil {
						return err
					}
					if receipt.Committed || !reflect.DeepEqual(current, before) || saved.Status != "prepared" || afterWrites != writes {
						t.Fatal("idle decision retained a mutation, event, receipt or successful execution")
					}
				}
				if !allowed {
					batch.Actions = []DecisionAction{{Op: "step", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"A viable replacement direction"}`)}}
					if _, err := tx.CommitDecision("p", f.planner, batch); err != nil {
						return err
					}
				}
				return nil
			})
		})
	}
}

func TestDecisionCanYieldOnlyToActionableCuration(t *testing.T) {
	for _, mode := range []string{"ready", "pending", "failed", "retry_granted", "automatic_retry"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("observation", "")
			fact := f.fact(producer, "retained observation")
			f.finish(producer, fact)
			f.action(f.planner, "curation_request", "request", map[string]any{"sources": []string{fact}, "reason": "Reconcile the retained observation"}, "")
			if mode != "ready" {
				curator, _ := f.curator("curation-attempt")
				if mode != "pending" {
					f.do(func(tx *Tx) error {
						failure := json.RawMessage(`{"status":"failed","failure_kind":"invalid_output"}`)
						if mode == "automatic_retry" {
							failure = json.RawMessage(`{"status":"failed","failure_kind":"unavailable"}`)
						}
						if err := tx.ExecutionStatus(curator, "failed", failure); err != nil {
							return err
						}
						if err := tx.ReleaseCurator("p", curator.Lease); err != nil {
							return err
						}
						if mode == "retry_granted" {
							return tx.ExecutionStatus(curator, "retry_requested", nil)
						}
						return nil
					})
				}
			}
			before, _ := registerProgressDecision(t, f)
			f.do(func(tx *Tx) error {
				batch := DecisionBatch{ExpectedVersion: DecisionStateVersion(before)}
				for _, commit := range []bool{false, true} {
					receipt, err := tx.decisionBatch("p", f.planner, batch, commit)
					if mode == "failed" {
						requireAPIStatus(t, err, 422)
					} else if err != nil || receipt.Committed != commit {
						t.Fatalf("actionable curation rejected: mode=%s commit=%t receipt=%+v err=%v", mode, commit, receipt, err)
					}
				}
				return nil
			})
		})
	}
}

func TestDecisionPreservesRegisteredObservationAfterPremiseCorrection(t *testing.T) {
	for _, mode := range []string{"running", "result_pending", "prepared", "retryable", "unregistered", "needs_review", "no_worker", "released_result_pending", "revoked", "dependency_invalidated"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("premise", "")
			source, correction := f.fact(producer, "old premise"), f.fact(producer, "corrected premise")
			f.finish(producer, source)
			payload := map[string]any{"action": "add", "from": []string{source}, "description": "Retain independent response observations"}
			if mode == "dependency_invalidated" {
				payload["depends_on"] = []string{producer.Intent}
			}
			id := f.action(f.planner, "step", "consumer", payload, "").ID
			var run Execution
			if mode != "needs_review" {
				run = dependencyRun(f, "consumer", id)
				if mode != "unregistered" {
					f.do(func(tx *Tx) error {
						if err := tx.RegisterExecution(run); err != nil {
							return err
						}
						if mode == "prepared" {
							return nil
						}
						if err := tx.ExecutionStatus(run, "running", nil); err != nil {
							return err
						}
						if mode == "result_pending" || mode == "released_result_pending" {
							return tx.ExecutionStatus(run, "result_pending", json.RawMessage(`{"status":"success"}`))
						}
						if mode == "retryable" {
							return tx.ExecutionStatus(run, "retryable", nil)
						}
						return nil
					})
				}
			}
			curator, input := f.curator("correction")
			f.action(curator.Fence(), "curate", "correct", CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{{Kind: "refutes", Source: correction, Target: source, Reason: "Fresh retained observation corrects the premise"}}}, DecisionStateVersion(input))
			f.finish(curator, "")
			assertCurationNeeded(t, f, false)
			if mode == "no_worker" || mode == "released_result_pending" || mode == "revoked" {
				f.do(func(tx *Tx) error {
					if mode != "revoked" {
						_, err := tx.Exec("UPDATE intents SET worker=NULL WHERE project_id='p' AND id=?", id)
						return err
					}
					_, err := tx.Exec("INSERT INTO xloom_revoked_runs(project_id,worker) VALUES('p',?)", run.Lease)
					return err
				})
			}
			before, _ := registerProgressDecision(t, f)
			allowed := mode == "running" || mode == "result_pending"
			f.do(func(tx *Tx) error {
				if mode == "released_result_pending" {
					step := dependencyStepState(t, before, id)
					if step.Worker != nil || step.Status != "needs_review" {
						t.Fatalf("pending delivery did not release its invalidated Step: %+v", step)
					}
					pending, err := tx.Execution("p", run.ID)
					if err != nil {
						return err
					}
					// Pending delivery skips the restart check, but its immutable
					// prerequisite check still rejects this corrected From premise.
					err = tx.ResumeExecution(run)
					requireAPIStatus(t, err, 409)
					if !strings.Contains(err.Error(), "dependency_invalidated") {
						t.Fatalf("pending delivery failed for an unrelated reason: %v", err)
					}
					after, err := tx.Execution("p", run.ID)
					if err != nil {
						return err
					}
					if pending.Status != "result_pending" || !reflect.DeepEqual(after, pending) {
						t.Fatal("rejected delivery recovery changed its status, result or allowance")
					}
				}
				batch := DecisionBatch{ExpectedVersion: DecisionStateVersion(before), Actions: []DecisionAction{{Op: "goal", Payload: json.RawMessage(`{"action":"add","condition":"Assess the independent observation"}`)}}}
				for _, commit := range []bool{false, true} {
					receipt, err := tx.decisionBatch("p", f.planner, batch, commit)
					if allowed {
						if err != nil || receipt.Committed != commit {
							t.Fatalf("live observation was treated as idle: commit=%t receipt=%+v err=%v", commit, receipt, err)
						}
					} else {
						requireAPIStatus(t, err, 422)
						current, err := tx.State("p")
						if err != nil {
							return err
						}
						if receipt.Committed || !reflect.DeepEqual(current, before) {
							t.Fatal("an unavailable observation accepted a plan or retained its mutation")
						}
					}
				}
				return nil
			})
			if allowed {
				f.do(func(tx *Tx) error { return tx.StepHeartbeatReady("p", id, run.Lease) })
				fact := f.fact(run, "independent response")
				if err := f.state().ValidateFactSources([]string{fact}, true); err != nil {
					t.Fatalf("independent observation did not survive the plan: %v", err)
				}
			}
		})
	}
}
