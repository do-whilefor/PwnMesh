package sqlstore

import (
	"fmt"

	"pwnmesh/internal/board/model"
)

// Scoped sequences are shared by graph and metadata identities. Board chooses
// the kind and public prefix; the increment belongs to its mutation transaction.
func NextScopedID(q Queries, project, kind, prefix string) (string, error) {
	if _, err := q.Exec("INSERT OR IGNORE INTO scoped_counters(project_id,kind,value) VALUES(?,?,0)", project, kind); err != nil {
		return "", err
	}
	var n int
	err := q.QueryRow("UPDATE scoped_counters SET value=value+1 WHERE project_id=? AND kind=? RETURNING value", project, kind).Scan(&n)
	return fmt.Sprintf("%s%03d", prefix, n), err
}

// InsertFact reports primary-key collisions without overwriting an immutable
// observation. Constraint and trigger failures must still reach the caller.
func InsertFact(q Queries, project string, fact model.Fact) (bool, error) {
	result, err := q.Exec("INSERT INTO facts(id,project_id,description) VALUES(?,?,?) ON CONFLICT(id,project_id) DO NOTHING", fact.ID, project, fact.Description)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func SaveIntentConclusion(q Queries, project string, intent model.Intent) error {
	_, err := q.Exec("UPDATE intents SET to_fact_id=?,worker=?,last_heartbeat_at=?,concluded_at=? WHERE project_id=? AND id=?", intent.To, intent.Worker, intent.Heartbeat, intent.ConcludedAt, project, intent.ID)
	return err
}

func AbandonIntent(q Queries, project, id, now string) error {
	_, err := q.Exec("UPDATE intents SET worker=NULL,concluded_at=? WHERE project_id=? AND id=?", now, project, id)
	return err
}

func DeleteIntent(q Queries, project, id string) error {
	_, err := q.Exec("DELETE FROM intents WHERE project_id=? AND id=?", project, id)
	return err
}

func RevokeRun(q Queries, project, worker string) error {
	_, err := q.Exec("INSERT OR IGNORE INTO pwnmesh_revoked_runs(project_id,worker) VALUES(?,?)", project, worker)
	return err
}

func RevokePendingStepRuns(q Queries, project, step string, generation int64) error {
	_, err := q.Exec(`INSERT OR IGNORE INTO pwnmesh_revoked_runs(project_id,worker)
SELECT project_id,lease FROM pwnmesh_executions WHERE project_id=? AND intent=? AND generation=? AND kind='explore' AND `+PendingExecutionSQL, project, step, generation)
	return err
}

func InsertAssetAnchors(q Queries, project string, generation int64, kind, id string, assets []model.Asset) error {
	for _, asset := range assets {
		if _, err := q.Exec("INSERT OR IGNORE INTO pwnmesh_assets(project_id,id,kind,value,method) VALUES(?,?,?,?,?)", project, asset.ID, asset.Kind, asset.Value, asset.Method); err != nil {
			return err
		}
		if _, err := q.Exec("INSERT OR IGNORE INTO pwnmesh_asset_anchors(project_id,generation,node_kind,node_id,asset_id) VALUES(?,?,?,?,?)", project, generation, kind, id, asset.ID); err != nil {
			return err
		}
	}
	return nil
}

// Reservations include leased intents and pending executions whose intent
// lease may already have been revoked. Board supplies the path conflict policy.
func VisitStepWriteOwners(q Queries, project string, visit func(id, worker string) error) error {
	rows, err := q.Query(`SELECT id,worker FROM intents WHERE project_id=? AND worker IS NOT NULL AND to_fact_id IS NULL AND concluded_at IS NULL
UNION SELECT intent,lease FROM pwnmesh_executions WHERE project_id=? AND kind='explore' AND `+PendingExecutionSQL+` ORDER BY id,worker`, project, project)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, worker string
		if err := rows.Scan(&id, &worker); err != nil {
			return err
		}
		if err := visit(id, worker); err != nil {
			return err
		}
	}
	return rows.Err()
}
