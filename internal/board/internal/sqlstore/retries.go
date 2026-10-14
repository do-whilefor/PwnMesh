package sqlstore

func RetryProjectState(q Queries, project string) (status string, generation int64, reasonWorker *string, err error) {
	err = q.QueryRow(`SELECT p.status,COALESCE(round.generation,0),p.reason_worker FROM projects p LEFT JOIN pwnmesh_project_rounds round ON round.project_id=p.id WHERE p.id=?`, project).Scan(&status, &generation, &reasonWorker)
	return
}

// Board decides whether outstanding grants must match the current input.
// Pending executions always retain priority across dispatcher namespaces.
func OtherDecisionRetryPending(q Queries, project, id, kind string, matchGrantInput bool, retryKey string, generation int64) (pending bool, err error) {
	err = q.QueryRow(`SELECT EXISTS(SELECT 1 FROM pwnmesh_executions WHERE project_id=? AND id!=? AND kind=? AND status IN ('prepared','running','retryable','result_pending','retry_requested') AND (status!='retry_requested' OR NOT ? OR (retry_key=? AND generation=?)))`, project, id, kind, matchGrantInput, retryKey, generation).Scan(&pending)
	return
}

func OtherStepRetryPending(q Queries, project, intent, id string) (pending bool, err error) {
	err = q.QueryRow("SELECT EXISTS(SELECT 1 FROM pwnmesh_executions WHERE project_id=? AND intent=? AND id!=? AND status IN ('prepared','running','retryable','result_pending','retry_requested'))", project, intent, id).Scan(&pending)
	return
}

func ExecutionFailureKind(q Queries, project, id string) (kind string, err error) {
	err = q.QueryRow("SELECT COALESCE(json_extract(result,'$.failure_kind'),'') FROM pwnmesh_executions WHERE project_id=? AND id=?", project, id).Scan(&kind)
	return
}

// SaveExecutionRetryGrant preserves the original Job and Result bytes. Board
// authorizes the transition and keeps its revocation and lease release atomic.
func SaveExecutionRetryGrant(q Queries, project, id, now string) error {
	_, err := q.Exec("UPDATE pwnmesh_executions SET status='retry_requested',updated_at=? WHERE project_id=? AND id=?", now, project, id)
	return err
}
