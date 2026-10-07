package board

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompletionDisputeScopeFollowsEvidenceDependencies(t *testing.T) {
	for _, path := range []string{"direct", "producer", "prerequisite", "relation", "child_goal", "unrelated", "same_producer"} {
		t.Run(path, func(t *testing.T) {
			state := State{
				FactRecords: []FactRecord{{ID: "proof", SourceStepID: "report"}, {ID: "disputed"}, {ID: "independent"}},
				Steps:       []Step{{ID: "report", From: []string{"origin"}, Result: Ptr("proof")}, {ID: "probe", From: []string{"origin"}, Result: Ptr("disputed")}},
				Candidates:  []Candidate{{ID: "candidate", Sources: []string{"disputed"}}},
				Disputes:    []Dispute{{ID: "d", Status: "uncertain", CandidateIDs: []string{"candidate"}}},
			}
			from := []string{"proof"}
			switch path {
			case "direct":
				from = []string{"disputed"}
			case "producer":
				state.Steps[0].From = []string{"disputed"}
			case "prerequisite":
				state.Steps[0].DependsOn = []string{"probe"}
			case "relation":
				state.FactRelations = []FactRelation{{Kind: "narrows", Source: "proof", Target: "disputed"}}
			case "child_goal":
				state.Goals = []Goal{{ID: "required", Status: "achieved", Sources: []string{"disputed"}}}
			case "same_producer":
				state.Steps[0].Result = Ptr("disputed")
			}
			before := state.Disputes[0]
			err := state.validateCompletionDisputes(from)
			if path == "unrelated" || path == "same_producer" {
				if err != nil {
					t.Fatalf("shared origin made independent evidence disputed: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "unresolved dispute") {
				t.Fatalf("%s evidence dependency bypassed dispute: %v", path, err)
			}
			if !reflect.DeepEqual(state.Disputes[0], before) {
				t.Fatal("completion scope changed the unresolved judgment")
			}
		})
	}
}

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
				_, err := tx.CompleteProject("p", f.planner, []string{fact}, "Original goal verified by retained observation")
				return err
			})
			state := f.state()
			if state.Graph.Project.Status != "completed" || state.Curation.ThroughRevision != 0 || len(state.Findings) != 0 {
				t.Fatal("completion required manufacturing a shared conclusion")
			}
		})
	}
}

func TestCompletionScopesExplicitCurationRequestsToProof(t *testing.T) {
	state := State{Revision: 4, Curation: CurationProgress{ThroughRevision: 2, Request: &CurationRequest{Revision: 3, Sources: []string{"disputed"}, Reason: "Original observations appear inconsistent"}}}
	if err := state.validateCompletionDisputes([]string{"disputed"}); err == nil {
		t.Fatal("completion bypassed an explicit request to reconcile its evidence")
	}
	if err := state.validateCompletionDisputes([]string{"independent"}); err != nil {
		t.Fatalf("unrelated request vetoed independent proof: %v", err)
	}
	state.Curation.ThroughRevision = 3
	if err := state.validateCompletionDisputes([]string{"disputed"}); err != nil {
		t.Fatalf("acknowledged request still blocks completion: %v", err)
	}
	state.Curation.ThroughRevision = 2
	state.Graph.Project.Generation = 1
	if err := state.validateCompletionDisputes([]string{"disputed"}); err != nil {
		t.Fatalf("earlier-round request leaked into new goal: %v", err)
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
				_, err := tx.CompleteProject("p", f.planner, []string{proof}, "The independent health response fulfills the user requirement; incidental uncertainty is retained")
				return err
			})
			after := f.state()
			if after.Graph.Project.Status != "completed" || !reflect.DeepEqual(before.Candidates, after.Candidates) || !reflect.DeepEqual(before.Disputes, after.Disputes) {
				t.Fatal("completion erased conflict history or failed to finish")
			}
		})
	}
}
