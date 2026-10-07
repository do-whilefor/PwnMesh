package board

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestCompletionAssessmentDoesNotWaitForPlannerWorkOrAcceptCoverage(t *testing.T) {
	f := newOrchestrationFixture(t)
	assess := func(want bool) *CompletionReview {
		t.Helper()
		before := f.state()
		var review *CompletionReview
		f.do(func(tx *Tx) error {
			var err error
			review, err = tx.CompletionAssessment(before)
			return err
		})
		if (review != nil) != want || !reflect.DeepEqual(before, f.state()) {
			t.Fatalf("assessment readiness=%v want %v, or assessment mutated state: %+v", review != nil, want, review)
		}
		return review
	}
	assess(false) // No observations.
	producer := f.worker("producer", "")
	fact := f.fact(producer, "no HTTP response was obtained")
	assess(true) // Running work must not suppress root assessment.
	f.finish(producer, fact)
	assess(true) // Plain observations need no separate curation turn.
	curator, input := f.curator("curator")
	f.curate(curator, input)
	assess(true) // A curator lease blocks commit, not root assessment.
	f.finish(curator, "")
	review := assess(true)
	if review.Acceptance != "not_checked" || review.Description != "" || review.StateVersion != DecisionStateVersion(f.state()) || !reflect.DeepEqual(review.From, []string{fact}) || len(review.FactRecords) != 1 || !strings.Contains(review.FactRecords[0].Description, "no HTTP response") {
		t.Fatalf("eligibility invented success or altered negative evidence: %+v", review)
	}
	if !reflect.DeepEqual(review.UserInputs, f.state().Graph.Facts[:2]) {
		t.Fatal("assessment replaced original requirements with planner goals")
	}
	f.action(f.planner, "goal", "missing-coverage", map[string]any{"action": "add", "condition": "Inspect another required endpoint"}, "")
	assess(true) // Planner-created goals cannot suppress root assessment.
}

func TestCompletionAssessmentDoesNotBypassDisputes(t *testing.T) {
	f := newOrchestrationFixture(t)
	a, b := f.worker("positive", ""), f.worker("negative", "")
	fa, fb := f.fact(a, "positive"), f.fact(b, "negative")
	ca, cb := f.candidate(a, fa, "verified"), f.candidate(b, fb, "refuted")
	f.finish(a, fa)
	f.finish(b, fb)
	curator, input := f.curator("conflict")
	f.curate(curator, input, CurateGroup{CandidateIDs: []string{ca, cb}, Status: "candidate", Reason: "Evidence disagrees", Question: "Which observation applies?"})
	f.finish(curator, "")
	state := f.state()
	f.do(func(tx *Tx) error {
		review, err := tx.CompletionAssessment(state)
		if review == nil || len(review.Disputes) == 0 || review.Acceptance != "not_checked" {
			t.Fatal("root assessment lost the unresolved conflict")
		}
		requireAPIStatus(t, tx.ValidateStateCompletion("p", []string{fa}), 409)
		return err
	})
}

func TestCompletionContextKeepsCoverageAndDeduplicatesEvidence(t *testing.T) {
	state, _ := completionReviewFixture()
	state.Graph.Project = Project{ID: "p", Status: "active", OrchestrationVersion: 1}
	state.Goals = []Goal{{ID: "goal", Condition: state.Graph.Facts[1].Description, Status: "open"}, {ID: "withdrawn", Condition: "Required endpoint /missing remains untested", Status: "withdrawn", Reason: "Planner withdrew this; user never waived it"}}
	state.FactRecords = nil
	ids := []string{}
	for n := 0; n < 6; n++ {
		id := fmt.Sprintf("f%03d", n)
		ids = append(ids, id)
		state.FactRecords = append(state.FactRecords, FactRecord{ID: id, Description: "Observed endpoint", Scope: "prod", Status: "valid", Evidence: []EvidenceRef{{RunID: "run", Path: "/evidence/" + id, Excerpt: id + strings.Repeat("x", 1600)}}})
		state.Steps = append(state.Steps, Step{ID: "step-" + id, GoalID: "goal", Description: "Inspect required endpoint " + id, Status: "completed", Result: Ptr(id), From: []string{"origin"}, SupportValid: true})
	}
	state.Steps = append(state.Steps, Step{ID: "failed", GoalID: "withdrawn", Description: "The missing endpoint was not reached", Status: "failed", Reason: "Connection refused", From: []string{"origin"}})
	view, err := ContextView(state, "", DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"from": ids, "description": ""})
	review, err := buildCompletionReview(state, DecisionStateVersion(state), payload, MaxCompletionReviewBytes)
	if err != nil {
		t.Fatal(err)
	}
	d := &DecisionContext{Version: 2, StateVersion: review.StateVersion, View: view, Mode: "full", BaselineBytes: len(view)}
	duplicate, _ := json.Marshal(struct {
		View       json.RawMessage
		Assessment *CompletionReview
	}{view, review})
	if err := UseCompletionAssessment(state, d, review); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d)
	if d.Mode != "completion" || d.CompletionAssessment == nil || len(raw) >= len(duplicate) || len(raw) > DefaultContextViewBytes {
		t.Fatalf("completion packet repeated input or exceeded budget: before=%d after=%d mode=%s", len(duplicate), len(raw), d.Mode)
	}
	var coverage struct {
		Goals []Goal
		Steps []Step
	}
	if json.Unmarshal(d.View, &coverage) != nil || !reflect.DeepEqual(coverage.Goals, state.Goals) || !reflect.DeepEqual(coverage.Steps, state.Steps) {
		t.Fatal("completion packet dropped failed/withdrawn coverage or task inputs")
	}
	if !reflect.DeepEqual(d.CompletionAssessment.UserInputs, state.Graph.Facts) || !reflect.DeepEqual(d.CompletionAssessment.Hints, state.Graph.Hints) {
		t.Fatal("original requirements or user hints were lost")
	}
	for _, fact := range state.FactRecords {
		if strings.Count(string(raw), fact.Evidence[0].Excerpt) != 1 {
			t.Fatal("accepted observation evidence was repeated or omitted")
		}
	}
}

func TestCompletionContextMarksWholeFactOmissionsAndFallsBackForLargeCoverage(t *testing.T) {
	state, payload := completionReviewFixture()
	state.FactRecords[0].Evidence[0].Excerpt = strings.Repeat("x", DefaultContextViewBytes)
	review, err := buildCompletionReview(state, "v", payload, MaxCompletionReviewBytes)
	if err != nil {
		t.Fatal(err)
	}
	d := &DecisionContext{StateVersion: "v", View: json.RawMessage(`{"original":true}`), Mode: "full"}
	if err := UseCompletionAssessment(state, d, review); err != nil {
		t.Fatal(err)
	}
	if d.CompletionAssessment == nil || len(d.CompletionAssessment.FactRecords) != 0 || !reflect.DeepEqual(d.CompletionAssessment.OmittedFactIDs, []string{"f001"}) {
		t.Fatalf("large evidence was silently truncated: %+v", d.CompletionAssessment)
	}
	state.Steps = []Step{{ID: "failed", Status: "failed", Description: strings.Repeat("coverage", DefaultContextViewBytes)}}
	d = &DecisionContext{StateVersion: "v", View: json.RawMessage(`{"original":true}`), Mode: "full"}
	if err := UseCompletionAssessment(state, d, review); err != nil {
		t.Fatal(err)
	}
	if d.CompletionAssessment != nil || d.Mode != "full" || string(d.View) != `{"original":true}` {
		t.Fatal("oversized mandatory coverage did not retain the normal planning path")
	}
}
