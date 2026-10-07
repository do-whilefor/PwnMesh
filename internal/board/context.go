package board

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
)

const DefaultContextViewBytes = 32 << 10

type contextFact struct {
	FactRecord
	DetailsOmitted bool `json:"details_omitted,omitempty"`
}

// UserInputFacts returns original requirements and their IDs, including reopen
// feedback stored as legacy Facts. These are mandatory input, not history.
func (s State) UserInputFacts() ([]Fact, map[string]bool) {
	s = contextState(s)
	ids := map[string]bool{"origin": true, "goal": true}
	for _, step := range s.Steps {
		if s.externalFeedbackStep(step) {
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

// ContextView creates a bounded model projection. It never changes the
// registered graph snapshot. Original user input and the active step are
// mandatory; omitted history can be obtained through paginated graph reads.
func ContextView(state State, stepID string, maxBytes int) (json.RawMessage, error) {
	return contextView(state, stepID, maxBytes, false)
}

// CurationContextView keeps new candidate judgments and related history while
// retaining the same discovery index, evidence budget and exact read contract.
// Facts have no revision field, so they remain a bounded observation view rather
// than being incorrectly labelled as a complete event delta.
func CurationContextView(state State, maxBytes int) (json.RawMessage, error) {
	return contextView(state, "", maxBytes, true)
}

func contextView(state State, stepID string, maxBytes int, curate bool) (json.RawMessage, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultContextViewBytes
	}
	state = contextState(state)
	activeCandidates := state.ActiveCandidates()
	sort.SliceStable(activeCandidates, func(i, j int) bool { return activeCandidates[i].Revision > activeCandidates[j].Revision })
	view := struct {
		Version       int                    `json:"version"`
		Revision      int64                  `json:"revision"`
		Project       Project                `json:"project"`
		UserInputs    []Fact                 `json:"user_inputs"`
		Hints         []Hint                 `json:"hints"`
		Goals         []Goal                 `json:"goals"`
		Steps         []Step                 `json:"steps"`
		Facts         []contextFact          `json:"fact_records"`
		Findings      []Finding              `json:"findings"`
		Relations     []FactRelation         `json:"fact_relations"`
		Omitted       map[string]int         `json:"omitted"`
		Overview      *contextOverview       `json:"overview,omitempty"`
		Candidates    []contextCandidate     `json:"candidates,omitempty"`
		Disputes      []Dispute              `json:"disputes,omitempty"`
		Curation      *CurationProgress      `json:"curation,omitempty"`
		CurationInput *curationInputCoverage `json:"curation_input,omitempty"`
	}{Version: 1, Revision: state.Revision, Project: state.Graph.Project, UserInputs: []Fact{}, Hints: append([]Hint{}, state.Graph.Hints...), Goals: []Goal{}, Steps: []Step{}, Facts: []contextFact{}, Findings: []Finding{}, Relations: []FactRelation{}, Omitted: map[string]int{"goals": len(state.Goals), "steps": len(state.Steps), "fact_records": len(state.FactRecords), "fact_details": len(state.FactRecords), "findings": len(state.Findings), "fact_relations": len(state.FactRelations)}}
	if state.Graph.Project.OrchestrationVersion == 1 {
		view.Curation = &state.Curation
		view.Omitted["candidates"] = len(activeCandidates)
		if curate {
			view.Omitted["candidates"] = len(state.Candidates)
		}
		view.Omitted["disputes"] = len(state.Disputes)
	}
	var inputIDs map[string]bool
	view.UserInputs, inputIDs = state.UserInputFacts()
	goalByID := map[string]Goal{}
	for _, goal := range state.Goals {
		goalByID[goal.ID] = goal
	}
	relevant := map[string]bool{}
	var curationCandidates map[string]bool
	if curate {
		if request := state.PendingCurationRequest(); request != nil {
			for _, source := range request.Sources {
				relevant[source] = true
			}
		}
		curationCandidates = curationContextCandidates(state)
		view.CurationInput = &curationInputCoverage{ReadMore: "Use supplied candidate judgments and source IDs; global omissions include unrelated history. Read missing relevant candidates only when omitted_candidates > 0. For a candidate marked evidence_omitted, read section:evidence with ids:[candidate ID]. Read missing source facts by ID."}
		for _, candidate := range state.Candidates {
			if curationCandidates[candidate.ID] {
				view.CurationInput.RelevantCandidates++
				view.CurationInput.ProvidedCandidates++ // Reserve maximum encoded digits.
				view.CurationInput.OmittedCandidates++
				view.CurationInput.EvidenceOmitted++
				for _, source := range candidate.Sources {
					relevant[source] = true
				}
			}
		}
	}
	if stepID == "" && !curate {
		for _, candidate := range activeCandidates {
			for _, source := range candidate.Sources {
				relevant[source] = true
			}
		}
		for _, dispute := range state.Disputes {
			if dispute.Status == "resolved" {
				continue
			}
			for _, source := range dispute.ReviewFactIDs {
				relevant[source] = true
			}
			for _, finding := range state.Findings {
				if finding.ID == dispute.FindingID {
					for _, source := range finding.Sources {
						relevant[source] = true
					}
				}
			}
		}
		for _, step := range state.Steps {
			if contextActiveStep(step.Status) {
				for _, id := range step.From {
					relevant[id] = true
				}
				for _, upstream := range state.Steps {
					for _, dependency := range step.DependsOn {
						if upstream.ID == dependency && upstream.Result != nil {
							relevant[*upstream.Result] = true
						}
					}
				}
			}
		}
	}
	selectedGoals := map[string]bool{}
	goalID := "goal"
	if stepID != "" {
		found := false
		for _, step := range state.Steps {
			if step.ID == stepID {
				view.Steps = append(view.Steps, step)
				view.Omitted["steps"]--
				goalID = step.GoalID
				for _, id := range step.From {
					relevant[id] = true
				}
				for _, upstream := range state.Steps {
					for _, dependency := range step.DependsOn {
						if upstream.ID == dependency && upstream.Result != nil {
							relevant[*upstream.Result] = true
						}
					}
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("context view: unknown step %q", stepID)
		}
	}
	for goalID != "" {
		if selectedGoals[goalID] {
			return nil, errors.New("context view: cyclic goal ancestry")
		}
		goal, ok := goalByID[goalID]
		if !ok {
			return nil, fmt.Errorf("context view: missing goal %q", goalID)
		}
		selectedGoals[goalID] = true
		view.Goals = append(view.Goals, goal)
		view.Omitted["goals"]--
		for _, id := range goal.Sources {
			relevant[id] = true
		}
		goalID = goal.ParentID
	}
	if !selectedGoals["goal"] {
		if goal, ok := goalByID["goal"]; ok {
			view.Goals = append(view.Goals, goal)
			selectedGoals["goal"] = true
			view.Omitted["goals"]--
		}
	}
	base, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	// Reserve discovery space before adding evidence bodies. A clean Decide
	// session must see older observations even when they are not in its delta.
	overviewBudget := maxBytes / 3
	if curate {
		// Candidate judgments are the curator's work, not optional history.
		// Keep a smaller discovery index so large collections cannot hide them.
		overviewBudget = maxBytes / 8
	}
	overview, err := buildContextOverview(state, min(overviewBudget, maxBytes-len(base)-16), curationCandidates)
	if err != nil {
		return nil, err
	}
	view.Overview = overview
	base, err = json.Marshal(view)
	if err != nil {
		return nil, err
	}
	used := len(base)
	if used > maxBytes {
		return nil, errors.New("context view: original user inputs, hints, goal ancestry and current step exceed the budget")
	}
	remaining := maxBytes - used
	sectionLimit := used + remaining/2
	if curate {
		sectionLimit = used + remaining*3/4
	}
	// Omitted counts start at their largest value, so updating them never
	// grows the encoded object. Include commas in this conservative accounting.
	add := func(value any, currentCount int) bool {
		raw, err := json.Marshal(value)
		if err != nil {
			return false
		}
		extra := len(raw)
		if currentCount > 0 {
			extra++
		} else {
			switch value.(type) {
			case contextCandidate:
				extra += len(`,"candidates":[]`)
			case Dispute:
				extra += len(`,"disputes":[]`)
			}
		}
		if used+extra > sectionLimit {
			return false
		}
		used += extra
		return true
	}
	if curate {
		// Fit complete judgment fields and source IDs before bulky support or
		// historical disputes. Evidence is restored below only when it fits.
		for _, candidate := range state.Candidates {
			if !curationCandidates[candidate.ID] {
				continue
			}
			projected := contextCandidate{CandidateView: state.CandidateView(candidate), EvidenceOmitted: len(candidate.Evidence) > 0, EvidenceCount: len(candidate.Evidence)}
			projected.Evidence = nil
			if add(projected, len(view.Candidates)) {
				view.Candidates = append(view.Candidates, projected)
				view.Omitted["candidates"]--
				view.CurationInput.OmittedCandidates--
				if !projected.EvidenceOmitted {
					view.CurationInput.EvidenceOmitted--
				}
			}
		}
		view.CurationInput.ProvidedCandidates -= view.CurationInput.OmittedCandidates
	}
	// Open disputes are planning inputs. Keep their full review question and
	// provenance ahead of optional evidence bodies; the index exposes omissions.
	disputes := append([]Dispute{}, state.Disputes...)
	sort.SliceStable(disputes, func(i, j int) bool {
		return disputes[i].Status != "resolved" && disputes[j].Status == "resolved"
	})
	for _, dispute := range disputes {
		if add(dispute, len(view.Disputes)) {
			view.Disputes = append(view.Disputes, dispute)
			view.Omitted["disputes"]--
		}
	}
	for _, candidate := range activeCandidates {
		if curate {
			continue
		}
		projected := contextCandidate{CandidateView: state.CandidateView(candidate), EvidenceOmitted: len(candidate.Evidence) > 0, EvidenceCount: len(candidate.Evidence)}
		projected.Evidence = nil
		if add(projected, len(view.Candidates)) {
			view.Candidates = append(view.Candidates, projected)
			view.Omitted["candidates"]--
		}
	}
	// Bring correction sources near the evidence they supersede/refute.
	decisionRelationClosure(state.FactRelations, relevant)
	facts := append([]FactRecord{}, state.FactRecords...)
	sort.SliceStable(facts, func(i, j int) bool {
		a, b := facts[i], facts[j]
		if relevant[a.ID] != relevant[b.ID] {
			return relevant[a.ID]
		}
		if (a.Status == "valid") != (b.Status == "valid") {
			return a.Status == "valid"
		}
		return false
	})
	selectedFacts := map[string]bool{}
	for _, f := range facts {
		if inputIDs[f.ID] {
			view.Omitted["fact_records"]--
			view.Omitted["fact_details"]--
			continue
		}
		candidate := contextFact{FactRecord: f}
		full := f.Status == "valid" || relevant[f.ID]
		if full && add(candidate, len(view.Facts)) {
			view.Facts = append(view.Facts, candidate)
			view.Omitted["fact_records"]--
			view.Omitted["fact_details"]--
			selectedFacts[f.ID] = true
			continue
		}
		candidate = contextFact{FactRecord: FactRecord{ID: f.ID, Status: f.Status, Legacy: f.Legacy, SupportInvalid: f.SupportInvalid}, DetailsOmitted: true}
		if add(candidate, len(view.Facts)) {
			view.Facts = append(view.Facts, candidate)
			view.Omitted["fact_records"]--
			selectedFacts[f.ID] = true
		}
	}
	if len(view.Candidates) > 0 {
		byID := map[string]Candidate{}
		for _, candidate := range state.Candidates {
			byID[candidate.ID] = candidate
		}
		for i, candidate := range view.Candidates {
			if !candidate.EvidenceOmitted {
				continue
			}
			full := candidate
			full.Evidence, full.EvidenceOmitted = byID[candidate.ID].Evidence, false
			before, _ := json.Marshal(candidate)
			after, _ := json.Marshal(full)
			if extra := len(after) - len(before); used+extra <= sectionLimit {
				used += extra
				view.Candidates[i] = full
				if curate {
					view.CurationInput.EvidenceOmitted--
				}
			}
		}
	}
	// This count concerns supplied candidate bodies only. Entirely omitted
	// records already have their own count, including their unknown support.
	if curate {
		view.CurationInput.EvidenceOmitted -= view.CurationInput.OmittedCandidates
	}
	sectionLimit = min(maxBytes, used+remaining/10)
	for _, relation := range state.FactRelations {
		if (selectedFacts[relation.Source] || selectedFacts[relation.Target]) && add(relation, len(view.Relations)) {
			view.Relations = append(view.Relations, relation)
			view.Omitted["fact_relations"]--
		}
	}
	sectionLimit = min(maxBytes, used+remaining/10)
	for _, goal := range state.Goals {
		if !selectedGoals[goal.ID] && add(goal, len(view.Goals)) {
			view.Goals = append(view.Goals, goal)
			view.Omitted["goals"]--
		}
	}
	sectionLimit = min(maxBytes, used+remaining/5)
	steps := append([]Step{}, state.Steps...)
	sort.SliceStable(steps, func(i, j int) bool {
		return contextActiveStep(steps[i].Status) && !contextActiveStep(steps[j].Status)
	})
	for _, step := range steps {
		if step.ID != stepID && add(step, len(view.Steps)) {
			view.Steps = append(view.Steps, step)
			view.Omitted["steps"]--
		}
	}
	sectionLimit = maxBytes
	findings := append([]Finding{}, state.Findings...)
	sort.SliceStable(findings, func(i, j int) bool {
		related := func(f Finding) bool {
			for _, id := range f.Sources {
				if relevant[id] {
					return true
				}
			}
			return false
		}
		return related(findings[i]) && !related(findings[j])
	})
	for _, finding := range findings {
		if add(finding, len(view.Findings)) {
			view.Findings = append(view.Findings, finding)
			view.Omitted["findings"]--
		}
	}
	result, err := json.Marshal(view)
	if err == nil && len(result) > maxBytes {
		return nil, errors.New("context view exceeded its budget")
	}
	return result, err
}

// Excerpts are literal prefixes, never generated summaries or new evidence.
// Each section is independently discoverable even when no entry fits. Paging
// starts at zero because priority order here differs from storage order.
type contextOverview struct {
	Sections []contextOverviewSection `json:"sections"`
	ReadMore string                   `json:"read_more"`
}

type contextOverviewSection struct {
	Section string                `json:"section"`
	Total   int                   `json:"total"`
	Omitted int                   `json:"omitted"`
	Offset  int                   `json:"offset"`
	Limit   int                   `json:"limit"`
	Items   []contextOverviewNode `json:"items"`
}

type contextOverviewNode struct {
	ID                    string   `json:"id,omitempty"`
	Excerpt               string   `json:"excerpt,omitempty"`
	TextTruncated         bool     `json:"text_truncated,omitempty"`
	Status                string   `json:"status,omitempty"`
	GoalID                string   `json:"goal_id,omitempty"`
	ParentID              string   `json:"parent_id,omitempty"`
	Sources               []string `json:"sources,omitempty"`
	SourcesOmitted        int      `json:"sources_omitted,omitempty"`
	InvalidSources        []string `json:"invalid_sources,omitempty"`
	InvalidSourcesOmitted int      `json:"invalid_sources_omitted,omitempty"`
	BlockedBy             []string `json:"blocked_by,omitempty"`
	BlockedByOmitted      int      `json:"blocked_by_omitted,omitempty"`
	SupportValid          *bool    `json:"support_valid,omitempty"`
	SupportInvalid        bool     `json:"support_invalid,omitempty"`
	Kind                  string   `json:"kind,omitempty"`
	Source                string   `json:"source,omitempty"`
	Target                string   `json:"target,omitempty"`
	DisputeID             string   `json:"dispute_id,omitempty"`
	LatestRunID           string   `json:"latest_run_id,omitempty"`
	GroupKey              string   `json:"group_key,omitempty"`
	Supersedes            string   `json:"supersedes,omitempty"`
	Superseded            bool     `json:"superseded,omitempty"`
}

func buildContextOverview(state State, maxBytes int, curationCandidates map[string]bool) (*contextOverview, error) {
	view := &contextOverview{ReadMore: "Excerpts are discovery aids; use supplied full records and evidence, reading only missing details. Omission is not absence. Use read_graph with section and ids, or scan the section from offset 0 with limit 20 and follow next_offset; the runtime pins reads to state_version."}
	for i, section := range []string{"goals", "steps", "facts", "findings", "relations"} {
		total := []int{len(state.Goals), len(state.Steps), len(state.FactRecords), len(state.Findings), len(state.FactRelations)}[i]
		view.Sections = append(view.Sections, contextOverviewSection{Section: section, Total: total, Omitted: total, Limit: 20, Items: []contextOverviewNode{}})
	}
	if state.Graph.Project.OrchestrationVersion == 1 {
		view.Sections = append(view.Sections, contextOverviewSection{Section: "candidates", Total: len(state.Candidates), Omitted: len(state.Candidates), Limit: 20, Items: []contextOverviewNode{}}, contextOverviewSection{Section: "disputes", Total: len(state.Disputes), Omitted: len(state.Disputes), Limit: 20, Items: []contextOverviewNode{}})
	}
	base, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	if len(base) > maxBytes {
		return nil, errors.New("context view: user requirements and graph discovery index exceed the budget")
	}
	type candidate struct {
		section, priority, ordinal int
		node                       contextOverviewNode
	}
	items := []candidate{}
	add := func(section, priority, ordinal int, node contextOverviewNode, description string, sources []string) {
		node.Excerpt, node.TextTruncated = contextExcerpt(description, 240)
		node.Sources = append([]string(nil), sources[:min(len(sources), 16)]...)
		node.SourcesOmitted = len(sources) - len(node.Sources)
		items = append(items, candidate{section, priority, ordinal, node})
	}
	corrected := map[string]bool{}
	for i, relation := range state.FactRelations {
		corrected[relation.Source], corrected[relation.Target] = true, true
		add(4, 1, i, contextOverviewNode{Kind: relation.Kind, Source: relation.Source, Target: relation.Target}, relation.Reason, nil)
	}
	for i, goal := range state.Goals {
		priority := 4
		if goal.ID == "goal" || goal.Status == "open" {
			priority = 0
		}
		supportValid := goal.SupportValid
		add(0, priority, i, contextOverviewNode{ID: goal.ID, Status: goal.Status, ParentID: goal.ParentID, SupportValid: &supportValid}, goal.Condition, goal.Sources)
	}
	for i, step := range state.Steps {
		priority := 5
		if contextActiveStep(step.Status) {
			priority = 0
		}
		invalid := append([]string(nil), step.InvalidSources[:min(len(step.InvalidSources), 16)]...)
		blocked := append([]string(nil), step.BlockedBy[:min(len(step.BlockedBy), 16)]...)
		var support *bool
		if state.Graph.Project.OrchestrationVersion == 1 {
			valid := step.SupportValid
			support = &valid
		}
		add(1, priority, i, contextOverviewNode{ID: step.ID, Status: step.Status, GoalID: step.GoalID, InvalidSources: invalid, InvalidSourcesOmitted: len(step.InvalidSources) - len(invalid), BlockedBy: blocked, BlockedByOmitted: len(step.BlockedBy) - len(blocked), SupportValid: support, LatestRunID: step.LatestRunID}, step.Description, step.From)
	}
	for i, fact := range state.FactRecords {
		priority := 3
		if corrected[fact.ID] {
			priority = 1
		}
		add(2, priority, i, contextOverviewNode{ID: fact.ID, Status: fact.Status, SupportInvalid: fact.SupportInvalid}, fact.Description, nil)
	}
	for i, finding := range state.Findings {
		priority := 3
		if !finding.SupportValid {
			priority = 1
		}
		supportValid := finding.SupportValid
		add(3, priority, i, contextOverviewNode{ID: finding.ID, Status: finding.Status, SupportValid: &supportValid}, finding.Claim, finding.Sources)
	}
	if state.Graph.Project.OrchestrationVersion == 1 {
		active := map[string]bool{}
		for _, candidate := range state.ActiveCandidates() {
			active[candidate.ID] = true
		}
		for i, c := range state.Candidates {
			projected := state.CandidateView(c)
			priority := 6
			if active[c.ID] {
				priority = 2
			}
			if curationCandidates[c.ID] {
				priority = -1
			}
			add(5, priority, i, contextOverviewNode{ID: c.ID, Status: c.Status, GroupKey: projected.GroupKey, SupportValid: projected.SupportValid, Supersedes: c.Supersedes, Superseded: !active[c.ID]}, c.Claim, c.Sources)
		}
		for i, d := range state.Disputes {
			add(6, 0, i, contextOverviewNode{ID: d.ID, Status: d.Status, DisputeID: d.ID}, d.Question, d.CandidateIDs)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].priority != items[j].priority {
			return items[i].priority < items[j].priority
		}
		// Interleave facts and findings instead of letting a large collection
		// hide the existence of the other kind.
		return items[i].ordinal < items[j].ordinal
	})
	used := len(base)
	for _, item := range items {
		raw, err := json.Marshal(item.node)
		if err != nil {
			return nil, err
		}
		section := &view.Sections[item.section]
		extra := len(raw)
		if len(section.Items) > 0 {
			extra++
		}
		if used+extra > maxBytes {
			continue
		}
		section.Items = append(section.Items, item.node)
		section.Omitted--
		used += extra // Decreasing omitted counts cannot increase encoded size.
	}
	return view, nil
}

func contextExcerpt(text string, maxBytes int) (string, bool) {
	if len(text) <= maxBytes {
		return text, false
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end], true
}

func contextActiveStep(status string) bool {
	return status == "open" || status == "running" || status == "needs_review" || status == "blocked"
}

// Legacy jobs contain Graph only. Project the same meaning without guessing
// new provenance or changing those jobs' immutable registration snapshots.
func contextState(s State) State {
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
