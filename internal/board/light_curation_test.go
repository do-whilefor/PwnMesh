package board

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func assertCurationNeeded(t *testing.T, f *orchestrationFixture, want bool) {
	t.Helper()
	f.do(func(tx *Tx) error {
		got, err := tx.CurationNeeded("p")
		if err == nil && got != want {
			t.Fatalf("curation needed=%v, want %v", got, want)
		}
		return err
	})
}

func TestOrdinaryExplorationKeepsNotesWithoutMandatoryCuration(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	fact := f.fact(producer, "original-bytes")
	assertCurationNeeded(t, f, false)
	note := f.action(producer.Fence(), "candidate", "note", map[string]any{
		"claim": "An interpretation worth checking", "scope": "fixture", "sources": []string{fact}, "reason": "The original observation suggests this possibility",
	}, "").ID
	s := f.state()
	if len(s.Findings) != 0 || len(s.Candidates) != 1 || s.Candidates[0].ID != note || s.Candidates[0].Status != "candidate" || !reflect.DeepEqual(s.Candidates[0].Sources, []string{fact}) {
		t.Fatal("tentative note was forced into a shared conclusion or lost its original source")
	}
	assertCurationNeeded(t, f, false)
	f.finish(producer, fact)
	assertCurationNeeded(t, f, false)
	other := f.worker("unrelated-producer", "")
	otherFact := f.fact(other, "another-observation")
	f.action(other.Fence(), "candidate", "unrelated", map[string]any{
		"claim": "An unrelated interpretation", "scope": "fixture", "sources": []string{otherFact}, "reason": "This does not share a claim with the first note",
	}, "")
	f.finish(other, otherFact)
	assertCurationNeeded(t, f, false)
}

func TestCurationRequiresOnlyReconciliationGroupsAndRetainsOlderNotes(t *testing.T) {
	f := newOrchestrationFixture(t)
	first := f.worker("first", "")
	firstFact := f.fact(first, "first")
	firstNote := f.candidate(first, firstFact, "candidate")
	unrelated := f.action(first.Fence(), "candidate", "unrelated", map[string]any{
		"claim": "Separate hypothesis", "scope": "fixture", "reason": "Keep this optional exploration note",
	}, "").ID
	// Optional legacy/manual scans can acknowledge an input without promoting
	// its ordinary notes. A future matching producer must still see them.
	curator, input := f.curator("optional-scan")
	f.curate(curator, input)
	f.finish(curator, "")
	assertCurationNeeded(t, f, false)
	second := f.worker("second", "")
	secondNote := f.candidate(second, f.fact(second, "second"), "candidate")
	assertCurationNeeded(t, f, true)
	curator, input = f.curator("merge")
	relevant := input.PendingCurationCandidateIDs()
	if !relevant[firstNote] || !relevant[secondNote] || relevant[unrelated] || len(relevant) != 2 {
		t.Fatal("reconciliation missed an older active note or forced in an unrelated hypothesis")
	}
	raw, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{{CandidateIDs: []string{secondNote}, Status: "candidate", Reason: "Incomplete group"}}})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "incomplete", Payload: raw, ExpectedVersion: DecisionStateVersion(input)})
		return err
	})
	requireAPIStatus(t, err, 409)
	f.curate(curator, input, CurateGroup{CandidateIDs: []string{firstNote, secondNote}, Status: "candidate", Reason: "Merge parallel interpretations without claiming verification"})
	f.finish(curator, "")
	s := f.state()
	if len(s.Candidates) != 3 || len(s.Findings) != 1 || s.Findings[0].Status != "candidate" {
		t.Fatal("ordinary note was promoted or removed during unrelated reconciliation")
	}
	assertCurationNeeded(t, f, false)
}

func TestCurationReconcilesOppositeJudgmentsFromOneProducer(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	fact := f.fact(producer, "observation")
	f.candidate(producer, fact, "verified")
	f.action(producer.Fence(), "candidate", "opposing-judgment", map[string]any{
		"claim": "The fixture is reachable", "scope": "same controlled condition", "status": "refuted", "sources": []string{fact}, "reason": "A separate unretracted interpretation conflicts with the first",
	}, "")
	assertCurationNeeded(t, f, true)
}

func TestCandidateRevisionPreservesEvidenceAndCannotEraseAnotherProducerConflict(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	oldFact := f.fact(producer, "first-observation")
	old := f.candidate(producer, oldFact, "verified")
	original := f.state().Candidates[0]
	freshFact := f.fact(producer, "changed-observation")
	revise := func(id, status, key string) string {
		return f.action(producer.Fence(), "candidate", key, map[string]any{
			"claim": original.Claim, "scope": original.Scope, "status": status, "supersedes": id,
			"sources": []string{freshFact}, "reason": "Revise my interpretation using fresh original evidence",
		}, "").ID
	}
	revised := revise(old, "refuted", "revision")
	s := f.state()
	if !reflect.DeepEqual(s.Candidates[0], original) || len(s.Candidates) != 2 || len(s.ActiveCandidates()) != 1 || s.ActiveCandidates()[0].ID != revised || s.ActiveCandidates()[0].Supersedes != old {
		t.Fatal("revision changed immutable evidence or left the old interpretation active")
	}
	assertCurationNeeded(t, f, false)
	other := f.worker("other", "")
	otherNote := f.candidate(other, f.fact(other, "other-observation"), "verified")
	assertCurationNeeded(t, f, true)
	curator, input := f.curator("conflict")
	f.curate(curator, input, CurateGroup{CandidateIDs: []string{revised, otherNote}, Status: "candidate", Reason: "Active producer interpretations disagree", Question: "Which observation holds under an independent check?"})
	f.finish(curator, "")
	latest := revise(revised, "verified", "later-revision")
	assertCurationNeeded(t, f, true)
	curator, input = f.curator("cannot-erase-conflict")
	f.curate(curator, input, CurateGroup{CandidateIDs: []string{latest, otherNote}, Status: "verified", Reason: "Agreement after self-revision does not independently resolve the old conflict"})
	f.finish(curator, "")
	s = f.state()
	if len(s.Candidates) != 4 || len(s.ActiveCandidates()) != 2 || len(s.Disputes) != 1 || s.Disputes[0].Status != "open" || s.Findings[0].Status != "candidate" || !reflect.DeepEqual(s.Candidates[0], original) {
		t.Fatal("a producer revision erased independent conflict or original evidence")
	}
	assertCurationNeeded(t, f, false)
}

func TestCandidateRevisionRejectsForeignStaleAndChangedIdentityTargets(t *testing.T) {
	for _, test := range []string{"foreign_run", "superseded_target", "changed_claim", "changed_scope"} {
		t.Run(test, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			fact := f.fact(producer, "observation")
			note := f.candidate(producer, fact, "candidate")
			original := f.state().Candidates[0]
			payload := map[string]any{"claim": original.Claim, "scope": original.Scope, "supersedes": note, "sources": []string{fact}, "reason": "Revise an interpretation"}
			fence := producer.Fence()
			switch test {
			case "foreign_run":
				fence = f.worker("other", "").Fence()
			case "superseded_target":
				f.action(fence, "candidate", "first-revision", payload, "")
			case "changed_claim":
				payload["claim"] = "Another hypothesis"
			case "changed_scope":
				payload["scope"] = "Another condition"
			}
			before := f.state()
			raw, _ := json.Marshal(payload)
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", fence, StateAction{Op: "candidate", IdempotencyKey: "rejected", Payload: raw})
				return err
			})
			requireAPIStatus(t, err, 409)
			if !reflect.DeepEqual(f.state(), before) {
				t.Fatal("rejected revision changed source history or active notes")
			}
		})
	}
}
