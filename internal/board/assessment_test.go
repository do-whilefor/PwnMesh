package board

import (
	"context"
	"encoding/json"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestAssessmentCoordinatesOpposingProducerJudgments(t *testing.T) {
	f := newOrchestrationFixture(t)
	assessment := &model.Assessment{Claim: "Anonymous requests can read the record", Scope: "GET /record, same observation window"}
	var facts []string
	for _, status := range []string{"verified", "refuted"} {
		step := f.action(f.planner, "step", status, map[string]any{"action": "add", "from": []string{"origin"}, "description": "Independently check " + status, "assessment": assessment}, "").ID
		run := f.worker(status, step)
		fact := f.fact(run, status+"-response")
		facts = append(facts, fact)
		f.action(run.Fence(), "candidate", status+"-judgment", map[string]any{"claim": assessment.Claim, "scope": assessment.Scope, "status": status, "sources": []string{fact}, "reason": "Retained response supports this judgment"}, "")
		f.finish(run, fact)
	}
	state := f.state()
	if len(state.Candidates) != 2 || len(state.PendingCurationCandidateIDs()) != 2 || state.CandidateView(state.Candidates[0]).GroupKey != state.CandidateView(state.Candidates[1]).GroupKey {
		t.Fatal("coordinated opposing judgments did not enter the same reconciliation group")
	}
	if err := state.ValidateCompletionDisputes(facts); err == nil {
		t.Fatal("uncurated opposing judgments accepted as completion evidence")
	}
	for _, step := range state.Steps {
		if step.Assessment == nil || *step.Assessment != *assessment {
			t.Fatal("immutable assessment lost in stored Step projection")
		}
	}
	var snapshot *InputSnapshot
	f.do(func(tx *Tx) (err error) { snapshot, err = tx.FreezeInput(state); return err })
	reopenAssetFixture(t, f)
	checkRecoveredAssetSnapshot(t, f, snapshot, state)
}

func TestAssessmentRejectsRewordingScopeDriftAndMutation(t *testing.T) {
	f := newOrchestrationFixture(t)
	assessment := &model.Assessment{Claim: "Anonymous requests can read the record", Scope: "identity A"}
	step := f.action(f.planner, "step", "check", map[string]any{"action": "add", "from": []string{"origin"}, "description": "Check anonymous access", "assessment": assessment}, "").ID
	run := f.worker("producer", step)
	fact := f.fact(run, "response")
	for _, tc := range []struct{ claim, scope string }{{"Authentication is required", assessment.Scope}, {assessment.Claim, "identity B"}} {
		raw, _ := json.Marshal(map[string]any{"claim": tc.claim, "scope": tc.scope, "status": "verified", "sources": []string{fact}, "reason": "Original response"})
		err := f.store.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.StateAction("p", run.Fence(), StateAction{Op: "candidate", IdempotencyKey: "mismatch", Payload: raw})
			return err
		})
		requireAPIStatus(t, err, 422)
	}
	raw, _ := json.Marshal(map[string]any{"action": "priority", "id": step, "reason": "Reassign", "assessment": assessment})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "mutate", Payload: raw})
		return err
	})
	requireAPIStatus(t, err, 422)
	if len(f.state().Candidates) != 0 {
		t.Fatal("rejected judgments leaked into the blackboard")
	}
	if model.FindingIdentity(assessment.Claim, assessment.Scope) == model.FindingIdentity(assessment.Claim, "identity B") {
		t.Fatal("different assessment scopes were merged")
	}
}
