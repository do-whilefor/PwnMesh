package sqlstore

func StateMetadata(q Queries, project string) (raw []byte, revision, decision int64, err error) {
	err = q.QueryRow("SELECT data,revision,decision_revision FROM pwnmesh_state WHERE project_id=?", project).Scan(&raw, &revision, &decision)
	return
}

func SaveStateMetadata(q Queries, project, raw string, revision, decision int64) error {
	_, err := q.Exec("INSERT INTO pwnmesh_state(project_id,data,revision,decision_revision) VALUES(?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET data=excluded.data,revision=excluded.revision,decision_revision=excluded.decision_revision", project, raw, revision, decision)
	return err
}

func UpdateStateMetadata(q Queries, project, raw string) error {
	_, err := q.Exec("UPDATE pwnmesh_state SET data=? WHERE project_id=?", raw, project)
	return err
}

// Board supplies initial metadata and the decision increment. Existing metadata
// is never rewritten for an event-only change, including unknown stored fields.
func AdvanceStateRevision(q Queries, project, initial string, decisionIncrement int) (int64, error) {
	var revision int64
	err := q.QueryRow(`INSERT INTO pwnmesh_state(project_id,data,revision,decision_revision)
		VALUES(?,?,1,?)
		ON CONFLICT(project_id) DO UPDATE SET revision=revision+1,decision_revision=decision_revision+excluded.decision_revision
		RETURNING revision`, project, initial, decisionIncrement).Scan(&revision)
	return revision, err
}

// The caller archives and revokes the old round before removing these records.
func DeleteStateRecords(q Queries, project string) error {
	if err := DeleteActionReceipts(q, project); err != nil {
		return err
	}
	for _, query := range []string{
		"DELETE FROM pwnmesh_state_events WHERE project_id=?",
		"DELETE FROM pwnmesh_state WHERE project_id=?",
	} {
		if _, err := q.Exec(query, project); err != nil {
			return err
		}
	}
	return nil
}
