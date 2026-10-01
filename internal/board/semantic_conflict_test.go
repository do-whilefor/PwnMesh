package board

import (
	"context"
	"reflect"
	"testing"
)

// Different claim strings must not be silently merged, but an explicit request
// about their original evidence must still prevent premature acceptance.
func TestDifferentlyWordedConflictBlocksDependentAcceptance(t *testing.T) {
	f := newOrchestrationFixture(t)
	a, b := f.worker("authenticated", ""), f.worker("anonymous", "")
	fa, fb := f.fact(a, "authentication-required"), f.fact(b, "anonymous-access-allowed")
	for _, item := range []struct {
		run         Execution
		fact, claim string
	}{{a, fa, "The endpoint requires authentication"}, {b, fb, "Anonymous requests can access the endpoint"}} {
		f.action(item.run.Fence(), "candidate", "candidate:"+item.run.ID, map[string]any{
			"claim": item.claim, "scope": "same endpoint and observation window", "status": "verified",
			"sources": []string{item.fact}, "reason": "Interpretation to reconcile against the other original response",
		}, "")
	}
	f.finish(a, fa)
	f.finish(b, fb)
	before := f.state()
	if len(before.Disputes) != 0 || before.CandidateView(before.Candidates[0]).GroupKey == before.CandidateView(before.Candidates[1]).GroupKey {
		t.Fatal("different claims were automatically treated as an identical proposition")
	}
	f.action(f.planner, "curation_request", "compare-originals", map[string]any{
		"sources": []string{fa, fb}, "reason": "The same endpoint and observation window have opposite authentication conclusions; compare both raw responses before using either as proof",
	}, DecisionStateVersion(before))
	requested := f.state()
	for _, fact := range []string{fa, fb} {
		err := f.store.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.CompleteProject("p", f.planner, []string{fact}, "One successful subtask proves completion")
			return err
		})
		requireAPIStatus(t, err, 409)
	}
	if !reflect.DeepEqual(f.state(), requested) {
		t.Fatal("rejected completion consumed unresolved evidence or altered the task")
	}
	curator, input := f.curator("compare-evidence")
	f.action(curator.Fence(), "curate", "invalidate-unsupported-account", CuratePayload{
		ThroughRevision: input.Revision, Groups: []CurateGroup{},
		Relations: []CurateRelation{{Kind: "refutes", Source: fb, Target: fa, Reason: "The retained anonymous response contradicts the claimed authentication requirement for this exact endpoint and window"}},
	}, DecisionStateVersion(input))
	f.finish(curator, "")
	final := f.state()
	if final.PendingCurationRequest() != nil || final.ValidateFactSources([]string{fa}, true) == nil || final.ValidateFactSources([]string{fb}, true) != nil {
		t.Fatal("explicit evidence reconciliation did not invalidate only the contradicted support")
	}
	if !reflect.DeepEqual(final.Candidates, before.Candidates) {
		t.Fatal("reconciliation rewrote the producers' original claims")
	}
}
