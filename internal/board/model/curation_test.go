package model

import (
	"reflect"
	"testing"
)

func TestCandidateViewGroupsUseExactIdentityAndCurrentProducerSupport(t *testing.T) {
	if got := FindingIdentity("The fixture requires authentication", "GET /private"); got != "finding_c2005a6a88cab90ba74c6828" {
		t.Fatalf("persisted finding identity changed: %s", got)
	}
	state := State{}
	state.Graph.Project.Generation = 0
	state.FactRecords = []FactRecord{{ID: "current", Status: "valid"}, {ID: "stale", Status: "valid", SupportInvalid: true}, {ID: "refuted", Status: "refuted"}, {ID: "origin", Status: "input"}}
	base := Candidate{ID: "a", Claim: "same\tclaim", Scope: " local ", Status: "verified", Sources: []string{"current"}}
	view := state.CandidateView(base)
	if view.GroupKey != FindingIdentity("same claim", "local") || view.SupportValid == nil || !*view.SupportValid || !reflect.DeepEqual(view.Candidate, base) {
		t.Fatalf("projection changed producer input or identity: %+v", view)
	}
	for _, tc := range []struct {
		name, claim, scope string
		same               bool
	}{
		{"normalized whitespace", "  same\nclaim ", "local", true},
		{"different claim", "related claim", "local", false},
		{"different scope", "same claim", "another scope", false},
		{"case remains distinct", "Same claim", "local", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := base
			candidate.Claim, candidate.Scope = tc.claim, tc.scope
			if got := state.CandidateView(candidate).GroupKey == view.GroupKey; got != tc.same {
				t.Fatal("projection grouping diverged from exact finding identity")
			}
		})
	}
	for _, tc := range []struct {
		name, status string
		sources      []string
		valid        bool
	}{
		{"verified", "verified", []string{"current"}, true},
		{"refuted", "refuted", []string{"current"}, true},
		{"tentative stays tentative", "candidate", []string{"current"}, true},
		{"missing", "verified", []string{"missing"}, false},
		{"no sources", "candidate", nil, false},
		{"partial valid support is insufficient", "verified", []string{"current", "stale"}, false},
		{"refuted source", "refuted", []string{"refuted"}, false},
		{"user input is not observation", "verified", []string{"origin"}, false},
		{"duplicate source", "verified", []string{"current", "current"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := base
			candidate.Status, candidate.Sources = tc.status, tc.sources
			view := state.CandidateView(candidate)
			if view.Status != tc.status || view.SupportValid == nil || *view.SupportValid != tc.valid {
				t.Fatalf("support hint raised producer confidence or lost invalidity: %+v", view)
			}
		})
	}
}
