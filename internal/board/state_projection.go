package board

import (
	"database/sql"
	"errors"
	"pwnmesh/internal/board/internal/projection"
	"pwnmesh/internal/board/internal/sqlstore"
)

func (t *Tx) stateData(project string) (stateData, int64, int64, error) {
	raw, revision, decision, err := sqlstore.StateMetadata(t, project)
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

// Project pending and persisted metadata using the same transaction snapshot.
func (t *Tx) projectState(g Graph, d stateData, revision, decision int64) (State, error) {
	runs, err := sqlstore.LatestStepExecutions(t, g.Project.ID)
	if err != nil {
		return State{}, err
	}
	s := projection.Build(g, d, revision, decision, runs)
	leases := make(map[string]string, len(runs))
	for _, run := range runs {
		leases[run.Intent] = run.Lease
	}
	for n := range s.Steps {
		step := &s.Steps[n]
		if step.Status != "failed" {
			continue
		}
		detail, err := sqlstore.ExecutionFailureReason(t, g.Project.ID, leases[step.ID])
		if err == nil && detail != "" {
			step.Reason = detail
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return State{}, err
		}
	}
	assets, anchors, err := sqlstore.ProjectAssets(t, g.Project.ID, g.Project.Generation)
	if err != nil {
		return State{}, err
	}
	return projection.WithAssets(s, assets, anchors), nil
}

func (t *Tx) StepAvailable(project, id string) error {
	d, _, _, err := t.stateData(project)
	if err != nil {
		return err
	}
	for _, step := range d.Steps {
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
