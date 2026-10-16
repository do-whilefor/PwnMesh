package board

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestExecutionWritesRollbackWithReviewEvents(t *testing.T) {
	f := newOrchestrationFixture(t)
	dispute, _, _ := f.conflict()
	step := f.step("independent review rollback", dispute.ID)
	previous := f.worker("old-review", step)
	f.do(func(tx *Tx) error {
		return tx.ExecutionStatus(previous, "failed", json.RawMessage(`{"status":"failed","error":"try again"}`))
	})
	grantMainRetry(f, "review-retry", previous)
	f.do(func(tx *Tx) error {
		_, err := tx.transactionQueries.Exec("UPDATE intents SET worker=NULL WHERE project_id='p' AND id=?", step)
		return err
	})
	next := dependencyRun(f, "new-review", step)
	var job map[string]any
	if err := json.Unmarshal(next.Job, &job); err != nil {
		t.Fatal(err)
	}
	job["previous_run_id"] = previous.ID
	next.Job, _ = json.Marshal(job)

	setEventFailure := func(enabled bool) {
		t.Helper()
		f.do(func(tx *Tx) error {
			query := "DROP TRIGGER reject_review_event"
			if enabled {
				query = `CREATE TRIGGER reject_review_event BEFORE INSERT ON pwnmesh_state_events BEGIN SELECT RAISE(ABORT,'event refused'); END`
			}
			_, err := tx.transactionQueries.Exec(query)
			return err
		})
	}
	before := f.state()
	for _, failure := range []struct{ table, message string }{
		{"pwnmesh_executions", "registration refused"},
		{"pwnmesh_state_events", "event refused"},
	} {
		f.do(func(tx *Tx) error {
			_, err := tx.transactionQueries.Exec("CREATE TRIGGER reject_registration BEFORE INSERT ON " + failure.table + " BEGIN SELECT RAISE(ABORT,'" + failure.message + "'); END")
			return err
		})
		f.do(func(tx *Tx) error {
			if err := tx.RegisterExecution(next); err == nil || !strings.Contains(err.Error(), failure.message) {
				t.Fatalf("registration did not reach injected failure: %v", err)
			}
			// Handling this operation's error must not consume its grant or
			// publish its review while unrelated work commits successfully.
			return tx.UpdateSettings(Settings{IntentTimeout: 30, ReasonTimeout: 30})
		})
		if after := f.state(); !reflect.DeepEqual(after, before) {
			t.Fatal("handled registration error retained state or revisions")
		}
		f.do(func(tx *Tx) error {
			grant, err := tx.Execution("p", previous.ID)
			if err != nil || grant.Status != "retry_requested" {
				t.Fatalf("failed registration consumed its grant: %+v, %v", grant, err)
			}
			_, err = tx.Execution("p", next.ID)
			requireAPIStatus(t, err, 404)
			settings, err := tx.Settings()
			if err != nil || settings.IntentTimeout != 30 || settings.ReasonTimeout != 30 {
				t.Fatalf("registration rolled back unrelated settings: %+v, %v", settings, err)
			}
			_, err = tx.transactionQueries.Exec("DROP TRIGGER reject_registration")
			return err
		})
	}
	f.do(func(tx *Tx) error { return tx.RegisterExecution(next) })
	before = f.state()
	setEventFailure(true)
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		err := tx.ExecutionStatus(next, "failed", json.RawMessage(`{"status":"failed","error":"review interrupted"}`))
		if err == nil || !strings.Contains(err.Error(), "event refused") {
			t.Fatalf("outcome did not reach its failure event: %v", err)
		}
		return tx.UpdateSettings(Settings{IntentTimeout: 40, ReasonTimeout: 40})
	})
	if err != nil {
		t.Fatal(err)
	}
	if after := f.state(); !reflect.DeepEqual(after, before) {
		t.Fatal("failed outcome event retained failure/review state or revisions")
	}
	f.do(func(tx *Tx) error {
		stored, err := tx.Execution("p", next.ID)
		if err != nil || stored.Status != "prepared" || len(stored.Result) != 0 {
			t.Fatalf("failed outcome event retained terminal result: %+v, %v", stored, err)
		}
		revoked, err := tx.RunRevoked("p", next.Lease)
		if err != nil || revoked {
			t.Fatalf("failed outcome event retained revocation: %v, %v", revoked, err)
		}
		events, err := tx.StateEvents("p", before.Revision)
		if err != nil || len(events) != 0 {
			t.Fatalf("failed outcome event became visible: %+v, %v", events, err)
		}
		return nil
	})
}

func TestExecutionOutcomeHandledFailureRollsBackEveryTransition(t *testing.T) {
	for _, failure := range []struct{ name, trigger, status string }{
		{"revocation", `BEFORE INSERT ON pwnmesh_revoked_runs`, "failed"},
		{"failure_event", `BEFORE INSERT ON pwnmesh_state_events WHEN json_extract(NEW.event,'$.op')='execution_failed'`, "rejected"},
		{"review_event", `BEFORE INSERT ON pwnmesh_state_events WHEN json_extract(NEW.event,'$.op')='dispute'`, "cancelled"},
	} {
		t.Run(failure.name, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			dispute, _, _ := f.conflict()
			e := f.worker("review", f.step("review outcome", dispute.ID))
			before := f.state()
			var original Execution
			f.do(func(tx *Tx) error {
				var err error
				original, err = tx.Execution("p", e.ID)
				if err != nil {
					return err
				}
				_, err = tx.transactionQueries.Exec("CREATE TRIGGER reject_outcome " + failure.trigger + " BEGIN SELECT RAISE(ABORT,'outcome unavailable'); END")
				return err
			})
			result := json.RawMessage(`{"status":"failed","error":"review interrupted"}`)
			f.do(func(tx *Tx) error {
				if err := tx.ExecutionStatus(e, failure.status, result); err == nil || !strings.Contains(err.Error(), "outcome unavailable") {
					t.Fatalf("outcome did not reach injected failure: %v", err)
				}
				return tx.UpdateSettings(Settings{IntentTimeout: 40, ReasonTimeout: 40})
			})
			if after := f.state(); !reflect.DeepEqual(before, after) {
				t.Fatal("handled outcome failure retained state or revision changes")
			}
			f.do(func(tx *Tx) error {
				stored, err := tx.Execution("p", e.ID)
				if err != nil || !reflect.DeepEqual(stored, original) {
					t.Fatalf("handled outcome failure retained execution changes: %+v, %v", stored, err)
				}
				revoked, err := tx.RunRevoked("p", e.Lease)
				if err != nil || revoked {
					t.Fatalf("handled outcome failure retained revocation: %v, %v", revoked, err)
				}
				events, err := tx.StateEvents("p", before.Revision)
				if err != nil || len(events) != 0 {
					t.Fatalf("handled outcome failure retained events: %+v, %v", events, err)
				}
				settings, err := tx.Settings()
				if err != nil || settings.IntentTimeout != 40 {
					t.Fatalf("outcome rolled back unrelated caller work: %+v, %v", settings, err)
				}
				_, err = tx.transactionQueries.Exec("DROP TRIGGER reject_outcome")
				return err
			})
			for range 2 {
				f.do(func(tx *Tx) error { return tx.ExecutionStatus(e, failure.status, result) })
			}
			after := f.state()
			if after.Revision != before.Revision+2 || after.Disputes[0].Status != "uncertain" {
				t.Fatalf("retry did not commit exactly one failure and review transition: %+v", after)
			}
		})
	}
}

func TestReviewStartDoesNotRequestDecisionButPreservesVersionAndEvents(t *testing.T) {
	for _, outcome := range []string{"failed", "rejected", "cancelled", "new_evidence"} {
		t.Run(outcome, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			dispute, _, _ := f.conflict()
			step := f.step("independent review", dispute.ID)
			before := f.state()
			review := f.worker("independent-review", step)
			started := f.state()
			if started.Revision != before.Revision+1 || started.DecisionRevision != before.DecisionRevision || started.Disputes[0].Status != "reviewing" {
				t.Fatalf("review startup changed the decision boundary: revision %d/%d decision %d/%d dispute=%s", before.Revision, started.Revision, before.DecisionRevision, started.DecisionRevision, started.Disputes[0].Status)
			}
			if DecisionStateVersion(started) == DecisionStateVersion(before) {
				t.Fatal("review startup disappeared from the strict shared-content version")
			}
			f.do(func(tx *Tx) error {
				// Removing a scheduling trigger must not authorize an old CAS.
				requireAPIStatus(t, tx.CheckDecisionStateVersion(started, f.planner, DecisionStateVersion(before)), 409)
				events, err := tx.StateEvents("p", before.Revision)
				if err != nil {
					return err
				}
				if len(events) != 1 || events[0].Op != "dispute" || events[0].ID != dispute.ID || events[0].RunID != review.Lease {
					t.Fatalf("lost the registered review startup event: %+v", events)
				}
				var payload struct {
					Status string `json:"status"`
					StepID string `json:"step_id"`
				}
				var recorded Dispute
				if json.Unmarshal(events[0].Payload, &payload) != nil || json.Unmarshal(events[0].Result, &recorded) != nil || payload.Status != "prepared" || payload.StepID != step || recorded.Status != "reviewing" {
					t.Fatal("startup event no longer describes the mechanical transition")
				}
				input, err := tx.ScheduleInput("p", 0, "")
				if err == nil && input.DecisionRevision != before.DecisionRevision {
					t.Fatal("scheduling page requested a decision for review startup")
				}
				return err
			})
			if outcome == "new_evidence" {
				f.fact(review, "new independent observation")
			} else {
				f.do(func(tx *Tx) error {
					return tx.ExecutionStatus(review, outcome, json.RawMessage(`{"status":"failed","error":"independent evidence unavailable"}`))
				})
			}
			after := f.state()
			if after.DecisionRevision <= started.DecisionRevision || DecisionStateVersion(after) == DecisionStateVersion(started) {
				t.Fatal("new evidence or failed review no longer invalidated the decision boundary")
			}
			if outcome != "new_evidence" && after.Disputes[0].Status != "uncertain" {
				t.Fatal("failed review hid the unresolved dispute")
			}
		})
	}
}

func TestReviewStartPreservesOnlyCurrentUncommittedPlanningSignal(t *testing.T) {
	for _, mode := range []string{"prepared", "running", "retryable", "result_pending", "committed", "already_stale", "failed"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			dispute, _, _ := f.conflict()
			step := f.step("authorized review", dispute.ID)
			observationStep := ""
			if mode == "already_stale" {
				observationStep = f.step("already authorized observation", "")
			}
			input := f.state()
			planner := Execution{ProjectID: "p", ID: "plan", Namespace: "test", Backend: "planner", Kind: "reason", Lease: f.planner.Run, RetryKey: DecisionRetryKey(input.Graph, input.DecisionRevision)}
			f.do(func(tx *Tx) error {
				ref, err := tx.FreezeInput(input)
				if err != nil {
					return err
				}
				planner.Job, _ = json.Marshal(map[string]any{"kind": "reason", "run_id": planner.ID, "graph": input.Graph, "input_snapshot": ref, "decision": map[string]any{"version": 2, "state_version": ref.StateVersion}, "graph_rpc": true, "result_contract_version": 2})
				if err = tx.RegisterExecution(planner); err != nil {
					return err
				}
				switch mode {
				case "running", "retryable", "result_pending":
					return tx.ExecutionStatus(planner, mode, nil)
				case "committed":
					_, err = tx.CommitDecision("p", f.planner, DecisionBatch{ExpectedVersion: ref.StateVersion, Actions: []DecisionAction{}})
					return err
				case "failed":
					return tx.ExecutionStatus(planner, "failed", json.RawMessage(`{"status":"failed","failure_kind":"invalid_result"}`))
				}
				return nil
			})
			if mode == "already_stale" {
				// A separate semantic change already supplies its own new input.
				producer := f.worker("new-observation", observationStep)
				f.fact(producer, "new planning input")
			}
			before := f.state()
			review := f.worker("review-start", step)
			after := f.state()
			increment := int64(0)
			if mode == "prepared" || mode == "running" || mode == "retryable" || mode == "result_pending" {
				increment = 1
			}
			if after.DecisionRevision != before.DecisionRevision+increment || after.Revision != before.Revision+1 || after.Disputes[0].Status != "reviewing" {
				t.Fatalf("mechanical startup lost or invented a planning signal: before=%d/%d after=%d/%d", before.Revision, before.DecisionRevision, after.Revision, after.DecisionRevision)
			}
			f.do(func(tx *Tx) error {
				requireAPIStatus(t, tx.CheckDecisionStateVersion(after, f.planner, DecisionStateVersion(input)), 409)
				if increment == 0 {
					return nil
				}
				if DecisionRetryKey(after.Graph, after.DecisionRevision) == planner.RetryKey {
					t.Fatal("invalidated live planner cannot be replaced with fresh input")
				}
				// The same registration/recovered HTTP response cannot manufacture
				// additional planning turns after the transition was recorded.
				if err := tx.RegisterExecution(review); err != nil {
					return err
				}
				unchanged, err := tx.State("p")
				if err == nil && unchanged.DecisionRevision != after.DecisionRevision {
					t.Fatal("idempotent review registration renewed planning allowance")
				}
				return err
			})
		})
	}
}
