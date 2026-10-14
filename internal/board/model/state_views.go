package model

import (
	"slices"
	"unicode/utf8"
)

// WithLegacyDefaults projects graph-only jobs without changing their stored snapshot.
func (s State) WithLegacyDefaults() State {
	if len(s.Goals) == 0 {
		root := Goal{ID: "goal", Status: "open", Sources: []string{}}
		for _, f := range s.Graph.Facts {
			if f.ID == "goal" {
				root.Condition = f.Description
			}
		}
		if s.Graph.Project.Status == "completed" {
			root.Status = "achieved"
		}
		s.Goals = []Goal{root}
	}
	if len(s.Steps) == 0 {
		for _, i := range s.Graph.Intents {
			step := Step{ID: i.ID, From: i.From, GoalID: "goal", Description: i.Description, Status: "open", Result: i.To, Worker: i.Worker, CreatedAt: i.CreatedAt}
			if i.Worker != nil {
				step.Status = "running"
			}
			if i.To != nil {
				step.Status = "completed"
			}
			s.Steps = append(s.Steps, step)
		}
	}
	if len(s.FactRecords) == 0 {
		for _, f := range s.Graph.Facts {
			status := "valid"
			if f.ID == "origin" || f.ID == "goal" {
				status = "input"
			}
			s.FactRecords = append(s.FactRecords, FactRecord{ID: f.ID, Description: f.Description, Status: status, Legacy: true, Evidence: []EvidenceRef{}})
		}
	}
	return s
}

// UserInputFacts includes original requirements and legacy reopen feedback.
func (s State) UserInputFacts() ([]Fact, map[string]bool) {
	s = s.WithLegacyDefaults()
	ids := map[string]bool{"origin": true, "goal": true}
	for _, step := range s.Steps {
		if s.ExternalFeedbackStep(step) {
			ids[*step.Result] = true
		}
	}
	inputs := []Fact{}
	for _, fact := range s.Graph.Facts {
		if ids[fact.ID] {
			inputs = append(inputs, fact)
		}
	}
	return inputs, ids
}

// ExternalFeedbackStep identifies human input recorded as a synthetic concluded Intent.
func (s State) ExternalFeedbackStep(step Step) bool {
	if step.Description != "external_feedback" || step.Result == nil {
		return false
	}
	return slices.ContainsFunc(s.FactRecords, func(fact FactRecord) bool {
		return fact.ID == *step.Result && fact.Legacy && fact.SourceStepID == ""
	})
}

// FactProducers preserves explicit provenance and infers only unique legacy results.
func FactProducers(facts map[string]FactRecord, steps map[string]Step) map[string]string {
	producers := make(map[string]string)
	for _, step := range steps {
		id := Value(step.Result)
		fact, exists := facts[id]
		if !exists || id == "origin" || id == "goal" || fact.SourceStepID != "" {
			continue
		}
		if _, seen := producers[id]; seen {
			producers[id] = "" // Multiple legacy candidates are not provenance.
		} else {
			producers[id] = step.ID
		}
	}
	for id, fact := range facts {
		if id == "origin" || id == "goal" || fact.SourceStepID == "" {
			continue
		}
		if _, exists := steps[fact.SourceStepID]; exists {
			producers[id] = fact.SourceStepID
		}
	}
	return producers
}

// ContextExcerpt returns a literal UTF-8 prefix for deterministic reading views.
func ContextExcerpt(text string, maxBytes int) (string, bool) {
	if len(text) <= maxBytes {
		return text, false
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end], true
}
