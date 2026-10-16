package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFactDerivationCompletionAndReadingClosure(t *testing.T) {
	state := State{Revision: 4, Graph: Graph{Project: Project{OrchestrationVersion: 1}},
		FactRecords: []FactRecord{{ID: "raw", Status: "valid", SourceStepID: "producer"}, {ID: "middle", Status: "valid", DerivedFrom: []string{"raw"}}, {ID: "proof", Status: "valid", DerivedFrom: []string{"middle"}}},
		Steps:       []Step{{ID: "producer", Status: "completed", SupportValid: true, Result: Ptr("raw")}},
		Findings:    []Finding{{ID: "finding", Sources: []string{"proof"}}},
		Curation:    CurationProgress{Request: &CurationRequest{Revision: 3, Sources: []string{"raw"}, Reason: "Check the input"}},
	}
	if err := state.ValidateCompletionDisputes([]string{"proof"}); err == nil {
		t.Fatal("derived proof bypassed its source's pending curation request")
	}
	if history := state.History(); len(history) != 0 {
		t.Fatal("reading history hid a needed derived Fact ancestor")
	}
	selected := map[string]bool{"proof": true}
	factSupportClosure(state.FactRecords, []FactRelation{{Source: "correction", Target: "raw"}}, selected)
	for _, id := range []string{"proof", "middle", "raw", "correction"} {
		if !selected[id] {
			t.Fatalf("context omitted explicit support %s", id)
		}
	}
	decision, err := buildDecisionChanges(state, json.RawMessage(`{}`), &DecisionContext{}, DecisionChanges{"fact_records": {"raw"}}, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var view DecisionView
	if json.Unmarshal(decision.View, &view) != nil || len(view.Facts) != 3 {
		t.Fatalf("changed input hid derived consumers from Decide: %s", decision.View)
	}
}

func TestFactDerivationOptionalEncodingAndShape(t *testing.T) {
	raw, _ := json.Marshal(FactRecord{ID: "raw"})
	if strings.Contains(string(raw), "derived_from") {
		t.Fatal("absent derivation changed historical Fact encoding")
	}
	for _, input := range []string{`null`, `[null]`, `[""]`, `[" f1"]`, `["f1","f1"]`, `"f1"`} {
		if _, err := ParseFactDerivation(json.RawMessage(input)); err == nil {
			t.Fatalf("malformed derivation accepted: %s", input)
		}
	}
	for _, input := range []string{``, `[]`, `["f1","f2"]`} {
		if _, err := ParseFactDerivation(json.RawMessage(input)); err != nil {
			t.Fatalf("valid derivation rejected: %s: %v", input, err)
		}
	}
}
