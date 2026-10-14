package sqlstore

import "pwnmesh/internal/board/model"

const executionColumns = `project_id,id,namespace,backend,kind,intent,lease,job,retry_key,status,result,resumes,created_at,updated_at`

func scanExecution(s scanner) (model.Execution, error) {
	var e model.Execution
	var job, result []byte
	err := s.Scan(&e.ProjectID, &e.ID, &e.Namespace, &e.Backend, &e.Kind, &e.Intent, &e.Lease, &job, &e.RetryKey, &e.Status, &result, &e.Resumes, &e.CreatedAt, &e.UpdatedAt)
	e.Job, e.Result = job, result
	return e, err
}

func Execution(q Queries, project, id string) (model.Execution, error) {
	return scanExecution(q.QueryRow("SELECT "+executionColumns+" FROM pwnmesh_executions WHERE project_id=? AND id=?", project, id))
}

func ExecutionByLease(q Queries, project, lease, kind string) (model.Execution, error) {
	query := "SELECT " + executionColumns + " FROM pwnmesh_executions WHERE project_id=? AND lease=?"
	args := []any{project, lease}
	if kind != "" {
		query += " AND kind=?"
		args = append(args, kind)
	}
	return scanExecution(q.QueryRow(query, args...))
}

// VisitExecutions retains registration order and bounds memory to one saved
// execution. The callback may archive each row using the same transaction.
func VisitExecutions(q Queries, project string, visit func(model.Execution) error) error {
	rows, err := q.Query("SELECT "+executionColumns+" FROM pwnmesh_executions WHERE project_id=? ORDER BY rowid", project)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanExecution(rows)
		if err != nil {
			return err
		}
		if err := visit(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

func ExecutionRetryKeyRecorded(q Queries, project, namespace, retryKey string) (bool, error) {
	var recorded bool
	err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM pwnmesh_executions WHERE project_id=? AND namespace=? AND retry_key=? AND status IN ('prepared','running','retryable','result_pending','failed','rejected','cancelled','succeeded','retry_requested','retried'))`, project, namespace, retryKey).Scan(&recorded)
	return recorded, err
}

// InsertExecution persists Board's validated registration and consumes its
// selected grant. The caller owns the surrounding transaction and any review
// transitions; storage never commits independently or grants retry authority.
func InsertExecution(q Queries, e model.Execution, metadata model.ExecutionSummary, previousID string) error {
	if previousID != "" {
		if _, err := q.Exec("UPDATE pwnmesh_executions SET status='retried',updated_at=? WHERE project_id=? AND id=? AND status='retry_requested'", e.CreatedAt, e.ProjectID, previousID); err != nil {
			return err
		}
	}
	_, err := q.Exec(`INSERT INTO pwnmesh_executions(`+executionColumns+`,`+executionMetadataColumns+`,metadata_version) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)`,
		e.ProjectID, e.ID, e.Namespace, e.Backend, e.Kind, e.Intent, e.Lease, []byte(e.Job), e.RetryKey, e.Status, []byte(e.Result), e.Resumes, e.CreatedAt, e.UpdatedAt,
		metadata.Generation, metadata.InputRevision, metadata.DecisionRevision, metadata.StateVersion, metadata.HasState, metadata.FactCount, metadata.HintCount, metadata.OpenCount)
	return err
}

// SaveExecutionOutcome updates only the mutable outcome. Board chooses the
// transition, retained result and revocation; immutable input stays untouched.
func SaveExecutionOutcome(q Queries, e model.Execution, revoke bool) error {
	_, err := q.Exec(`UPDATE pwnmesh_executions SET status=?,result=?,updated_at=? WHERE project_id=? AND id=?`, e.Status, []byte(e.Result), e.UpdatedAt, e.ProjectID, e.ID)
	if err == nil && revoke {
		_, err = q.Exec("INSERT OR IGNORE INTO pwnmesh_revoked_runs(project_id,worker) VALUES(?,?)", e.ProjectID, e.Lease)
	}
	return err
}

func AddExecutionResume(q Queries, project, id, now string, delta int) error {
	_, err := q.Exec(`UPDATE pwnmesh_executions SET resumes=resumes+?,updated_at=? WHERE project_id=? AND id=?`, delta, now, project, id)
	return err
}

func LatestExecutionJob(q Queries, project, lease, kind string) ([]byte, error) {
	var raw []byte
	err := q.QueryRow("SELECT job FROM pwnmesh_executions WHERE project_id=? AND lease=? AND kind=? ORDER BY rowid DESC LIMIT 1", project, lease, kind).Scan(&raw)
	return raw, err
}

func CreatedDirectionCount(q Queries, project, creator string) (int, error) {
	var count int
	err := q.QueryRow("SELECT COUNT(*) FROM intents WHERE project_id=? AND creator=? AND (to_fact_id IS NULL OR to_fact_id!='goal')", project, creator).Scan(&count)
	return count, err
}
