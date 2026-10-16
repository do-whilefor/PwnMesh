package board

import (
	"database/sql"
	"errors"
	"pwnmesh/internal/board/internal/sqlstore"
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
func (t *Tx) RequestAutomaticDecisionRetry(e Execution) (err error) {
	current, failure, err := sqlstore.ExecutionSummaryWithFailure(t.transactionQueries, e.ProjectID, e.ID)
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
	if !automaticDecisionRetryEligible(current.Kind, status, failure.ResultStatus, failure.Kind, failure.Cause) {
		return Err(409, "Decision failure does not allow automatic retry")
	}
	projectStatus, generation, leaseWorker, err := sqlstore.RetryProjectState(t.transactionQueries, current.ProjectID)
	if err != nil {
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
			return CodedErr(409, CodeStateChanged, "state_changed: curation retry input is no longer current")
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
	attempts, err := sqlstore.ExecutionAttempts(t.transactionQueries, current.Namespace, current.ProjectID, current.Kind, current.RetryKey, generation)
	if err != nil {
		return err
	}
	if attempts != 1 {
		return Err(409, "Automatic decision retry allowance exhausted for this input")
	}
	pending, err := sqlstore.OtherDecisionRetryPending(t.transactionQueries, current.ProjectID, current.ID, current.Kind, current.Kind == "curate", current.RetryKey, generation)
	if err != nil {
		return err
	}
	if pending || leaseWorker != nil && *leaseWorker != current.Lease {
		return Err(409, "Another decision is active or authorized")
	}
	// A caller may handle a failed grant and continue its transaction. Keep
	// revocation, lease release and authorization indivisible in that case too.
	if _, err = t.transactionQueries.Exec("SAVEPOINT pwnmesh_management"); err != nil {
		return err
	}
	defer t.finishManagement(&err)
	// Terminal failures are already revoked. Revoke defensively and release
	// only this attempt's lease in the same transaction as its one-use grant.
	if err = sqlstore.RevokeRun(t.transactionQueries, current.ProjectID, current.Lease); err != nil {
		return err
	}
	if leaseWorker != nil {
		if current.Kind == "curate" {
			err = t.ReleaseCurator(current.ProjectID, current.Lease)
		} else {
			err = sqlstore.SaveReasonLease(t.transactionQueries, current.ProjectID, nil)
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
	return sqlstore.SaveExecutionRetryGrant(t.transactionQueries, current.ProjectID, current.ID, t.Now)
}
