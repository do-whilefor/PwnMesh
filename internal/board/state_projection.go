package board

import (
	"database/sql"
	"errors"
	"pwnmesh/internal/board/internal/projection"
	"pwnmesh/internal/board/internal/sqlstore"
)

func (t *Tx) stateData(project string) (stateData, int64, int64, error) {
	raw, revision, decision, err := sqlstore.StateMetadata(t.transactionQueries, project)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return stateData{}, revision, decision, err
	}
	data, err := projection.DecodeMetadata(raw)
	return data, revision, decision, err
}

func (t *Tx) State(project string) (State, error) {
	g, err := t.Load(project)
	if err != nil {
		return State{}, err
	}
	d, revision, decision, err := t.stateData(project)
	if err != nil {
		return State{}, err
	}
	return t.projectState(g, d, revision, decision)
}

// projectionInputs is local to one transaction and project generation. Pure
// prospective projections may reuse it while only metadata changes.
type projectionInputs struct {
	runs           []ExecutionSummary
	assets         []Asset
	anchors        []AssetAnchor
	failureReasons map[string]string
}

func (t *Tx) loadProjectionInputs(g Graph) (projectionInputs, error) {
	runs, err := sqlstore.LatestStepExecutions(t.transactionQueries, g.Project.ID)
	if err != nil {
		return projectionInputs{}, err
	}
	assets, anchors, err := sqlstore.ProjectAssets(t.transactionQueries, g.Project.ID, g.Project.Generation)
	return projectionInputs{runs: runs, assets: assets, anchors: anchors}, err
}

func (inputs projectionInputs) build(g Graph, d stateData, revision, decision int64) State {
	s := projection.Build(g, d, revision, decision, inputs.runs)
	for n := range s.Steps {
		if reason, ok := inputs.failureReasons[s.Steps[n].ID]; ok && s.Steps[n].Status == "failed" {
			s.Steps[n].Reason = reason
		}
	}
	return projection.WithAssets(s, inputs.assets, inputs.anchors)
}

// Project pending and persisted metadata using the same transaction snapshot.
func (t *Tx) projectState(g Graph, d stateData, revision, decision int64) (State, error) {
	inputs, err := t.loadProjectionInputs(g)
	if err != nil {
		return State{}, err
	}
	s := inputs.build(g, d, revision, decision)
	leases := make(map[string]string, len(inputs.runs))
	for _, run := range inputs.runs {
		leases[run.Intent] = run.Lease
	}
	failedLeases := []string{}
	for _, step := range s.Steps {
		if step.Status == "failed" {
			failedLeases = append(failedLeases, leases[step.ID])
		}
	}
	reasons, err := sqlstore.ExecutionFailureReasons(t.transactionQueries, g.Project.ID, failedLeases)
	if err != nil {
		return State{}, err
	}
	for n := range s.Steps {
		step := &s.Steps[n]
		if detail := reasons[leases[step.ID]]; step.Status == "failed" && detail != "" {
			step.Reason = detail
		}
	}
	return s, nil
}

func (t *Tx) StepAvailable(project, id string) error {
	steps, err := t.stepMetadataByID(project, id)
	if err != nil {
		return err
	}
	for _, step := range steps {
		if step.ID == id && step.Status == "abandoned" {
			return Err(409, "Step was abandoned")
		}
	}
	return nil
}

// ValidateStateCompletion adds checks only when extended state is present;
// legacy clients without FGS metadata retain their original contract.
func (t *Tx) ValidateStateCompletion(project string, from []string) error {
	current, loadErr := t.State(project)
	if loadErr != nil {
		return loadErr
	}
	if current.Graph.Project.OrchestrationVersion == 1 {
		for _, step := range current.Steps {
			if step.Status == "blocked" || (step.Status == "completed" && !step.SupportValid && Value(step.Result) != "goal" && !current.ExternalFeedbackStep(step)) {
				return Err(409, "Project has a blocked or unsupported Step "+step.ID)
			}
		}
		if err := current.ValidateCompletionDisputes(from); err != nil {
			return err
		}
		if current.Graph.Project.Curator != nil {
			return Err(409, "Project has an active curator")
		}
	}
	d, _, _, err := t.stateData(project)
	if err != nil || (len(d.Facts) == 0 && len(d.Goals) == 0 && len(d.Steps) == 0 && len(d.Findings) == 0 && len(d.FactRelations) == 0) {
		return err
	}
	if err = current.ValidateFactSources(from, true); err != nil {
		return err
	}
	for _, goal := range current.Goals {
		if goal.ID != "goal" && (goal.Status == "open" || (goal.Status == "achieved" && !goal.SupportValid)) {
			return Err(409, "Project has an unresolved child goal")
		}
	}
	return nil
}
