package model

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestExecutionViewsPreserveCanonicalSnapshot(t *testing.T) {
	state := readStateFixture(t, "state.json")
	original, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	version := DecisionStateVersion(state)
	for _, step := range []string{"", "step"} {
		view, err := ContextView(state, step, DefaultContextViewBytes)
		if err != nil || len(view) > DefaultContextViewBytes || !bytes.Contains(view, []byte("Inspect the agreed scope")) {
			t.Fatalf("mandatory input lost from bounded execution view: %s, %v", view, err)
		}
	}
	curation, err := CurationContextView(state, DefaultContextViewBytes)
	if err != nil || !bytes.Contains(curation, []byte("Reconcile scope")) {
		t.Fatalf("curator view lost the pending request: %s, %v", curation, err)
	}
	decision, err := BuildDecisionContextFromCursor(state, nil, nil, DefaultContextViewBytes)
	if err != nil || decision.StateVersion != version || decision.Generation != state.Graph.Project.Generation {
		t.Fatalf("decision view lost its original input identity: %+v, %v", decision, err)
	}
	assessment, err := CompletionAssessment(state)
	if err != nil || assessment == nil || len(assessment.FactRecords) != 1 || assessment.FactRecords[0].ID != "proof" {
		t.Fatalf("assessment lost the evidence-backed observation: %+v, %v", assessment, err)
	}
	if err := UseCompletionAssessment(state, decision, assessment); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(state)
	if err != nil || !bytes.Equal(original, after) || DecisionStateVersion(state) != version {
		t.Fatal("reading execution views changed the immutable state or its digest")
	}
}
