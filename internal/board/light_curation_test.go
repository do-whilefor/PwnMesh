package board

import (
	"context"
	"encoding/json"
	"pwnmesh/internal/board/model"
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

func TestCurationRelationBatchWakesUnsupportedFindingOnce(t *testing.T) {
	for _, kind := range []string{"refutes", "supersedes", "narrows"} {
		t.Run(kind, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			original := f.fact(producer, "original")
			candidate := f.candidate(producer, original, "verified")
			f.finish(producer, original)
			curator, input := f.curator("initial")
			f.curate(curator, input, CurateGroup{CandidateIDs: []string{candidate}, Status: "verified", Reason: "Initial supported observation"})
			assertCurationNeeded(t, f, false)

			corrector := f.worker("corrector", "")
			correction := f.fact(corrector, "correction")
			f.finish(corrector, correction)
			curator, input = f.curator("correction")
			f.action(curator.Fence(), "curate", "correct-evidence", CuratePayload{
				ThroughRevision: input.Revision, Groups: []CurateGroup{},
				Relations: []CurateRelation{{Kind: kind, Source: correction, Target: original, Reason: "Correct the retained premise"}},
			}, DecisionStateVersion(input))
			if finding := f.state().Findings[0]; finding.Status != "verified" || finding.SupportValid {
				t.Fatalf("fixture did not invalidate the retained judgment: %+v", finding)
			}
			assertCurationNeeded(t, f, true)
			raw, err := CurationContextView(f.state(), DefaultContextViewBytes)
			if err != nil {
				t.Fatal(err)
			}
			var view struct {
				Candidates []model.CandidateView `json:"candidates"`
				Coverage   curationInputCoverage `json:"curation_input"`
			}
			if err := json.Unmarshal(raw, &view); err != nil {
				t.Fatal(err)
			}
			if len(view.Candidates) != 1 || view.Candidates[0].ID != candidate || view.Candidates[0].SupportValid == nil || *view.Candidates[0].SupportValid || view.Coverage.RelevantCandidates != 1 || view.Coverage.OmittedCandidates != 0 {
				t.Fatalf("reconciliation lost the invalidated finding's producer judgment: %+v", view)
			}

			// Acknowledging this correction must not make the curator's own
			// event another correction and cause an unbounded idle loop.
			curator, input = f.curator("acknowledge-correction")
			f.curate(curator, input)
			assertCurationNeeded(t, f, false)

			curator, input = f.curator("duplicate-correction")
			f.action(curator.Fence(), "curate", "repeat-correction", CuratePayload{
				ThroughRevision: input.Revision, Groups: []CurateGroup{},
				Relations: []CurateRelation{{Kind: kind, Source: correction, Target: original, Reason: "Reword the already acknowledged correction"}},
			}, DecisionStateVersion(input))
			assertCurationNeeded(t, f, false)

			other := f.worker("unrelated", "")
			unrelatedOld, unrelatedNew := f.fact(other, "unrelated-old"), f.fact(other, "unrelated-new")
			f.finish(other, unrelatedNew)
			curator, input = f.curator("unrelated-correction")
			f.action(curator.Fence(), "curate", "correct-unrelated-evidence", CuratePayload{
				ThroughRevision: input.Revision, Groups: []CurateGroup{},
				Relations: []CurateRelation{{Kind: kind, Source: unrelatedNew, Target: unrelatedOld, Reason: "Correct an unrelated observation"}},
			}, DecisionStateVersion(input))
			assertCurationNeeded(t, f, false)
		})
	}
}

func TestCurationRelationBatchWakesTransitivelyUnsupportedFinding(t *testing.T) {
	for _, edge := range []string{"from", "depends_on"} {
		t.Run(edge, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			upstream := f.worker("upstream", "")
			original := f.fact(upstream, "original")
			f.finish(upstream, original)
			payload := map[string]any{"action": "add", "description": "Use the upstream result", "from": []string{original}}
			if edge == "depends_on" {
				payload["from"], payload["depends_on"] = []string{"origin"}, []string{upstream.Intent}
			}
			step := f.action(f.planner, "step", "consumer-step", payload, "").ID
			consumer := dependencyRun(f, "consumer", step)
			f.do(func(tx *Tx) error { return tx.RegisterExecution(consumer) })
			result := f.fact(consumer, "accepted-result")
			candidate := f.candidate(consumer, result, "verified")
			f.finish(consumer, result)
			curator, input := f.curator("initial")
			f.curate(curator, input, CurateGroup{CandidateIDs: []string{candidate}, Status: "verified", Reason: "Supported downstream result"})
			assertCurationNeeded(t, f, false)
			corrector := f.worker("corrector", "")
			correction := f.fact(corrector, "correction")
			f.finish(corrector, correction)
			curator, input = f.curator("correct-upstream")
			f.action(curator.Fence(), "curate", "correct-upstream", CuratePayload{
				ThroughRevision: input.Revision, Groups: []CurateGroup{},
				Relations: []CurateRelation{{Kind: "refutes", Source: correction, Target: original, Reason: "Invalidate the accepted result's prerequisite"}},
			}, DecisionStateVersion(input))
			assertCurationNeeded(t, f, true)
		})
	}
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
