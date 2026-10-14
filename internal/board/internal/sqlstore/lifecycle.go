package sqlstore

// Lifecycle rows share Board's transaction with its authority checks, archive,
// execution transitions and events. These operations never finalize it.
func MarkPausedExecutions(q Queries, project string) error {
	_, err := q.Exec(`INSERT OR IGNORE INTO pwnmesh_paused_executions(project_id,execution_id)
 SELECT e.project_id,e.id FROM pwnmesh_executions e
 WHERE e.project_id=? AND e.status IN ('prepared','running','retryable','result_pending')
 AND NOT EXISTS(SELECT 1 FROM pwnmesh_revoked_runs r WHERE r.project_id=e.project_id AND r.worker=e.lease)`, project)
	return err
}

func PausedExecutionIDs(q Queries, project string) ([]string, error) {
	return lifecycleExecutionIDs(q, "SELECT execution_id FROM pwnmesh_paused_executions WHERE project_id=?", project)
}

func PendingProjectExecutionIDs(q Queries, project string) ([]string, error) {
	return lifecycleExecutionIDs(q, "SELECT id FROM pwnmesh_executions WHERE project_id=? AND "+PendingExecutionSQL, project)
}

// Close the cursor before Board starts updating these executions.
func lifecycleExecutionIDs(q Queries, query, project string) ([]string, error) {
	rows, err := q.Query(query, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func DeletePausedExecutions(q Queries, project string) error {
	_, err := q.Exec("DELETE FROM pwnmesh_paused_executions WHERE project_id=?", project)
	return err
}

// Outstanding grants have already recorded their failure. Cancelling them
// retains the saved result; Board emits no second failure event for this write.
func CancelProjectRetries(q Queries, project, now string) error {
	_, err := q.Exec("UPDATE pwnmesh_executions SET status='cancelled',updated_at=? WHERE project_id=? AND status='retry_requested'", now, project)
	return err
}

func RecordProjectTermination(q Queries, project, now string) error {
	_, err := q.Exec("INSERT INTO pwnmesh_project_termination(project_id,terminated_at) VALUES(?,?)", project, now)
	return err
}

// ResetProjectRound removes the current projection after Board has archived
// it and revoked its leases. Counters, tombstones, inputs and history survive.
func ResetProjectRound(q Queries, project string, generation int64, now string) error {
	if _, err := q.Exec("DELETE FROM pwnmesh_project_termination WHERE project_id=?", project); err != nil {
		return err
	}
	if err := DeleteStateRecords(q, project); err != nil {
		return err
	}
	for _, query := range []string{
		"DELETE FROM pwnmesh_executions WHERE project_id=?",
		// The generation fence rejects old cursors. Retain the revision
		// sequence, but remove deletion markers created by the preceding write.
		"DELETE FROM pwnmesh_ui_execution_changes WHERE project_id=?",
		"DELETE FROM intents WHERE project_id=?",
		"DELETE FROM facts WHERE project_id=? AND id NOT IN ('origin','goal')",
	} {
		if _, err := q.Exec(query, project); err != nil {
			return err
		}
	}
	_, err := q.Exec(`INSERT INTO pwnmesh_project_rounds(project_id,generation,restarted_at) VALUES(?,?,?) ON CONFLICT(project_id) DO UPDATE SET generation=excluded.generation,restarted_at=excluded.restarted_at`, project, generation, now)
	return err
}
