package model

import (
	"encoding/json"
	"errors"
	"sort"
)

// DecisionContext is bound to one immutable State. A successful decision may
// acknowledge only this generation and revision, never a later live snapshot.
type DecisionContext struct {
	Version              int               `json:"version"`
	StateVersion         string            `json:"state_version"`
	View                 json.RawMessage   `json:"view"`
	Mode                 string            `json:"mode"`
	Fallback             string            `json:"fallback,omitempty"`
	FromRevision         int64             `json:"from_revision"`
	ToRevision           int64             `json:"to_revision"`
	Generation           int64             `json:"generation"`
	BaselineBytes        int               `json:"baseline_bytes"`
	CompletionAssessment *CompletionReview `json:"completion_assessment,omitempty"`
	ClosureProtocol      int               `json:"closure_protocol,omitempty"`
}

type DecisionChanges map[string][]string

type DecisionView struct {
	Version          int               `json:"version"`
	Revision         int64             `json:"revision"`
	DecisionRevision int64             `json:"decision_revision"`
	Generation       int64             `json:"generation"`
	FromRevision     int64             `json:"from_revision"`
	Project          Project           `json:"project"`
	UserInputs       []Fact            `json:"user_inputs"`
	Hints            []Hint            `json:"hints"`
	Goals            []Goal            `json:"goals"`
	Steps            []Step            `json:"steps"`
	Facts            []FactRecord      `json:"fact_records"`
	Findings         []Finding         `json:"findings"`
	Relations        []FactRelation    `json:"fact_relations"`
	Candidates       []CandidateView   `json:"candidates,omitempty"`
	Disputes         []Dispute         `json:"disputes,omitempty"`
	Curation         *CurationProgress `json:"curation,omitempty"`
	Changed          DecisionChanges   `json:"changed"`
	Removed          DecisionChanges   `json:"removed"`
	Omitted          map[string]int    `json:"omitted"`
	ReadMore         string            `json:"read_more"`
	Overview         *ContextOverview  `json:"overview"`
}

// Build the view from the current FGS and the cursor's event index.
func buildDecisionChanges(current State, baseline json.RawMessage, result *DecisionContext, changed DecisionChanges, ordered []StateEvent, maxBytes int) (*DecisionContext, error) {
	current = current.WithLegacyDefaults()
	project := current.Graph.Project
	project.Reason = nil
	project.Curator = nil
	facts := decisionIndex(current.FactRecords, func(v FactRecord) string { return v.ID })
	goals := decisionIndex(current.Goals, func(v Goal) string { return v.ID })
	steps := decisionIndex(current.Steps, func(v Step) string { return v.ID })
	findings := decisionIndex(current.Findings, func(v Finding) string { return v.ID })
	activeCandidates := current.ActiveCandidates()
	candidates := decisionIndex(activeCandidates, func(v Candidate) string { return v.ID })
	fallback := func(reason string) (*DecisionContext, error) {
		view, err := decisionFallbackView(current, baseline, changed, maxBytes)
		if err != nil {
			return nil, err
		}
		result.View, result.Fallback = view, reason
		return result, nil
	}
	selectedFacts, selectedGoals, selectedSteps, selectedFindings := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	selectedCandidates, selectedDisputes := map[string]bool{}, map[string]bool{}
	for _, id := range changed["fact_records"] {
		selectedFacts[id] = true
	}
	for _, id := range changed["goals"] {
		selectedGoals[id] = true
	}
	for _, id := range changed["steps"] {
		selectedSteps[id] = true
	}
	for _, id := range changed["findings"] {
		selectedFindings[id] = true
	}
	for _, id := range changed["candidates"] {
		if _, active := candidates[id]; active {
			selectedCandidates[id] = true
		}
	}
	for _, id := range changed["disputes"] {
		selectedDisputes[id] = true
	}
	// Tentative notes need not pass through Curate to reach the next Decide.
	// Keep their original support and unresolved disagreements alongside them.
	for _, candidate := range activeCandidates {
		if candidate.Revision > current.Curation.ThroughRevision {
			selectedCandidates[candidate.ID] = true
		}
	}
	for _, dispute := range current.Disputes {
		if dispute.Status != "resolved" {
			selectedDisputes[dispute.ID] = true
		}
		if selectedDisputes[dispute.ID] {
			selectedFindings[dispute.FindingID] = true
			for _, id := range dispute.CandidateIDs {
				if _, active := candidates[id]; active {
					selectedCandidates[id] = true
				}
			}
			for _, id := range dispute.ReviewFactIDs {
				selectedFacts[id] = true
			}
		}
	}
	for id := range selectedCandidates {
		candidate := candidates[id]
		for _, source := range candidate.Sources {
			selectedFacts[source] = true
		}
		if candidate.SourceStepID != "" {
			selectedSteps[candidate.SourceStepID] = true
		}
	}
	for _, event := range ordered {
		if event.ID != "" {
			changed["event_nodes"] = append(changed["event_nodes"], event.ID)
		}
		if _, ok := facts[event.ID]; ok {
			selectedFacts[event.ID] = true
		}
		if _, ok := goals[event.ID]; ok {
			selectedGoals[event.ID] = true
		}
		if _, ok := steps[event.ID]; ok {
			selectedSteps[event.ID] = true
		}
		if _, ok := findings[event.ID]; ok {
			selectedFindings[event.ID] = true
		}
	}
	if ids := changed["event_nodes"]; len(ids) > 0 {
		sort.Strings(ids)
		unique := ids[:0]
		for _, id := range ids {
			if len(unique) == 0 || unique[len(unique)-1] != id {
				unique = append(unique, id)
			}
		}
		changed["event_nodes"] = unique
	}
	// First find consumers affected by changed evidence. Do this before adding
	// support sources: a shared origin must not pull in every historical plan.
	for {
		count := len(selectedFacts)
		decisionRelationClosure(current.FactRelations, selectedFacts)
		for _, fact := range current.FactRecords {
			for _, source := range fact.DerivedFrom {
				if selectedFacts[source] {
					selectedFacts[fact.ID] = true
					break
				}
			}
		}
		if len(selectedFacts) == count {
			break
		}
	}
	references := func(ids []string) bool {
		for _, id := range ids {
			if selectedFacts[id] && id != "origin" && id != "goal" {
				return true
			}
		}
		return false
	}
	for _, goal := range current.Goals {
		if references(goal.Sources) {
			selectedGoals[goal.ID] = true
		}
	}
	for _, step := range current.Steps {
		if ActiveStepStatus(step.Status) || selectedGoals[step.GoalID] || references(step.From) {
			selectedSteps[step.ID] = true
		}
	}
	// Directly selected or affected Steps retain their results. A Step reached
	// only as a producer supplies ancestry, not its unrelated sibling outputs.
	resultSteps := make(map[string]bool, len(selectedSteps))
	for id := range selectedSteps {
		resultSteps[id] = true
	}
	producers := FactProducers(facts, steps)
	for _, goal := range current.Goals {
		if goal.Status == "open" {
			selectedGoals[goal.ID] = true
		}
	}
	selectedGoals["goal"] = true
	// Include each selected node's causal support, including producers of
	// process Facts and both ends of refutes/supersedes/narrows chains.
	for {
		count := len(selectedFacts) + len(selectedGoals) + len(selectedSteps) + len(selectedFindings) + len(selectedCandidates)
		for _, candidate := range activeCandidates {
			if references(candidate.Sources) {
				selectedCandidates[candidate.ID] = true
			}
			if selectedCandidates[candidate.ID] {
				for _, source := range candidate.Sources {
					selectedFacts[source] = true
				}
				if candidate.SourceStepID != "" {
					selectedSteps[candidate.SourceStepID] = true
				}
			}
		}
		for _, finding := range current.Findings {
			if references(finding.Sources) {
				selectedFindings[finding.ID] = true
			}
		}
		for id := range selectedFindings {
			for _, source := range findings[id].Sources {
				selectedFacts[source] = true
			}
		}
		for id := range selectedFacts {
			if producer := producers[id]; producer != "" {
				selectedSteps[producer] = true
			}
		}
		for _, step := range current.Steps {
			if selectedSteps[step.ID] {
				if step.GoalID != "" {
					selectedGoals[step.GoalID] = true
				}
				for _, source := range step.From {
					selectedFacts[source] = true
				}
				if resultSteps[step.ID] && step.Result != nil {
					selectedFacts[*step.Result] = true
				}
			}
		}
		for id := range selectedGoals {
			goal := goals[id]
			if goal.ParentID != "" {
				selectedGoals[goal.ParentID] = true
			}
			for _, source := range goal.Sources {
				selectedFacts[source] = true
			}
		}
		factSupportClosure(current.FactRecords, current.FactRelations, selectedFacts)
		if count == len(selectedFacts)+len(selectedGoals)+len(selectedSteps)+len(selectedFindings)+len(selectedCandidates) {
			break
		}
	}

	view := DecisionView{Version: 1, Revision: current.Revision, DecisionRevision: current.DecisionRevision, Generation: project.Generation, FromRevision: result.FromRevision, Project: project, UserInputs: []Fact{}, Hints: append([]Hint{}, current.Graph.Hints...), Goals: []Goal{}, Steps: []Step{}, Facts: []FactRecord{}, Findings: []Finding{}, Relations: []FactRelation{}, Changed: changed, Removed: DecisionChanges{}, Omitted: map[string]int{}, ReadMore: "Read omitted nodes and evidence using read_graph with ids. Omission is not absence; reads are pinned to state_version. Candidate notes are revisable interpretations, not completion proof. Missing or ambiguous producers are not inferred."}
	if current.Graph.Project.OrchestrationVersion == 1 {
		view.Curation = &current.Curation
	}
	for _, id := range changed["candidates"] {
		if _, active := candidates[id]; !active {
			view.Removed["candidates"] = append(view.Removed["candidates"], id)
		}
	}
	// The same bounded discovery index accompanies full and incremental bodies.
	// Keeping the latter narrow must not erase unrelated historical knowledge.
	var overview struct {
		Overview *ContextOverview `json:"overview"`
	}
	if err := json.Unmarshal(baseline, &overview); err != nil {
		return nil, err
	}
	view.Overview = overview.Overview
	var inputIDs map[string]bool
	view.UserInputs, inputIDs = current.UserInputFacts()
	for _, goal := range current.Goals {
		if selectedGoals[goal.ID] {
			view.Goals = append(view.Goals, goal)
		}
	}
	for _, step := range current.Steps {
		if selectedSteps[step.ID] {
			view.Steps = append(view.Steps, step)
		}
	}
	inputRecords := 0
	for _, fact := range current.FactRecords {
		if inputIDs[fact.ID] {
			inputRecords++ // These immutable records are already in user_inputs.
			continue
		}
		if selectedFacts[fact.ID] {
			view.Facts = append(view.Facts, fact)
		}
	}
	for _, finding := range current.Findings {
		if selectedFindings[finding.ID] {
			view.Findings = append(view.Findings, finding)
		}
	}
	for _, candidate := range activeCandidates {
		if selectedCandidates[candidate.ID] {
			view.Candidates = append(view.Candidates, current.CandidateView(candidate))
		}
	}
	for _, dispute := range current.Disputes {
		if selectedDisputes[dispute.ID] {
			view.Disputes = append(view.Disputes, dispute)
		}
	}
	for _, relation := range current.FactRelations {
		if selectedFacts[relation.Source] || selectedFacts[relation.Target] {
			view.Relations = append(view.Relations, relation)
		}
	}
	view.Omitted["goals"] = len(current.Goals) - len(view.Goals)
	view.Omitted["steps"] = len(current.Steps) - len(view.Steps)
	view.Omitted["fact_records"] = len(current.FactRecords) - len(view.Facts) - inputRecords
	view.Omitted["findings"] = len(current.Findings) - len(view.Findings)
	view.Omitted["fact_relations"] = len(current.FactRelations) - len(view.Relations)
	if current.Graph.Project.OrchestrationVersion == 1 {
		view.Omitted["candidates"] = len(activeCandidates) - len(view.Candidates)
		view.Omitted["disputes"] = len(current.Disputes) - len(view.Disputes)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBytes {
		return fallback("related_context_over_budget")
	}
	result.View, result.Mode = raw, "changes"
	return result, nil
}

func decisionIndex[T any](items []T, key func(T) string) map[string]T {
	out := make(map[string]T, len(items))
	for _, item := range items {
		out[key(item)] = item
	}
	return out
}

func decisionRelationClosure(relations []FactRelation, selected map[string]bool) {
	for {
		count := len(selected)
		for _, relation := range relations {
			if selected[relation.Source] || selected[relation.Target] {
				selected[relation.Source], selected[relation.Target] = true, true
			}
		}
		if count == len(selected) {
			return
		}
	}
}

// Changes are also an index, so large revisions must not force an unbounded
// payload. Current nodes remain discoverable through the overview pages.
func decisionFallbackView(state State, baseline json.RawMessage, changed DecisionChanges, maxBytes int) (json.RawMessage, error) {
	metadata := struct {
		Changed        DecisionChanges `json:"changed"`
		Removed        DecisionChanges `json:"removed"`
		ChangedOmitted map[string]int  `json:"changed_omitted"`
		RemovedOmitted map[string]int  `json:"removed_omitted"`
		ReadMore       string          `json:"read_more"`
	}{DecisionChanges{}, DecisionChanges{}, map[string]int{}, map[string]int{}, "Read changed nodes and support using read_graph with ids. Scan overview sections to discover omitted history and changes; missing nodes are not evidence of absence. Reads are pinned to state_version."}
	for kind, ids := range changed {
		metadata.Changed[kind] = []string{}
		metadata.ChangedOmitted[kind] = len(ids)
	}
	index, _ := json.Marshal(metadata)
	used := len(index)
	if used > maxBytes/4 {
		return nil, errors.New("decision context: change index exceeds the budget")
	}
	keys := make([]string, 0, len(changed))
	for kind := range changed {
		keys = append(keys, kind)
	}
	sort.Strings(keys)
	for _, kind := range keys {
		for _, id := range changed[kind] {
			raw, _ := json.Marshal(id)
			extra := len(raw)
			if len(metadata.Changed[kind]) > 0 {
				extra++
			}
			if used+extra > maxBytes/4 {
				continue
			}
			metadata.Changed[kind] = append(metadata.Changed[kind], id)
			metadata.ChangedOmitted[kind]--
			used += extra
		}
	}
	index, _ = json.Marshal(metadata)
	// Merge two nonempty JSON objects by replacing their adjacent braces with
	// a comma. Reserve the bounded index before choosing history bodies.
	extra := len(index) - 1
	if len(baseline)+extra > maxBytes {
		var err error
		baseline, err = ContextView(state, "", maxBytes-extra)
		if err != nil {
			return nil, err
		}
	}
	view := make([]byte, 0, len(baseline)+extra)
	view = append(view, baseline[:len(baseline)-1]...)
	view = append(view, ',')
	view = append(view, index[1:]...)
	return view, nil
}
