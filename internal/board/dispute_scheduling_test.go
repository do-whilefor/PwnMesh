package board

import (
	"encoding/json"
	"testing"
)

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
	for _, mode := range []string{"prepared", "running", "committed", "already_stale", "failed"} {
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
				case "running":
					return tx.ExecutionStatus(planner, "running", nil)
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
			if mode == "prepared" || mode == "running" {
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
