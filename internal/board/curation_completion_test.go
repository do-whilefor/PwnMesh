package board

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCurationProducerStatusDoesNotRequireAnotherGenerationToComplete(t *testing.T) {
	for _, status := range []string{"candidate", "verified", "refuted"} {
		t.Run(status, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			fact := f.fact(producer, "observation")
			candidate := f.candidate(producer, fact, status)
			assertCurationNeeded(t, f, false)
			f.finish(producer, fact)
			assertCurationNeeded(t, f, false)
			f.do(func(tx *Tx) error {
				return tx.ValidateStateCompletion("p", []string{fact})
			})
			curator, input := f.curator("curator")
			f.curate(curator, input, CurateGroup{CandidateIDs: []string{candidate}, Status: status, Reason: "Retain the producer's supported judgment without inventing confidence"})
			f.do(func(tx *Tx) error {
				requireAPIStatus(t, tx.ValidateStateCompletion("p", []string{fact}), 409)
				return nil
			})
			f.finish(curator, "")
			state := f.state()
			if state.Graph.Project.Generation != 0 || state.Candidates[0].Generation != 0 || state.Findings[0].Status != status || len(state.Disputes) != 0 {
				t.Fatal("curation changed the project generation or producer confidence")
			}
			job, _ := json.Marshal(map[string]any{"kind": "reason", "run_id": "plan", "graph": state.Graph, "state": state, "graph_rpc": true, "result_contract_version": 2, "decision": map[string]any{"version": 2, "state_version": DecisionStateVersion(state)}})
			f.do(func(tx *Tx) error {
				return tx.RegisterExecution(Execution{ProjectID: "p", ID: "plan", Namespace: "test", Backend: "planner", Kind: "reason", Lease: f.planner.Run, Job: job, RetryKey: "reason:complete"})
			})
			payload, _ := json.Marshal(map[string]any{"from": []string{fact}, "description": "Retained observations cover the proposed proof, preserving uncertainty"})
			batch := DecisionBatch{ExpectedVersion: DecisionStateVersion(state), Actions: []DecisionAction{{Op: "complete", Payload: payload}}}
			before, _ := json.Marshal(f.state())
			f.do(func(tx *Tx) error {
				preview, err := tx.PreviewDecision("p", f.planner, batch)
				if err == nil && (preview.Committed || preview.CompletionReview == nil || preview.CompletionReview.Acceptance != "not_checked") {
					t.Fatal("protocol preview bypassed independent completion review")
				}
				return err
			})
			after, _ := json.Marshal(f.state())
			if string(before) != string(after) {
				t.Fatal("completion preview changed the board")
			}
			f.do(func(tx *Tx) error {
				receipt, err := tx.CommitDecision("p", f.planner, batch)
				if err == nil && (!receipt.Committed || !receipt.Completed) {
					t.Fatal("supported completion did not commit")
				}
				return err
			})
			if final := f.state(); final.Graph.Project.Status != "completed" || final.Findings[0].Status != status || final.Graph.Project.Generation != 0 {
				t.Fatal("completion required or caused a confidence/generation promotion")
			}
		})
	}
}

func TestCurationReviewFieldsRequireExistingDisputeAtomically(t *testing.T) {
	for _, fields := range []string{"review_fact_ids", "resolution", "both"} {
		t.Run(fields, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			fact := f.fact(producer, "observation")
			candidate := f.candidate(producer, fact, "verified")
			f.finish(producer, fact)
			curator, input := f.curator("curator")
			group := CurateGroup{CandidateIDs: []string{candidate}, Status: "verified", Reason: "Ordinary corroboration is not dispute resolution"}
			if fields != "resolution" {
				group.ReviewFactIDs = []string{fact}
			}
			if fields != "review_fact_ids" {
				group.Resolution = "resolved"
			}
			before, _ := json.Marshal(f.state())
			payload, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{group}})
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "invalid-review", Payload: payload, ExpectedVersion: DecisionStateVersion(input)})
				return err
			})
			requireAPIStatus(t, err, 422)
			after, _ := json.Marshal(f.state())
			if string(before) != string(after) {
				t.Fatal("invalid review changed findings, disputes, facts or curation cursor")
			}
		})
	}
}
