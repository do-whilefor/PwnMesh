package sqlstore

import (
	"database/sql"
	"errors"
	"strings"

	"pwnmesh/internal/board/model"
)

const ExecutionMetadataColumns = `generation,input_revision,decision_revision,state_version,has_state,fact_count,hint_count,open_count`
const executionSummaryColumns = `project_id,id,namespace,backend,kind,intent,lease,retry_key,status,resumes,created_at,updated_at,` + ExecutionMetadataColumns
const executionFailureColumns = `COALESCE(CASE WHEN json_valid(result) THEN json_extract(result,'$.status') END,''),COALESCE(CASE WHEN json_valid(result) THEN json_extract(result,'$.failure_kind') END,''),COALESCE(CASE WHEN json_valid(result) THEN json_extract(result,'$.failure_cause') END,'')`

type scanner interface{ Scan(...any) error }

func scanExecutionSummary(s scanner, prefix ...any) (model.ExecutionSummary, error) {
	var e model.ExecutionSummary
	args := append(prefix, &e.ProjectID, &e.ID, &e.Namespace, &e.Backend, &e.Kind, &e.Intent, &e.Lease, &e.RetryKey, &e.Status, &e.Resumes, &e.CreatedAt, &e.UpdatedAt,
		&e.Generation, &e.InputRevision, &e.DecisionRevision, &e.StateVersion, &e.HasState, &e.FactCount, &e.HintCount, &e.OpenCount)
	err := s.Scan(args...)
	return e, err
}

// Execution reads use the caller's transaction and return persisted facts.
// Board chooses scope and interprets admission, retry and missing-row policy.
func PendingExecutions(q Queries, namespace string, after int64, limit int) (model.ExecutionPage, error) {
	page := model.ExecutionPage{Items: []model.ExecutionSummary{}}
	rows, err := q.Query("SELECT rowid,"+executionSummaryColumns+" FROM pwnmesh_executions WHERE namespace=? AND rowid>? AND "+PendingExecutionSQL+" ORDER BY rowid LIMIT ?", namespace, after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		var rowid int64
		e, err := scanExecutionSummary(rows, &rowid)
		if err != nil {
			return page, err
		}
		if len(page.Items) == limit {
			page.NextCursor = last
			break
		}
		page.Items = append(page.Items, e)
		last = rowid
	}
	return page, rows.Err()
}

func ExecutionSummary(q Queries, namespace, project, id string) (model.ExecutionSummary, error) {
	return scanExecutionSummary(q.QueryRow("SELECT "+executionSummaryColumns+" FROM pwnmesh_executions WHERE namespace=? AND project_id=? AND id=?", namespace, project, id))
}

// LatestIntentExecution follows registration order across all namespaces and
// generations. Its caller validates whether that attempt can authorize a retry.
func LatestIntentExecution(q Queries, project, intent string) (model.ExecutionSummary, error) {
	return scanExecutionSummary(q.QueryRow("SELECT "+executionSummaryColumns+" FROM pwnmesh_executions WHERE project_id=? AND intent=? ORDER BY rowid DESC LIMIT 1", project, intent))
}

func LatestSucceededExecution(q Queries, namespace, project, kind string, generation int64) (model.ExecutionSummary, error) {
	return scanExecutionSummary(q.QueryRow("SELECT "+executionSummaryColumns+" FROM pwnmesh_executions WHERE namespace=? AND project_id=? AND generation=? AND kind=? AND status='succeeded' ORDER BY created_at DESC,rowid DESC LIMIT 1", namespace, project, generation, kind))
}

type ExecutionCandidate struct {
	Kind, Intent, RetryKey string
}

type ExecutionAdmissionFilter struct {
	ProjectID, Namespace string
	ExecutionCandidate
	Generation        int64
	AnyPendingIntent  bool
	GrantMatchesInput bool
}

type ExecutionAdmission struct {
	Pending, Tried bool
	PreviousRunID  string
}

func ReadExecutionAdmission(q Queries, f ExecutionAdmissionFilter) (ExecutionAdmission, error) {
	var out ExecutionAdmission
	err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM pwnmesh_executions WHERE namespace=? AND project_id=? AND kind=? AND (? OR intent=?) AND `+PendingExecutionSQL+`)`, f.Namespace, f.ProjectID, f.Kind, f.AnyPendingIntent, f.Intent).Scan(&out.Pending)
	if err != nil {
		return out, err
	}
	err = q.QueryRow(`SELECT id FROM pwnmesh_executions WHERE namespace=? AND project_id=? AND kind=? AND intent=? AND status='retry_requested' AND (NOT ? OR (retry_key=? AND generation=?)) ORDER BY created_at DESC,rowid DESC LIMIT 1`, f.Namespace, f.ProjectID, f.Kind, f.Intent, f.GrantMatchesInput, f.RetryKey, f.Generation).Scan(&out.PreviousRunID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	err = q.QueryRow(`SELECT EXISTS(SELECT 1 FROM pwnmesh_executions WHERE namespace=? AND project_id=? AND kind=? AND retry_key=? AND status NOT IN ('retry_requested','retried'))`, f.Namespace, f.ProjectID, f.Kind, f.RetryKey).Scan(&out.Tried)
	return out, err
}

func ExecutionAttempts(q Queries, namespace, project, kind, retryKey string, generation int64) (int, error) {
	var count int
	err := q.QueryRow(`SELECT COUNT(*) FROM pwnmesh_executions WHERE namespace=? AND project_id=? AND kind=? AND generation=? AND retry_key=?`, namespace, project, kind, generation, retryKey).Scan(&count)
	return count, err
}

func ExecutionStateExists(q Queries, namespace, project, kind, stateVersion string) (bool, error) {
	var exists bool
	err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM pwnmesh_executions WHERE namespace=? AND project_id=? AND kind=? AND state_version=?)`, namespace, project, kind, stateVersion).Scan(&exists)
	return exists, err
}

// Only the small classification fields are extracted from Result. Neither
// admission nor retry classification materializes a saved Job or Result blob.
type ExecutionFailure struct {
	ResultStatus, Kind, Cause string
}

func ExecutionSummaryWithFailure(q Queries, project, id string) (model.ExecutionSummary, ExecutionFailure, error) {
	var failure ExecutionFailure
	e, err := scanExecutionSummary(q.QueryRow("SELECT "+executionFailureColumns+","+executionSummaryColumns+" FROM pwnmesh_executions WHERE project_id=? AND id=?", project, id), &failure.ResultStatus, &failure.Kind, &failure.Cause)
	return e, failure, err
}

func ExecutionInputFailure(q Queries, namespace, project, kind, retryKey string, generation int64) (id, status string, failure ExecutionFailure, err error) {
	err = q.QueryRow(`SELECT id,status,`+executionFailureColumns+` FROM pwnmesh_executions WHERE namespace=? AND project_id=? AND generation=? AND kind=? AND retry_key=? LIMIT 1`, namespace, project, generation, kind, retryKey).Scan(&id, &status, &failure.ResultStatus, &failure.Kind, &failure.Cause)
	return
}

func ReadExecutionAdmissions(q Queries, project, namespace string, candidates []ExecutionCandidate) (map[string]ExecutionAdmission, error) {
	admissions := make(map[string]ExecutionAdmission, len(candidates))
	if len(candidates) == 0 {
		return admissions, nil
	}
	values := make([]string, 0, len(candidates))
	args := make([]any, 0, 3*len(candidates)+6)
	for _, candidate := range candidates {
		values = append(values, "(?,?,?)")
		args = append(args, candidate.Kind, candidate.Intent, candidate.RetryKey)
	}
	args = append(args, namespace, project, namespace, project, namespace, project)
	rows, err := q.Query(`WITH candidates(kind,intent,retry_key) AS (VALUES `+strings.Join(values, ",")+`)
	SELECT c.retry_key,
	EXISTS(SELECT 1 FROM pwnmesh_executions e WHERE e.namespace=? AND e.project_id=? AND e.kind=c.kind AND e.intent=c.intent AND `+PendingExecutionSQL+`),
	COALESCE((SELECT e.id FROM pwnmesh_executions e WHERE e.namespace=? AND e.project_id=? AND e.kind=c.kind AND e.intent=c.intent AND e.status='retry_requested' ORDER BY e.created_at DESC,e.rowid DESC LIMIT 1),''),
	EXISTS(SELECT 1 FROM pwnmesh_executions e WHERE e.namespace=? AND e.project_id=? AND e.kind=c.kind AND e.retry_key=c.retry_key AND e.status NOT IN ('retry_requested','retried'))
	FROM candidates c`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var admission ExecutionAdmission
		if err := rows.Scan(&key, &admission.Pending, &admission.PreviousRunID, &admission.Tried); err != nil {
			return nil, err
		}
		admissions[key] = admission
	}
	return admissions, rows.Err()
}
