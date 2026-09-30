package board

import (
	"database/sql"
	"errors"
	"slices"
)

// Distinguish exhausted infrastructure/time recovery from a refusal, invalid
// plan, configuration error or human stop.
func automaticDecisionRetryEligible(kind, status, resultStatus, failureKind, failureCause string) bool {
	if status != "failed" || resultStatus != "failed" {
		return false
	}
	if kind == "reason" {
		return slices.Contains([]string{"recovery_exhausted", "budget_exhausted", "request_timeout", "transient_infrastructure", "transport", "rate_limit", "unavailable"}, failureKind)
	}
	if kind != "curate" {
		return false
	}
	// Unlike the existing Decide policy, a curator's plain budget/recovery
	// exhaustion is not sufficient: semantic work must not silently repeat.
	if failureKind == "budget_exhausted" || failureKind == "recovery_exhausted" {
		failureKind = failureCause
	}
	return slices.Contains([]string{"request_timeout", "transient_infrastructure", "transport", "rate_limit", "unavailable"}, failureKind)
}

// RequestAutomaticDecisionRetry grants one Decide or infrastructure-failed
// curator successor for unchanged input. The
// existing registry is the durable allowance: a second registered attempt with
// this retry key exhausts it, even after dispatcher restart. Human retry remains
// a separate operation and does not replenish the automatic allowance.
func (t *Tx) RequestAutomaticDecisionRetry(e Execution) error {
	var resultStatus, failureKind, failureCause string
	current, err := scanExecutionSummary(t.QueryRow("SELECT "+executionFailureColumns+","+executionSummaryColumns+" FROM xloom_executions WHERE project_id=? AND id=?", e.ProjectID, e.ID), &resultStatus, &failureKind, &failureCause)
	if errors.Is(err, sql.ErrNoRows) {
		return Err(404, "Execution not found")
	}
	if err != nil {
		return err
	}
	status := current.Status
	if status == "retry_requested" {
		status = "failed" // A lost grant response may be retried.
	}
	if !automaticDecisionRetryEligible(current.Kind, status, resultStatus, failureKind, failureCause) {
		return Err(409, "Decision failure does not allow automatic retry")
	}
	var projectStatus string
	var generation int64
	var leaseWorker *string
	if err = t.QueryRow(`SELECT p.status,COALESCE(round.generation,0),p.reason_worker FROM projects p LEFT JOIN xloom_project_rounds round ON round.project_id=p.id WHERE p.id=?`, current.ProjectID).Scan(&projectStatus, &generation, &leaseWorker); err != nil {
		return err
	}
	if projectStatus != "active" {
		return Err(403, "Project is "+projectStatus)
	}
	if current.Generation != generation {
		return Err(409, "Execution input belongs to a previous project round")
	}
	if current.Kind == "curate" {
		state, err := t.State(current.ProjectID)
		if err != nil {
			return err
		}
		if state.Graph.Project.OrchestrationVersion != 1 || current.RetryKey != CurationRetryKey(state) || current.StateVersion != DecisionStateVersion(state) {
			return Err(409, "state_changed: curation retry input is no longer current")
		}
		receipt, err := t.CurationReceipt(current.ProjectID, current.Lease)
		var apiError *APIError
		if err != nil && (!errors.As(err, &apiError) || apiError.Status != 404) {
			return err
		}
		if receipt.Committed {
			return Err(409, "Committed curation must recover its receipt instead of retrying")
		}
		leaseWorker = nil
		if curator := state.Graph.Project.Curator; curator != nil {
			leaseWorker = &curator.Worker
		}
	}
	var attempts int
	if err = t.QueryRow("SELECT COUNT(*) FROM xloom_executions WHERE project_id=? AND namespace=? AND kind=? AND retry_key=? AND generation=?", current.ProjectID, current.Namespace, current.Kind, current.RetryKey, generation).Scan(&attempts); err != nil {
		return err
	}
	if attempts != 1 {
		return Err(409, "Automatic decision retry allowance exhausted for this input")
	}
	var pending bool
	if err = t.QueryRow("SELECT EXISTS(SELECT 1 FROM xloom_executions WHERE project_id=? AND id!=? AND kind=? AND status IN ('prepared','running','retryable','result_pending','retry_requested') AND (status!='retry_requested' OR ?!='curate' OR (retry_key=? AND generation=?)))", current.ProjectID, current.ID, current.Kind, current.Kind, current.RetryKey, generation).Scan(&pending); err != nil {
		return err
	}
	if pending || leaseWorker != nil && *leaseWorker != current.Lease {
		return Err(409, "Another decision is active or authorized")
	}
	// Terminal failures are already revoked. Revoke defensively and release
	// only this attempt's lease in the same transaction as its one-use grant.
	if _, err = t.Exec("INSERT OR IGNORE INTO xloom_revoked_runs(project_id,worker) VALUES(?,?)", current.ProjectID, current.Lease); err != nil {
		return err
	}
	if leaseWorker != nil {
		if current.Kind == "curate" {
			err = t.ReleaseCurator(current.ProjectID, current.Lease)
		} else {
			_, err = t.Exec("UPDATE projects SET reason_worker=NULL,reason_trigger=NULL,reason_started_at=NULL,reason_last_heartbeat_at=NULL WHERE id=?", current.ProjectID)
		}
		if err != nil {
			return err
		}
	}
	// Eligibility and the grant share this transaction. Preserve the saved Job
	// and Result in place: authorizing a successor never restores the old run.
	if current.Status == "retry_requested" {
		return nil
	}
	_, err = t.Exec("UPDATE xloom_executions SET status='retry_requested',updated_at=? WHERE project_id=? AND id=?", t.Now, current.ProjectID, current.ID)
	return err
}
