package model

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
			err := state.ValidateCompletionDisputes(from)
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

func TestCompletionScopesExplicitCurationRequestsToProof(t *testing.T) {
	state := State{Revision: 4, Curation: CurationProgress{ThroughRevision: 2, Request: &CurationRequest{Revision: 3, Sources: []string{"disputed"}, Reason: "Original observations appear inconsistent"}}}
	if err := state.ValidateCompletionDisputes([]string{"disputed"}); err == nil {
		t.Fatal("completion bypassed an explicit request to reconcile its evidence")
	}
	if err := state.ValidateCompletionDisputes([]string{"independent"}); err != nil {
		t.Fatalf("unrelated request vetoed independent proof: %v", err)
	}
	state.Curation.ThroughRevision = 3
	if err := state.ValidateCompletionDisputes([]string{"disputed"}); err != nil {
		t.Fatalf("acknowledged request still blocks completion: %v", err)
	}
	state.Curation.ThroughRevision = 2
	state.Graph.Project.Generation = 1
	if err := state.ValidateCompletionDisputes([]string{"disputed"}); err != nil {
		t.Fatalf("earlier-round request leaked into new goal: %v", err)
	}
}
