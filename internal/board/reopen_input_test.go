package board

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func reopenInputFixture() (State, []Fact) {
	inputs := []Fact{{ID: "origin", Description: "Inspect production"}, {ID: "goal", Description: "Retain HTTP evidence"},
		{ID: "feedback-1", Description: "Also inspect /new as anonymous"}, {ID: "feedback-2", Description: "Keep the new response verbatim: \"响应\""}}
	g := Graph{Project: Project{ID: "p", Status: "active"}, Facts: append([]Fact{}, inputs...)}
	g.Facts = append(g.Facts, Fact{ID: "old-worker-result", Description: strings.Repeat("historical observation ", 2000)})
	g.Intents = []Intent{
		{ID: "reopen-1", From: []string{"origin"}, To: Ptr("feedback-1"), Description: "external_feedback"},
		{ID: "reopen-2", From: []string{"origin"}, To: Ptr("feedback-2"), Description: "external_feedback"},
		{ID: "old-worker", From: []string{"origin"}, To: Ptr("old-worker-result"), Description: "Inspect old endpoint"},
	}
	return State{Graph: g, Revision: 2}, inputs
}

func TestReopenRequirementsStayMandatoryAcrossPlanningViews(t *testing.T) {
	state, inputs := reopenInputFixture() // Historical jobs can contain Graph only.
	before, _ := json.Marshal(state)
	views := map[string]func() (json.RawMessage, error){
		"full":   func() (json.RawMessage, error) { return ContextView(state, "", DefaultContextViewBytes) },
		"curate": func() (json.RawMessage, error) { return CurationContextView(state, DefaultContextViewBytes) },
		"changes": func() (json.RawMessage, error) {
			d, err := BuildDecisionContextFromCursor(state, &DecisionCursor{ProjectID: "p", Revision: 1}, []StateEvent{{Revision: 2, Op: "reopen", ID: "feedback-2"}}, DefaultContextViewBytes)
			if err != nil {
				return nil, err
			}
			if d.Mode != "changes" {
				t.Fatalf("feedback fixture did not exercise incremental input: %s", d.Fallback)
			}
			return d.View, nil
		},
	}
	for name, build := range views {
		t.Run(name, func(t *testing.T) {
			raw, err := build()
			if err != nil {
				t.Fatal(err)
			}
			var view struct {
				UserInputs []Fact         `json:"user_inputs"`
				Facts      []contextFact  `json:"fact_records"`
				Omitted    map[string]int `json:"omitted"`
			}
			if err := json.Unmarshal(raw, &view); err != nil || !reflect.DeepEqual(view.UserInputs, inputs) {
				t.Fatalf("reopen requirements were omitted or worker output became input: %s (%v)", raw, err)
			}
			for _, fact := range view.Facts {
				if fact.ID != "old-worker-result" {
					t.Fatal("original input was duplicated as optional evidence", fact.ID)
				}
			}
			if view.Omitted["fact_records"] != 1-len(view.Facts) {
				t.Fatal("mandatory input counted as omitted history", view.Omitted)
			}
			for _, count := range view.Omitted {
				if count < 0 {
					t.Fatal("negative omission count")
				}
			}
		})
	}
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Fatal("projection mutated original input")
	}
}

func TestCompletionAssessmentPreservesRepeatedReopenRequirements(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	fact := f.fact(producer, "original result")
	f.finish(producer, fact)
	curator, input := f.curator("curator")
	f.curate(curator, input)
	f.finish(curator, "")
	want := append([]Fact{}, f.state().Graph.Facts[:2]...)
	for _, feedback := range []Fact{{ID: "feedback-1", Description: "The original result is insufficient; inspect /new"}, {ID: "feedback-2", Description: "Also retain the anonymous response"}} {
		f.do(func(tx *Tx) error {
			g, err := tx.Load("p")
			if err != nil {
				return err
			}
			g.Facts = append(g.Facts, feedback)
			g.Intents = append(g.Intents, Intent{ID: "step-" + feedback.ID, From: []string{fact}, To: Ptr(feedback.ID), Description: "external_feedback", Creator: "user", ConcludedAt: Ptr(tx.Now)})
			return tx.SaveUserInput(g, "reopen", feedback.ID, "", feedback, nil)
		})
		want = append(want, feedback)
		state := f.state()
		var assessment *CompletionReview
		f.do(func(tx *Tx) error { var err error; assessment, err = tx.CompletionAssessment(state); return err })
		if assessment == nil || !reflect.DeepEqual(assessment.UserInputs, want) || !reflect.DeepEqual(assessment.From, []string{fact}) {
			t.Fatalf("mechanically ready completion lost reopened requirements or promoted feedback to evidence: %+v", assessment)
		}
		d := &DecisionContext{Version: 2, StateVersion: DecisionStateVersion(state)}
		if err := UseCompletionAssessment(state, d, assessment); err != nil {
			t.Fatal(err)
		}
		if d.CompletionAssessment == nil || !reflect.DeepEqual(d.CompletionAssessment.UserInputs, want) {
			t.Fatal("bounded completion lost feedback")
		}
		raw, _ := json.Marshal(d)
		for _, original := range want[2:] {
			encoded, _ := json.Marshal(original.Description)
			if strings.Count(string(raw), string(encoded)) != 1 {
				t.Fatal("requirement was duplicated or omitted", original.ID)
			}
		}
	}
}

func TestReopenRequirementsCannotBeTruncatedToFitCompletion(t *testing.T) {
	for _, size := range []int{6000, 9000} {
		state, _ := reopenInputFixture()
		state.Graph.Facts[2].Description = strings.Repeat("\x01", size) // JSON escaping consumes six bytes per character.
		if err := ValidateContextCapacity(state); err == nil {
			t.Fatal("oversized reopened input passed admission")
		}
		state = contextState(state)
		state.FactRecords = append(state.FactRecords, FactRecord{ID: "evidence", Status: "valid", Evidence: []EvidenceRef{{Excerpt: "Original result"}}})
		payload := json.RawMessage(`{"from":["evidence"],"description":"Proposed proof"}`)
		review, err := buildCompletionReview(state, "v", payload, MaxCompletionReviewBytes)
		if size == 9000 {
			if err == nil || review != nil {
				t.Fatal("preview silently omitted oversized feedback")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		d := &DecisionContext{StateVersion: "v", Mode: "full", View: json.RawMessage(`{"original":true}`)}
		if err := UseCompletionAssessment(state, d, review); err != nil {
			t.Fatal(err)
		}
		if d.CompletionAssessment != nil || d.Mode != "full" || string(d.View) != `{"original":true}` {
			t.Fatal("oversized feedback authorized the completion fast path")
		}
	}
}
