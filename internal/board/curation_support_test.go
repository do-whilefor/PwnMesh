package board

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func curationCandidateWithSupport(f *orchestrationFixture, e Execution, label, status string, sources ...string) string {
	f.t.Helper()
	return f.action(e.Fence(), "candidate", "candidate:"+label, map[string]any{
		"claim": "The fixture is reachable", "scope": "same controlled condition", "status": status,
		"sources": sources, "reason": "Observed result of " + label,
		"evidence": []EvidenceRef{{RunID: e.Lease, Path: "/workspace/" + e.ID + "/" + label + ".txt", Excerpt: "Retained evidence for " + label}},
	}, "").ID
}

func TestCurationFreshEvidenceReplacesStaleSupportWithoutErasingHistory(t *testing.T) {
	for _, status := range []string{"verified", "refuted"} {
		for _, relation := range []string{"supersedes", "refutes", "narrows"} {
			t.Run(status+"/"+relation, func(t *testing.T) {
				f := newOrchestrationFixture(t)
				oldRun := f.worker("old-producer", "")
				oldFact, stillValid := f.fact(oldRun, "old"), f.fact(oldRun, "other-premise")
				oldCandidate := curationCandidateWithSupport(f, oldRun, "old", status, oldFact, stillValid)
				curator, input := f.curator("initial-curation")
				firstReceipt := f.curate(curator, input, CurateGroup{CandidateIDs: []string{oldCandidate}, Status: status, Reason: "Initial supported judgment"})
				f.finish(curator, "")

				freshRun := f.worker("fresh-producer", "")
				freshFact := f.fact(freshRun, "fresh")
				freshCandidate := curationCandidateWithSupport(f, freshRun, "fresh", status, freshFact)
				curator, input = f.curator("fresh-curation")
				payload := CuratePayload{ThroughRevision: input.Revision,
					Relations: []CurateRelation{{Kind: relation, Source: freshFact, Target: oldFact, Reason: "Fresh observation corrects an old premise"}},
					// The old candidate remains in the finding even when omitted
					// from this group; only its current support is replaced.
					Groups: []CurateGroup{{CandidateIDs: []string{freshCandidate}, Status: status, Reason: "Fresh evidence supports the same judgment"}},
				}
				receipt := f.action(curator.Fence(), "curate", "replace-support", payload, DecisionStateVersion(input))
				after := f.state()
				if len(after.Findings) != 1 || len(after.Disputes) != 0 {
					t.Fatalf("same judgment created an unexpected finding or dispute: %+v", after.Findings)
				}
				finding := after.Findings[0]
				if finding.Status != status || !finding.SupportValid || !reflect.DeepEqual(finding.Sources, []string{freshFact}) || !reflect.DeepEqual(finding.CandidateIDs, []string{oldCandidate, freshCandidate}) {
					t.Fatalf("stale premises contaminated the fresh conclusion: %+v", finding)
				}
				if !reflect.DeepEqual(finding.Evidence, input.Candidates[1].Evidence) {
					t.Fatalf("stale candidate evidence leaked into current support: %+v", finding.Evidence)
				}
				if !reflect.DeepEqual(after.Candidates, input.Candidates) || after.ValidateFactSources([]string{stillValid}, true) != nil || after.ValidateFactSources([]string{oldFact}, true) == nil {
					t.Fatal("replacement changed historical judgments or failed to invalidate only the corrected premise")
				}
				f.do(func(tx *Tx) error {
					historical, err := tx.CurationReceipt("p", "curator@initial-curation")
					if err == nil && !reflect.DeepEqual(historical, firstReceipt) {
						t.Fatal("replacement changed the original curation receipt")
					}
					return err
				})
				replay := f.action(curator.Fence(), "curate", "replace-support", payload, DecisionStateVersion(input))
				if !reflect.DeepEqual(replay, receipt) || !reflect.DeepEqual(f.state(), after) {
					t.Fatal("replacement receipt replay changed current support or history")
				}
			})
		}
	}
}

func TestCurationStaleJudgmentCannotBorrowTentativeEvidence(t *testing.T) {
	for _, status := range []string{"verified", "refuted"} {
		t.Run(status, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			oldFact, correction := f.fact(producer, "old"), f.fact(producer, "correction")
			oldCandidate := curationCandidateWithSupport(f, producer, "old", status, oldFact)
			freshCandidate := curationCandidateWithSupport(f, producer, "tentative", "candidate", correction)
			curator, input := f.curator("curator")
			raw, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision,
				Relations: []CurateRelation{{Kind: "supersedes", Source: correction, Target: oldFact, Reason: "The old premise is stale"}},
				Groups: []CurateGroup{{CandidateIDs: []string{oldCandidate, freshCandidate}, Status: status,
					Reason: "A tentative replacement cannot substantiate the old judgment"}},
			})
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "unsupported-verdict", Payload: raw, ExpectedVersion: DecisionStateVersion(input)})
				return err
			})
			requireAPIStatus(t, err, 409)
			if !reflect.DeepEqual(f.state(), input) {
				t.Fatal("unsupported verdict persisted part of the curation batch")
			}
			f.do(func(tx *Tx) error {
				_, err := tx.CurationReceipt("p", curator.Lease)
				requireAPIStatus(t, err, 404)
				return nil
			})
		})
	}
}

func TestCurationStaleOpposingJudgmentStillRequiresIndependentReview(t *testing.T) {
	f := newOrchestrationFixture(t)
	first, second := f.worker("first", ""), f.worker("second", "")
	oldFact, freshFact := f.fact(first, "old"), f.fact(second, "fresh")
	oldCandidate := curationCandidateWithSupport(f, first, "old", "verified", oldFact)
	freshCandidate := curationCandidateWithSupport(f, second, "fresh", "refuted", freshFact)
	curator, input := f.curator("curator")
	f.action(curator.Fence(), "curate", "preserve-conflict", CuratePayload{ThroughRevision: input.Revision,
		Relations: []CurateRelation{{Kind: "refutes", Source: freshFact, Target: oldFact, Reason: "The new observation challenges the old premise"}},
		Groups: []CurateGroup{{CandidateIDs: []string{oldCandidate, freshCandidate}, Status: "refuted",
			Reason: "Opposing judgments still need independent review", Question: "Does the fixture respond under the disputed condition?"}},
	}, DecisionStateVersion(input))
	after := f.state()
	if len(after.Disputes) != 1 || after.Disputes[0].Status != "open" || after.Findings[0].Status != "candidate" || !reflect.DeepEqual(after.Findings[0].Sources, []string{freshFact}) {
		t.Fatalf("invalidating a historical premise erased the unresolved conflict: %+v", after.Disputes)
	}
	if !reflect.DeepEqual(after.Candidates, input.Candidates) || !reflect.DeepEqual(after.Disputes[0].CandidateIDs, []string{oldCandidate, freshCandidate}) || len(after.Disputes[0].ProducerRunIDs) != 2 {
		t.Fatal("conflict history or producer independence boundary was lost")
	}
	f.do(func(tx *Tx) error {
		requireAPIStatus(t, tx.ValidateStateCompletion("p", []string{freshFact}), 409)
		return nil
	})
}

func TestCurationRetainsSourceFreeTentativeEvidence(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	candidate := curationCandidateWithSupport(f, producer, "tentative", "candidate")
	curator, input := f.curator("curator")
	f.curate(curator, input, CurateGroup{CandidateIDs: []string{candidate}, Status: "candidate", Reason: "Retain the tentative observation without promoting its confidence"})
	finding := f.state().Findings[0]
	if finding.Status != "candidate" || finding.SupportValid || len(finding.Sources) != 0 || !reflect.DeepEqual(finding.Evidence, input.Candidates[0].Evidence) {
		t.Fatalf("source-free candidate evidence was lost or promoted: %+v", finding)
	}
}
