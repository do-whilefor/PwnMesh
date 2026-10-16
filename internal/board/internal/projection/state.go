package projection

import (
	"pwnmesh/internal/board/model"
	"slices"
	"strings"
)

// Build builds the same effective evidence and dependency view for
// persisted reads and a curator's pending atomic batch. Relations never
// rewrite the original observations stored in d.Facts.
func Build(g model.Graph, d Metadata, revision, decision int64, runs []model.ExecutionSummary) model.State {
	s := model.State{Graph: g, Goals: []model.Goal{}, Steps: []model.Step{}, FactRecords: []model.FactRecord{}, Findings: slices.Clone(d.Findings), FactRelations: d.FactRelations, Revision: revision, DecisionRevision: decision, Candidates: d.Candidates, Disputes: d.Disputes, Curation: d.Curation}
	root := model.Goal{ID: "goal", Status: "open", Sources: []string{}, CreatedAt: g.Project.CreatedAt}
	facts := make(map[string]model.FactRecord, len(d.Facts))
	for _, record := range d.Facts {
		if _, exists := facts[record.ID]; !exists {
			facts[record.ID] = record
		}
	}
	statuses := make(map[string]string, len(d.FactRelations))
	for _, relation := range d.FactRelations {
		statuses[relation.Target] = map[string]string{"supersedes": "superseded", "refutes": "refuted", "narrows": "narrowed"}[relation.Kind]
	}
	for _, f := range g.Facts {
		if f.ID == "goal" {
			root.Condition = f.Description
		}
		record := model.FactRecord{ID: f.ID, Description: f.Description, Status: "valid", Evidence: []model.EvidenceRef{}, Legacy: true}
		if f.ID == "origin" || f.ID == "goal" {
			record.Status = "input"
		}
		if existing, ok := facts[f.ID]; ok {
			record = existing
		}
		if status, ok := statuses[f.ID]; ok {
			record.Status = status
		}
		s.FactRecords = append(s.FactRecords, record)
	}
	if g.Project.Status == "completed" {
		root.Status = "achieved"
		for _, i := range g.Intents {
			if model.Value(i.To) == "goal" {
				root.Sources = i.From
			}
		}
	}
	s.Goals = append(s.Goals, root)
	s.Goals = append(s.Goals, d.Goals...)
	latest := make(map[string]model.ExecutionSummary, len(runs))
	currentRuns := make(map[string]bool, len(runs))
	for _, execution := range runs {
		latest[execution.Intent] = execution
		currentRuns[execution.Intent] = execution.Generation == g.Project.Generation
	}
	metadataByID := make(map[string]StepMetadata, len(d.Steps))
	for _, metadata := range d.Steps {
		// Preserve historical duplicate records: the last metadata wins, while
		// any retained abandonment still overrides the runtime projection.
		if metadataByID[metadata.ID].Status == "abandoned" {
			metadata.Status = "abandoned"
		}
		metadataByID[metadata.ID] = metadata
	}
	for _, i := range g.Intents {
		step := model.Step{ID: i.ID, From: i.From, GoalID: "goal", Description: i.Description, Status: "open", Result: i.To, Worker: i.Worker, CreatedAt: i.CreatedAt}
		if i.Worker != nil {
			step.Status = "running"
		}
		if i.To != nil {
			step.Status = "completed"
		}
		if metadata, ok := metadataByID[i.ID]; ok {
			step.Assessment = metadata.Assessment
			step.GoalID, step.Priority, step.Reason = metadata.GoalID, metadata.Priority, metadata.Reason
			step.DisputeID = metadata.DisputeID
			step.Repair = metadata.Repair
			step.DependsOn = append([]string(nil), metadata.DependsOn...)
			step.WritePaths = append([]string(nil), metadata.WritePaths...)
			if metadata.Status == "abandoned" {
				step.Status = "abandoned"
			}
		}
		if execution, ok := latest[i.ID]; ok && i.To == nil && step.Status != "abandoned" && slices.Contains([]string{"failed", "rejected", "cancelled"}, execution.Status) {
			step.Status = "failed"
			step.Reason = "execution " + execution.Status
		}
		if execution, ok := latest[i.ID]; ok && g.Project.OrchestrationVersion == 1 && slices.Contains([]string{"failed", "rejected", "cancelled", "retry_requested"}, execution.Status) {
			step.LatestRunID = execution.ID
		}
		if g.Project.OrchestrationVersion != 1 {
			for _, id := range step.From {
				if s.ValidateFactSources([]string{id}, false) != nil {
					step.InvalidSources = append(step.InvalidSources, id)
				}
			}
			if step.Status == "open" && len(step.InvalidSources) > 0 {
				step.Status = "needs_review"
			}
		}
		s.Steps = append(s.Steps, step)
	}
	if g.Project.OrchestrationVersion == 1 {
		projectStepSupport(&s, latest, currentRuns)
	}
	// Assess conclusions once, after Step dependencies have established which
	// accepted result Facts still have valid support.
	for n := range s.Goals {
		s.Goals[n].SupportValid = s.Goals[n].Status == "achieved" && s.ValidateFactSources(s.Goals[n].Sources, true) == nil
	}
	for n := range s.Findings {
		s.Findings[n].SupportValid = len(s.Findings[n].Sources) > 0 && s.ValidateFactSources(s.Findings[n].Sources, true) == nil
	}
	return s
}

// projectStepSupport derives current support while retaining historical Step
// success and every original observation. Accepted results carry their Step's
// prerequisites; other Facts carry only explicitly declared derivations.
func projectStepSupport(s *model.State, latest map[string]model.ExecutionSummary, currentRuns map[string]bool) {
	steps := map[string]*model.Step{}
	facts := map[string]*model.FactRecord{}
	owners := map[string]string{}
	for n := range s.Steps {
		step := &s.Steps[n]
		steps[step.ID] = step
		if step.Result != nil && *step.Result != "goal" && !s.ExternalFeedbackStep(*step) {
			owners[*step.Result] = step.ID
		}
	}
	for n := range s.FactRecords {
		facts[s.FactRecords[n].ID] = &s.FactRecords[n]
	}
	visited, valid := map[string]bool{}, map[string]bool{}
	derivedVisited, derivedValid := map[string]bool{}, map[string]bool{}
	var supports func(string) bool
	var effective func(string, bool) bool
	var derived func(string) bool
	derived = func(id string) bool {
		if derivedVisited[id] {
			return derivedValid[id]
		}
		derivedVisited[id] = true
		fact := facts[id]
		if fact == nil {
			return false
		}
		for _, source := range fact.DerivedFrom {
			if !effective(source, true) {
				return false
			}
		}
		derivedValid[id] = true
		return true
	}
	effective = func(id string, observation bool) bool {
		fact := facts[id]
		if fact == nil || (fact.Status != "valid" && !(id == "origin" && !observation)) {
			return false
		}
		if observation && (fact.Legacy || len(fact.Evidence) == 0) {
			return false
		}
		if owner := owners[id]; owner != "" {
			return supports(owner)
		}
		return derived(id)
	}
	supports = func(id string) bool {
		if visited[id] {
			return valid[id]
		}
		// A malformed historical cycle fails closed. No recursive state reads
		// or mutation of the stored observation content is needed.
		visited[id] = true
		step := steps[id]
		if step == nil || step.Status != "completed" || step.Result == nil {
			return false
		}
		if *step.Result != "goal" {
			execution := latest[id]
			fact := facts[*step.Result]
			if !currentRuns[id] || execution.Status != "succeeded" || fact == nil || fact.Legacy || fact.Status != "valid" || fact.SourceStepID != id || !runMatches(execution.Lease, fact.RunID) || len(fact.Evidence) == 0 {
				return false
			}
			if !derived(*step.Result) {
				return false
			}
		}
		if len(step.From) == 0 {
			return false
		}
		for _, source := range step.From {
			if !effective(source, false) {
				return false
			}
		}
		for _, upstream := range step.DependsOn {
			if !supports(upstream) {
				return false
			}
		}
		valid[id] = true
		return true
	}
	for n := range s.Steps {
		s.Steps[n].SupportValid = supports(s.Steps[n].ID)
	}
	for id, fact := range facts {
		if owners[id] != "" || len(fact.DerivedFrom) > 0 {
			fact.SupportInvalid = !effective(id, true)
		}
	}
	for n := range s.Steps {
		step := &s.Steps[n]
		step.InvalidSources, step.BlockedBy = nil, nil
		for _, id := range step.From {
			if !effective(id, false) {
				step.InvalidSources = append(step.InvalidSources, id)
			}
		}
		for _, id := range step.DependsOn {
			if !supports(id) {
				step.BlockedBy = append(step.BlockedBy, id)
			}
		}
		if step.Status == "open" || step.Status == "needs_review" {
			if len(step.BlockedBy) > 0 {
				step.Status = "blocked"
			} else if len(step.InvalidSources) > 0 {
				step.Status = "needs_review"
			}
		}
	}
}

func runMatches(identity, id string) bool {
	return identity == id || (id != "" && strings.HasSuffix(identity, "@"+id))
}
