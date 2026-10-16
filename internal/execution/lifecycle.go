package execution

import (
	"encoding/json"
	b "pwnmesh/internal/board"
)

func currentExecution(t *b.Tx, project, run string) (b.Execution, error) {
	e, err := t.Execution(project, run)
	if err != nil {
		return b.Execution{}, err
	}
	if err = requireCurrentExecution(e); err != nil {
		return b.Execution{}, err
	}
	return e, nil
}

func leasedExecution(t *b.Tx, project, run string, fence b.ExecutionFence) (b.Execution, error) {
	e, err := currentExecution(t, project, run)
	if err != nil {
		return b.Execution{}, err
	}
	if fence != e.Fence() {
		return b.Execution{}, b.Err(403, "Execution lease mismatch")
	}
	return e, nil
}

// Resume reclaims the registered run through Board's recovery allowance.
func Resume(t *b.Tx, project, run string, fence b.ExecutionFence) (b.Execution, error) {
	e, err := leasedExecution(t, project, run, fence)
	if err != nil {
		return b.Execution{}, err
	}
	if err = t.ResumeExecution(e); err != nil {
		return b.Execution{}, err
	}
	return t.Execution(project, run)
}

// Retry authorizes one new attempt. An execution lease cannot grant a retry.
func Retry(t *b.Tx, project, run string, fence b.ExecutionFence, automatic bool) error {
	if fence.Run != "" {
		return b.Err(403, "retry authorization is a project-management operation")
	}
	if automatic {
		return t.RequestAutomaticDecisionRetry(b.Execution{ProjectID: project, ID: run})
	}
	e, err := currentExecution(t, project, run)
	if err != nil {
		return err
	}
	if e.Status != "failed" && e.Status != "rejected" && e.Status != "cancelled" {
		return b.Err(409, "Only failed, rejected or cancelled executions can be retried")
	}
	if err := t.CheckRepairRetry(e.ProjectID, e.ID); err != nil {
		return err
	}
	g, err := t.Load(e.ProjectID)
	if err != nil {
		return err
	}
	if err = g.RequireActive(); err != nil {
		return err
	}
	if e.Kind != "reason" && e.Kind != "curate" {
		if err = t.StepAvailable(e.ProjectID, e.Intent); err != nil {
			return err
		}
		for _, intent := range g.Intents {
			if intent.ID == e.Intent && (intent.To != nil || intent.ConcludedAt != nil) {
				return b.Err(409, "An ended step cannot authorize a retry")
			}
		}
	}
	return t.ExecutionStatus(e, "retry_requested", e.Result)
}

// ChangeStatus retains pending-result immutability and validates runtime receipts.
func ChangeStatus(t *b.Tx, project, run string, fence b.ExecutionFence, status string, result json.RawMessage) (b.Execution, error) {
	e, err := leasedExecution(t, project, run, fence)
	if err != nil {
		return b.Execution{}, err
	}
	switch status {
	case "running", "retryable", "result_pending", "failed", "cancelled", "rejected":
	default:
		return b.Execution{}, b.Err(422, "Invalid execution status")
	}
	if !e.Pending() {
		return b.Execution{}, b.Err(409, "Execution is terminal")
	}
	if e.Status == "result_pending" && (status == "running" || status == "retryable") {
		return b.Execution{}, b.Err(409, "Pending result cannot return to execution")
	}
	if status == "running" || status == "retryable" || status == "result_pending" {
		g, err := t.Load(e.ProjectID)
		if err != nil {
			return b.Execution{}, err
		}
		if err = g.RequireActive(); err != nil {
			return b.Execution{}, err
		}
		if err = t.CheckExecution(g, e.Fence()); err != nil {
			return b.Execution{}, err
		}
	}
	if status == "result_pending" {
		var rr struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(result, &rr) != nil || rr.Status != "success" {
			return b.Execution{}, b.Err(422, "Pending result must be successful")
		}
		if err := validateRepairResult(e.Job, result); err != nil {
			return b.Execution{}, err
		}
	}
	if err = t.ExecutionStatus(e, status, result); err != nil {
		return b.Execution{}, err
	}
	e, err = t.Execution(e.ProjectID, e.ID)
	return e, err
}
