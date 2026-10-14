package sqlstore

import "pwnmesh/internal/board/model"

// Lease writes touch only their owning columns. Board checks authority and
// eligibility before calling them in its existing transaction.
func SaveReasonLease(q Queries, project string, reason *model.Reason) error {
	if reason == nil {
		_, err := q.Exec("UPDATE projects SET reason_worker=NULL,reason_trigger=NULL,reason_started_at=NULL,reason_last_heartbeat_at=NULL WHERE id=?", project)
		return err
	}
	_, err := q.Exec("UPDATE projects SET reason_worker=?,reason_trigger=?,reason_started_at=?,reason_last_heartbeat_at=? WHERE id=?", reason.Worker, reason.Trigger, reason.StartedAt, reason.Heartbeat, project)
	return err
}

func SaveIntentLease(q Queries, project, intent, worker, heartbeat string) error {
	_, err := q.Exec("UPDATE intents SET worker=?,last_heartbeat_at=? WHERE project_id=? AND id=?", worker, heartbeat, project, intent)
	return err
}

func ReleaseIntentLease(q Queries, project, intent string) error {
	_, err := q.Exec("UPDATE intents SET worker=NULL WHERE project_id=? AND id=?", project, intent)
	return err
}

func ReleaseCuratorLease(q Queries, project, worker string) error {
	_, err := q.Exec(`UPDATE pwnmesh_project_orchestration SET curator_worker=NULL,curator_trigger=NULL,curator_started_at=NULL,curator_heartbeat=NULL WHERE project_id=? AND curator_worker=?`, project, worker)
	return err
}

func RevokeProjectRuns(q Queries, project string) error {
	_, err := q.Exec(`INSERT OR IGNORE INTO pwnmesh_revoked_runs(project_id,worker)
SELECT project_id,worker FROM intents WHERE project_id=? AND worker IS NOT NULL
UNION SELECT id,reason_worker FROM projects WHERE id=? AND reason_worker IS NOT NULL
UNION SELECT project_id,lease FROM pwnmesh_executions WHERE project_id=?
UNION SELECT project_id,curator_worker FROM pwnmesh_project_orchestration WHERE project_id=? AND curator_worker IS NOT NULL`, project, project, project, project)
	return err
}

func RunRevoked(q Queries, project, worker string) (bool, error) {
	var revoked bool
	err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM pwnmesh_revoked_runs WHERE project_id=? AND worker=?)", project, worker).Scan(&revoked)
	return revoked, err
}

// ExpireLeases applies Board's scope, supported version, clock and timeouts.
// Completed intents retain their worker; open intents retain their heartbeat.
func ExpireLeases(q Queries, project, now string, version, intentTimeout, reasonTimeout int) error {
	for _, expiry := range []struct {
		query, projectColumn string
		timeout              int
	}{
		{`UPDATE intents SET worker=NULL WHERE to_fact_id IS NULL AND worker IS NOT NULL AND (julianday(?)-julianday(last_heartbeat_at))*86400>?`, "project_id", intentTimeout},
		{`UPDATE projects SET reason_worker=NULL,reason_trigger=NULL,reason_started_at=NULL,reason_last_heartbeat_at=NULL WHERE reason_worker IS NOT NULL AND (julianday(?)-julianday(reason_last_heartbeat_at))*86400>?`, "id", reasonTimeout},
		{`UPDATE pwnmesh_project_orchestration SET curator_worker=NULL,curator_trigger=NULL,curator_started_at=NULL,curator_heartbeat=NULL WHERE curator_worker IS NOT NULL AND (julianday(?)-julianday(curator_heartbeat))*86400>?`, "project_id", reasonTimeout},
	} {
		args := []any{now, expiry.timeout, version}
		expiry.query += " AND " + expiry.projectColumn + " IN (SELECT project_id FROM pwnmesh_project_orchestration WHERE version=?)"
		if project != "" {
			expiry.query += " AND " + expiry.projectColumn + "=?"
			args = append(args, project)
		}
		if _, err := q.Exec(expiry.query, args...); err != nil {
			return err
		}
	}
	return nil
}
