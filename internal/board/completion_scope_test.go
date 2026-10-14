package board

import (
	"reflect"
	"testing"
)

func TestCompletionDoesNotNeedCurationForPlainEvidenceOrTentativeNotes(t *testing.T) {
	for _, note := range []bool{false, true} {
		t.Run(map[bool]string{false: "observation", true: "tentative_note"}[note], func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			fact := f.fact(producer, "result")
			if note {
				f.candidate(producer, fact, "candidate")
			}
			f.finish(producer, fact)
			f.do(func(tx *Tx) error {
				return f.complete(tx, []string{fact}, "Original goal verified by retained observation")
			})
			state := f.state()
			if state.Graph.Project.Status != "completed" || state.Curation.ThroughRevision != 0 || len(state.Findings) != 0 {
				t.Fatal("completion required manufacturing a shared conclusion")
			}
		})
	}
}

func TestCompletionKeepsUnrelatedConflictsWithoutBlockingIndependentProof(t *testing.T) {
	for _, curated := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending_conflict", true: "open_dispute"}[curated], func(t *testing.T) {
			f := newOrchestrationFixture(t)
			f.do(func(tx *Tx) error {
				graph, err := tx.Load("p")
				if err != nil {
					return err
				}
				graph.Facts[1].Description = "Confirm the independent release health check"
				return tx.Save(graph)
			})
			a, b, independent := f.worker("a", ""), f.worker("b", ""), f.worker("independent", "")
			fa, fb, proof := f.fact(a, "a"), f.fact(b, "b"), f.fact(independent, "release-health")
			ca, cb := f.candidate(a, fa, "verified"), f.candidate(b, fb, "refuted")
			f.finish(a, fa)
			f.finish(b, fb)
			f.finish(independent, proof)
			if curated {
				curator, input := f.curator("curator")
				f.curate(curator, input, CurateGroup{CandidateIDs: []string{ca, cb}, Status: "candidate", Reason: "Unresolved incidental interpretation", Question: "Which incidental observation applies?"})
				f.finish(curator, "")
			}
			before := f.state()
			f.do(func(tx *Tx) error {
				requireAPIStatus(t, tx.ValidateStateCompletion("p", []string{fa}), 409)
				return tx.ValidateStateCompletion("p", []string{proof})
			})
			f.do(func(tx *Tx) error {
				return f.complete(tx, []string{proof}, "The independent health response fulfills the user requirement; incidental uncertainty is retained")
			})
			after := f.state()
			if after.Graph.Project.Status != "completed" || !reflect.DeepEqual(before.Candidates, after.Candidates) || !reflect.DeepEqual(before.Disputes, after.Disputes) {
				t.Fatal("completion erased conflict history or failed to finish")
			}
		})
	}
}
