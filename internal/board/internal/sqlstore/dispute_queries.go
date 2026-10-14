package sqlstore

// IntentExecutionCount includes every registered attempt, across namespaces,
// generations and outcomes. Board decides which steps share a review allowance.
func IntentExecutionCount(q Queries, project, intent string) (count int, err error) {
	err = q.QueryRow("SELECT COUNT(*) FROM pwnmesh_executions WHERE project_id=? AND intent=?", project, intent).Scan(&count)
	return
}

type DecisionExecutionInput struct {
	ProjectID, Lease, StateVersion string
	Generation, DecisionRevision   int64
}

// PendingDecisionExecution matches the exact saved planning input without
// loading a job or result. Board decides whether its invalidation needs a new
// decision revision; the shared-content version remains a strict CAS boundary.
func PendingDecisionExecution(q Queries, input DecisionExecutionInput) (exists bool, err error) {
	err = q.QueryRow(`SELECT EXISTS(SELECT 1 FROM pwnmesh_executions
		WHERE project_id=? AND kind='reason' AND lease=? AND `+PendingExecutionSQL+`
		AND generation=? AND decision_revision=? AND state_version=?
		AND json_extract(job,'$.decision.version')=2)`,
		input.ProjectID, input.Lease, input.Generation, input.DecisionRevision, input.StateVersion).Scan(&exists)
	return
}
