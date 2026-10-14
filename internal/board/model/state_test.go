package model

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func readStateFixture(t *testing.T, name string) State {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

// Recorded from Board before extraction, including curation after asset anchors.
func TestStatePreservesOriginalCanonicalBytesAndVersion(t *testing.T) {
	expected, err := os.ReadFile("testdata/state.json")
	if err != nil {
		t.Fatal(err)
	}
	state := readStateFixture(t, "state.json")
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, bytes.TrimSpace(expected)) {
		t.Fatalf("canonical state bytes changed:\n%s", raw)
	}
	if got := DecisionStateVersion(state); got != "f42d8500b2ef68afc97ddd08d5773317f7502507b9e22eda6cf16dd425ff5d43" {
		t.Fatalf("original decision version changed: %s", got)
	}
	after, err := json.Marshal(state)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("version computation changed the input snapshot")
	}
}

func TestDecisionStateVersionIgnoresRuntimeMetadataButRetainsSharedInputs(t *testing.T) {
	state := readStateFixture(t, "state.json")
	version := DecisionStateVersion(state)
	state.Revision, state.DecisionRevision = 99, 88
	state.Graph.Project.Reason = &Reason{Worker: "another-planner"}
	state.Graph.Project.Curator = nil
	state.Graph.Intents[0].Worker = Ptr("another-worker")
	state.Graph.Intents[0].Heartbeat = nil
	state.Steps[0].Worker = nil
	state.Steps[0].Status = "open"
	if got := DecisionStateVersion(state); got != version {
		t.Fatal("runtime activity invalidated unchanged shared inputs")
	}
	state.Curation.Request.Reason = "Additional evidence needs review"
	if DecisionStateVersion(state) == version {
		t.Fatal("changed curation input did not invalidate the decision")
	}
}

func TestLegacyProjectionKeepsOriginalInputAndCanonicalStateSeparate(t *testing.T) {
	state := State{Graph: Graph{
		Project: Project{ID: "legacy", Status: "active"},
		Facts:   []Fact{{ID: "origin", Description: "Initial scope"}, {ID: "goal", Description: "Requested proof"}, {ID: "feedback", Description: "Follow-up scope"}},
		Intents: []Intent{{ID: "feedback-intent", Description: "external_feedback", To: Ptr("feedback")}},
	}}
	original, _ := json.Marshal(state)
	projected := state.WithLegacyDefaults()
	if len(projected.Goals) != 1 || projected.Goals[0].Condition != "Requested proof" || len(projected.Steps) != 1 || !projected.ExternalFeedbackStep(projected.Steps[0]) {
		t.Fatalf("legacy graph projection changed: %+v", projected)
	}
	inputs, ids := state.UserInputFacts()
	if len(inputs) != 3 || !ids["feedback"] || len(state.History()) != 0 {
		t.Fatal("legacy human feedback was omitted or treated as execution history")
	}
	after, _ := json.Marshal(state)
	if !bytes.Equal(original, after) || len(state.Steps) != 0 || len(state.FactRecords) != 0 {
		t.Fatal("read projection replaced the canonical graph-only snapshot")
	}
}
