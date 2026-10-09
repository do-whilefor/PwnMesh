package board

import (
	"encoding/json"
	"pwnmesh/internal/artifactcheck"
	"slices"
)

// DependencyResult freezes the accepted upstream result used by one run.
// RunID is the registered execution ID, not a model-supplied evidence identity.
type DependencyResult struct {
	StepID string `json:"step_id"`
	FactID string `json:"fact_id"`
	RunID  string `json:"run_id"`
}

func validateStepDependencies(s State, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if s.Graph.Project.OrchestrationVersion != 1 {
		return Err(422, "depends_on requires orchestration version 1")
	}
	if len(ids) > 64 {
		return Err(422, "a Step may depend on at most 64 upstream Steps")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return Err(422, "duplicate Step dependency")
		}
		seen[id] = true
		if !slices.ContainsFunc(s.Steps, func(step Step) bool {
			return step.ID == id && Value(step.Result) != "goal" && !s.externalFeedbackStep(step)
		}) {
			return Err(422, "Step dependency "+id+" must identify an existing Step in this project generation")
		}
	}
	// Dependencies can only point to already-created Steps, including earlier
	// actions in this transaction. Immutable inputs exclude self/forward edges
	// and cycles without permitting arbitrary updates to the dependency graph.
	return nil
}

// The compatible reopen endpoint records human feedback as a synthetic
// concluded Intent. It is original input, not a successful Worker execution.
func (s State) externalFeedbackStep(step Step) bool {
	if step.Description != "external_feedback" || step.Result == nil {
		return false
	}
	return slices.ContainsFunc(s.FactRecords, func(fact FactRecord) bool {
		return fact.ID == *step.Result && fact.Legacy && fact.SourceStepID == ""
	})
}

// projectStepSupport derives current support while retaining historical Step
// success and every original observation. Only a Step's accepted result Fact
// carries its task prerequisites; other independently retained observations
// keep their original meaning and can be assessed on their own evidence.
func (s *State) projectStepSupport(latest map[string]Execution, currentRuns map[string]bool) {
	steps := map[string]*Step{}
	facts := map[string]*FactRecord{}
	owners := map[string]string{}
	for n := range s.Steps {
		step := &s.Steps[n]
		steps[step.ID] = step
		if step.Result != nil && *step.Result != "goal" && !s.externalFeedbackStep(*step) {
			owners[*step.Result] = step.ID
		}
	}
	for n := range s.FactRecords {
		facts[s.FactRecords[n].ID] = &s.FactRecords[n]
	}
	visited, valid := map[string]bool{}, map[string]bool{}
	var supports func(string) bool
	var effective func(string, bool) bool
	effective = func(id string, observation bool) bool {
		fact := facts[id]
		if fact == nil || (fact.Status != "valid" && !(id == "origin" && !observation)) {
			return false
		}
		if owner := owners[id]; owner != "" {
			return supports(owner)
		}
		return true
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
	for id, owner := range owners {
		if fact := facts[id]; fact != nil {
			fact.SupportInvalid = !supports(owner)
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

func (t *Tx) StepDependencyResults(project, id string) ([]DependencyResult, error) {
	s, err := t.State(project)
	if err != nil {
		return nil, err
	}
	return t.stepDependencyResults(s, id)
}

func (t *Tx) stepDependencyResults(s State, id string) ([]DependencyResult, error) {
	var target *Step
	for n := range s.Steps {
		if s.Steps[n].ID == id {
			target = &s.Steps[n]
			break
		}
	}
	if target == nil {
		return nil, Err(404, "Step not found")
	}
	results := []DependencyResult{}
	if len(target.DependsOn) == 0 {
		return results, nil
	}
	steps := make(map[string]*Step, len(s.Steps))
	for n := range s.Steps {
		steps[s.Steps[n].ID] = &s.Steps[n]
	}
	facts := make(map[string]FactRecord, len(s.FactRecords))
	for _, fact := range s.FactRecords {
		facts[fact.ID] = fact
	}
	for _, upstream := range target.DependsOn {
		step := steps[upstream]
		if step == nil {
			return nil, Err(409, "Step dependency "+upstream+" is unavailable")
		}
		if !step.SupportValid || step.Result == nil {
			return nil, Err(409, "Step dependency "+upstream+" has no supported successful result")
		}
		fact, exists := facts[*step.Result]
		if !exists {
			return nil, Err(409, "Step dependency "+upstream+" is unavailable")
		}
		// Binding needs runtime identity only, not the upstream Worker's saved
		// input or local DAG result, which can be much larger than this graph.
		var e Execution
		err := t.QueryRow("SELECT id,status,intent FROM xloom_executions WHERE project_id=? AND lease=?", s.Graph.Project.ID, fact.RunID).Scan(&e.ID, &e.Status, &e.Intent)
		if err != nil || e.Status != "succeeded" || e.Intent != upstream {
			return nil, Err(409, "Step dependency "+upstream+" has no successful producing run")
		}
		results = append(results, DependencyResult{StepID: upstream, FactID: fact.ID, RunID: e.ID})
	}
	slices.SortFunc(results, func(a, b DependencyResult) int {
		if a.StepID < b.StepID {
			return -1
		}
		if a.StepID > b.StepID {
			return 1
		}
		return 0
	})
	return results, nil
}

// CheckExecutionDependencies is used at registration, process start, resume
// and final acceptance. A late result cannot silently bind itself to a
// newer upstream run or newly repaired evidence after its input was frozen.
func (t *Tx) CheckExecutionDependencies(e Execution) error {
	g, err := t.Load(e.ProjectID)
	if err != nil {
		return err
	}
	if g.Project.OrchestrationVersion != 1 || controlKind(e.Kind) {
		return nil
	}
	s, err := t.State(e.ProjectID)
	if err != nil {
		return err
	}
	if err = t.stepReadyState(s, e.Intent, ""); err != nil {
		return Err(409, "dependency_invalidated: "+err.Error())
	}
	expected, err := t.stepDependencyResults(s, e.Intent)
	if err != nil {
		return Err(409, "dependency_invalidated: "+err.Error())
	}
	var job struct {
		DependencyResults []DependencyResult  `json:"dependency_results"`
		Repair            *artifactcheck.Spec `json:"repair"`
	}
	if json.Unmarshal(e.Job, &job) != nil || !slices.Equal(expected, job.DependencyResults) {
		return Err(409, "dependency_invalidated: registered upstream results do not match current successful dependencies")
	}
	var repair *artifactcheck.Spec
	for _, step := range s.Steps {
		if step.ID == e.Intent {
			repair = step.Repair
			break
		}
	}
	a, _ := json.Marshal(repair)
	b, _ := json.Marshal(job.Repair)
	if string(a) != string(b) {
		return Err(409, "repair input does not match the authorized Step")
	}
	return nil
}
