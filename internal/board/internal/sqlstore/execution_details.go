package sqlstore

// ExecutionInputByLease returns the registered input without reading its
// potentially large result. Board validates the job and its snapshot binding.
func ExecutionInputByLease(q Queries, project, lease, kind string) (id string, job []byte, err error) {
	err = q.QueryRow("SELECT id,job FROM pwnmesh_executions WHERE project_id=? AND lease=? AND kind=?", project, lease, kind).Scan(&id, &job)
	return
}

func ExecutionIdentityByLease(q Queries, project, lease string) (id, status, intent string, err error) {
	err = q.QueryRow("SELECT id,status,intent FROM pwnmesh_executions WHERE project_id=? AND lease=?", project, lease).Scan(&id, &status, &intent)
	return
}

func RunningStepExecution(q Queries, project, intent, lease string) (running bool, err error) {
	err = q.QueryRow(`SELECT EXISTS(SELECT 1 FROM pwnmesh_executions WHERE project_id=? AND intent=? AND lease=? AND kind='explore' AND status IN ('running','result_pending'))`, project, intent, lease).Scan(&running)
	return
}

func CuratorExecutionStatus(q Queries, project, lease string) (status string, err error) {
	err = q.QueryRow("SELECT status FROM pwnmesh_executions WHERE project_id=? AND lease=? AND kind='curate' AND intent=''", project, lease).Scan(&status)
	return
}

type ExecutionResultRow struct {
	Kind, Intent, Lease, Status string
	Result                      []byte
}

func ExecutionResult(q Queries, project, id string) (result ExecutionResultRow, err error) {
	err = q.QueryRow("SELECT kind,intent,lease,status,result FROM pwnmesh_executions WHERE project_id=? AND id=?", project, id).Scan(&result.Kind, &result.Intent, &result.Lease, &result.Status, &result.Result)
	return
}

// SaveExecutionObservation changes only Board's validated result bytes. In
// particular it must not trigger status-based cleanup of decision read views.
func SaveExecutionObservation(q Queries, project, id string, result []byte) error {
	_, err := q.Exec("UPDATE pwnmesh_executions SET result=? WHERE project_id=? AND id=?", result, project, id)
	return err
}

func DecisionReadView(q Queries, project, id string) (version string, state []byte, err error) {
	err = q.QueryRow("SELECT state_version,state FROM pwnmesh_decision_read_views WHERE project_id=? AND execution_id=?", project, id).Scan(&version, &state)
	return
}

func SaveDecisionReadView(q Queries, project, id, version string, state []byte) error {
	_, err := q.Exec("INSERT INTO pwnmesh_decision_read_views(project_id,execution_id,state_version,state) VALUES(?,?,?,?) ON CONFLICT(project_id,execution_id) DO UPDATE SET state_version=excluded.state_version,state=excluded.state", project, id, version, state)
	return err
}
