package board

import (
	"database/sql"
	"errors"
	"slices"
)

// retryStep authorizes one new business attempt. The failed run remains
// revoked and its lease remains held until the dispatcher finishes cleanup.
// Registration consumes the existing previous_run_id grant transactionally.
func (t *Tx) retryStep(s State, d *stateData, id, run, reason string) (string, any, bool, error) {
	if s.Graph.Project.OrchestrationVersion != 1 {
		return "", nil, false, Err(422, "main-agent retry requires orchestration version 1")
	}
	var step *Step
	for n := range s.Steps {
		if s.Steps[n].ID == id {
			step = &s.Steps[n]
			break
		}
	}
	if step == nil {
		return "", nil, false, Err(404, "Step not found")
	}
	if step.Status == "completed" || step.Status == "abandoned" {
		return "", nil, false, Err(409, "an ended Step cannot authorize a retry")
	}
	for _, intent := range s.Graph.Intents {
		if intent.ID == id && (intent.To != nil || intent.ConcludedAt != nil) {
			return "", nil, false, Err(409, "an ended Step cannot authorize a retry")
		}
	}
	if !slices.ContainsFunc(s.Goals, func(goal Goal) bool { return goal.ID == step.GoalID && goal.Status == "open" }) {
		return "", nil, false, Err(409, "retry requires an open task goal")
	}
	latest, err := scanExecutionSummary(t.QueryRow("SELECT "+executionSummaryColumns+" FROM xloom_executions WHERE project_id=? AND intent=? ORDER BY rowid DESC LIMIT 1", s.Graph.Project.ID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, Err(409, "Step has no execution to retry")
	}
	if err != nil {
		return "", nil, false, err
	}
	if latest.ID != run || latest.Generation != s.Graph.Project.Generation || latest.Kind != "explore" {
		return "", nil, false, Err(409, "latest_run_id must match this Step's latest attempt in the current generation")
	}
	if !slices.Contains([]string{"failed", "rejected", "cancelled", "retry_requested"}, latest.Status) {
		return "", nil, false, Err(409, "only a failed, rejected or cancelled attempt may be retried")
	}
	if err := t.CheckRepairRetry(latest.ProjectID, latest.ID); err != nil {
		return "", nil, false, err
	}
	var pending bool
	if err = t.QueryRow("SELECT EXISTS(SELECT 1 FROM xloom_executions WHERE project_id=? AND intent=? AND id!=? AND status IN ('prepared','running','retryable','result_pending','retry_requested'))", latest.ProjectID, latest.Intent, latest.ID).Scan(&pending); err != nil {
		return "", nil, false, err
	}
	if pending {
		return "", nil, false, Err(409, "another execution of this Step is still pending or authorized; wait for cleanup")
	}
	if err = t.StepReady(s.Graph.Project.ID, id); err != nil {
		return "", nil, false, err
	}
	if step.DisputeID != "" {
		for _, dispute := range s.Disputes {
			if dispute.ID != step.DisputeID {
				continue
			}
			for _, other := range s.Steps {
				if other.ID != id && slices.Contains(dispute.ReviewStepIDs, other.ID) && contextActiveStep(other.Status) {
					return "", nil, false, Err(409, "dispute already has another active independent review")
				}
			}
		}
		if err = t.validateReviewExecution(Execution{ProjectID: latest.ProjectID, Intent: latest.Intent, Lease: latest.Lease}); err != nil {
			return "", nil, false, err
		}
	}
	if latest.Status == "retry_requested" {
		return step.ID, *step, false, nil
	}
	if err = t.ExecutionStatus(Execution{ProjectID: latest.ProjectID, ID: latest.ID}, "retry_requested", nil); err != nil {
		return "", nil, false, err
	}
	for n := range d.Disputes {
		if d.Disputes[n].ID == step.DisputeID {
			d.Disputes[n].Status, d.Disputes[n].UpdatedAt = "pending_review", t.Now
		}
	}
	step.Reason = reason
	step.Status = "open"
	if step.Worker != nil {
		step.Status = "running"
	}
	step.LatestRunID = latest.ID
	for n := range d.Steps {
		if d.Steps[n].ID == id {
			d.Steps[n] = stepMetadataFrom(*step)
			return id, *step, true, nil
		}
	}
	d.Steps = append(d.Steps, stepMetadataFrom(*step))
	return id, *step, true, nil
}

func (t *Tx) CheckRepairRetry(project, run string) error {
	var stale bool
	err := t.QueryRow("SELECT COALESCE(json_extract(result,'$.failure_kind'),'')='repair_stale' FROM xloom_executions WHERE project_id=? AND id=?", project, run).Scan(&stale)
	if err != nil {
		return err
	}
	if stale {
		return Err(409, "stale repair requires a new assessed Step and artifact binding, not a retry of the old input")
	}
	return nil
}
