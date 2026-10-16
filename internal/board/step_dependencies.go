package board

import (
	"encoding/json"
	"slices"

	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/board/internal/sqlstore"
)

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
			return step.ID == id && Value(step.Result) != "goal" && !s.ExternalFeedbackStep(step)
		}) {
			return Err(422, "Step dependency "+id+" must identify an existing Step in this project generation")
		}
	}
	// Dependencies can only point to already-created Steps, including earlier
	// actions in this transaction. Immutable inputs exclude self/forward edges
	// and cycles without permitting arbitrary updates to the dependency graph.
	return nil
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
		run, status, intent, err := sqlstore.ExecutionIdentityByLease(t, s.Graph.Project.ID, fact.RunID)
		if err != nil || status != "succeeded" || intent != upstream {
			return nil, Err(409, "Step dependency "+upstream+" has no successful producing run")
		}
		results = append(results, DependencyResult{StepID: upstream, FactID: fact.ID, RunID: run})
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
	if _, err := checkReviewExecutionScope(s, e); err != nil {
		return Err(409, "dependency_invalidated: "+err.Error())
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
