package board

import "slices"

// HistoryMember points back to an unchanged record. Kind is step or fact;
// readers expand it through read_graph sections steps or facts with its ID.
type HistoryMember struct {
	Kind           string `json:"kind"`
	ID             string `json:"id"`
	Excerpt        string `json:"excerpt,omitempty"`
	TextTruncated  bool   `json:"text_truncated,omitempty"`
	Status         string `json:"status,omitempty"`
	Scope          string `json:"scope,omitempty"`
	ScopeTruncated bool   `json:"scope_truncated,omitempty"`
	ObservedAt     string `json:"observed_at,omitempty"`
}

type historyNodeKey struct{ Kind, ID string }

// HistoryEntry is a deterministic reading view, not a new observation or
// completion judgment. Summary is a literal prefix of the Step description.
type HistoryEntry struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	Summary       string          `json:"summary"`
	TextTruncated bool            `json:"text_truncated,omitempty"`
	Members       []HistoryMember `json:"members"`
	AssetIDs      []string        `json:"asset_ids,omitempty"`
}

// History groups cold terminal Steps with their observations without changing
// the snapshot. Anything still needed by a goal, judgment, dispute, correction
// or live execution stays individually discoverable, including its ancestry.
func (s State) History() []HistoryEntry {
	s = contextState(s)
	facts := decisionIndex(s.FactRecords, func(f FactRecord) string { return f.ID })
	steps := decisionIndex(s.Steps, func(step Step) string { return step.ID })
	producers := decisionFactProducers(facts, steps)
	outputs := map[string][]string{}
	resultOwners := map[string][]string{}
	for _, step := range s.Steps {
		if id := Value(step.Result); id != "" {
			resultOwners[id] = append(resultOwners[id], step.ID)
		}
	}
	for _, fact := range s.FactRecords {
		if producer := producers[fact.ID]; producer != "" {
			outputs[producer] = append(outputs[producer], fact.ID)
		}
	}
	protected := map[historyNodeKey]bool{}
	var pending []historyNodeKey
	keep := func(kind, id string) {
		member := historyNodeKey{Kind: kind, ID: id}
		if id != "" && !protected[member] {
			protected[member] = true
			pending = append(pending, member)
		}
	}
	keepFacts := func(ids []string) {
		for _, id := range ids {
			keep("fact", id)
		}
	}
	keepFacts([]string{"origin", "goal"})
	for _, fact := range s.FactRecords {
		if fact.Status != "valid" || fact.SupportInvalid {
			keep("fact", fact.ID)
		}
	}
	for _, step := range s.Steps {
		terminal := step.Status == "completed" || step.Status == "abandoned"
		if !terminal || Value(step.Result) == "goal" || s.externalFeedbackStep(step) ||
			len(step.InvalidSources) > 0 || len(step.BlockedBy) > 0 ||
			(s.Graph.Project.OrchestrationVersion == 1 && step.Status == "completed" && !step.SupportValid) {
			keep("step", step.ID)
		}
	}
	for _, goal := range s.Goals {
		keepFacts(goal.Sources)
	}
	// Retain historical candidate support too: a later curator may need it to
	// explain a supersession or disagreement with the currently active note.
	for _, candidate := range s.Candidates {
		keepFacts(candidate.Sources)
		keep("step", candidate.SourceStepID)
	}
	for _, finding := range s.Findings {
		keepFacts(finding.Sources)
	}
	for _, dispute := range s.Disputes {
		if dispute.Status != "resolved" {
			keepFacts(dispute.ReviewFactIDs)
			for _, id := range dispute.ReviewStepIDs {
				keep("step", id)
			}
			for _, step := range s.Steps {
				if step.DisputeID == dispute.ID {
					keep("step", step.ID)
				}
			}
		}
	}
	for _, relation := range s.FactRelations {
		keep("fact", relation.Source)
		keep("fact", relation.Target)
	}
	if request := s.PendingCurationRequest(); request != nil {
		keepFacts(request.Sources)
	}
	// Queue traversal handles transitive dependencies and cycles without
	// repeatedly scanning the whole graph or inferring missing producers.
	for cursor := 0; cursor < len(pending); cursor++ {
		member := pending[cursor]
		if member.Kind == "fact" {
			keep("step", producers[member.ID])
			// Ambiguous legacy ownership is not inferred for membership, but
			// none of its possible producers should disappear from hot work.
			for _, id := range resultOwners[member.ID] {
				keep("step", id)
			}
			continue
		}
		step, exists := steps[member.ID]
		if !exists {
			continue
		}
		keepFacts(step.From)
		keepFacts(outputs[step.ID])
		for _, id := range step.DependsOn {
			keep("step", id)
		}
		for _, id := range step.BlockedBy {
			keep("step", id)
		}
	}
	assets := s.historyAssetIndex()
	history := []HistoryEntry{}
	for _, step := range s.Steps {
		if protected[historyNodeKey{Kind: "step", ID: step.ID}] {
			continue
		}
		entry := HistoryEntry{ID: step.ID, Status: step.Status, Members: []HistoryMember{{Kind: "step", ID: step.ID, Status: step.Status}}}
		entry.Summary, entry.TextTruncated = contextExcerpt(step.Description, 240)
		for _, id := range outputs[step.ID] {
			fact := facts[id]
			member := HistoryMember{Kind: "fact", ID: id, Status: fact.Status, ObservedAt: fact.ObservedAt}
			member.Excerpt, member.TextTruncated = contextExcerpt(fact.Description, 160)
			member.Scope, member.ScopeTruncated = contextExcerpt(fact.Scope, 120)
			entry.Members = append(entry.Members, member)
		}
		for _, member := range entry.Members {
			entry.AssetIDs = append(entry.AssetIDs, assets[historyNodeKey{Kind: member.Kind, ID: member.ID}]...)
		}
		slices.Sort(entry.AssetIDs)
		entry.AssetIDs = slices.Compact(entry.AssetIDs)
		history = append(history, entry)
	}
	return history
}

// Build once per reading view; scanning all anchors per node would make large
// histories quadratic. The index owns its slices and cannot mutate State.
func (s State) historyAssetIndex() map[historyNodeKey][]string {
	index := map[historyNodeKey][]string{}
	for _, anchor := range s.AssetAnchors {
		member := historyNodeKey{Kind: anchor.NodeKind, ID: anchor.NodeID}
		index[member] = append(index[member], anchor.AssetID)
	}
	for member, ids := range index {
		slices.Sort(ids)
		index[member] = slices.Compact(ids)
	}
	return index
}
