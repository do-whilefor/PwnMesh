package board

// RevokeRuns invalidates execution identities while retaining worker and
// heartbeat metadata on concluded intents. A resumed project must reject
// delayed writes from revoked workers.
func (t *Tx) RevokeRuns(project string) error {
	_, err := t.Exec(`INSERT OR IGNORE INTO pwnmesh_revoked_runs(project_id,worker)
SELECT project_id,worker FROM intents WHERE project_id=? AND worker IS NOT NULL
UNION SELECT id,reason_worker FROM projects WHERE id=? AND reason_worker IS NOT NULL
UNION SELECT project_id,lease FROM pwnmesh_executions WHERE project_id=?
UNION SELECT project_id,curator_worker FROM pwnmesh_project_orchestration WHERE project_id=? AND curator_worker IS NOT NULL`, project, project, project, project)
	return err
}

func (t *Tx) RunRevoked(project, worker string) (bool, error) {
	var revoked bool
	err := t.QueryRow("SELECT EXISTS(SELECT 1 FROM pwnmesh_revoked_runs WHERE project_id=? AND worker=?)", project, worker).Scan(&revoked)
	return revoked, err
}

// ExpireProject limits lease housekeeping to the project being observed or
// mutated. Global scheduling/list requests pass an empty project and still
// expire all leases, including those whose workers have crashed.
func (t *Tx) ExpireProject(project string) error {
	s, err := t.Settings()
	if err != nil {
		return err
	}
	for _, expiry := range []struct {
		query, projectColumn string
		timeout              int
	}{
		{`UPDATE intents SET worker=NULL WHERE to_fact_id IS NULL AND worker IS NOT NULL AND (julianday(?)-julianday(last_heartbeat_at))*86400>?`, "project_id", s.IntentTimeout},
		{`UPDATE projects SET reason_worker=NULL,reason_trigger=NULL,reason_started_at=NULL,reason_last_heartbeat_at=NULL WHERE reason_worker IS NOT NULL AND (julianday(?)-julianday(reason_last_heartbeat_at))*86400>?`, "id", s.ReasonTimeout},
		{`UPDATE pwnmesh_project_orchestration SET curator_worker=NULL,curator_trigger=NULL,curator_started_at=NULL,curator_heartbeat=NULL WHERE curator_worker IS NOT NULL AND (julianday(?)-julianday(curator_heartbeat))*86400>?`, "project_id", s.ReasonTimeout},
	} {
		args := []any{t.Now, expiry.timeout}
		expiry.query += " AND " + expiry.projectColumn + " IN (SELECT project_id FROM pwnmesh_project_orchestration WHERE version=1)"
		if project != "" {
			expiry.query += " AND " + expiry.projectColumn + "=?"
			args = append(args, project)
		}
		if _, err = t.Exec(expiry.query, args...); err != nil {
			return err
		}
	}
	return nil
}
