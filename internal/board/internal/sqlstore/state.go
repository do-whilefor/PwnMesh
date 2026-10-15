package sqlstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

func StateMetadata(q Queries, project string) (raw []byte, revision, decision int64, err error) {
	err = q.QueryRow("SELECT data,revision,decision_revision FROM pwnmesh_state WHERE project_id=?", project).Scan(&raw, &revision, &decision)
	if err != nil {
		return
	}
	fields, decodeErr := storedStateFields(raw)
	if decodeErr != nil {
		err = decodeErr
		return
	}
	changed, restoreErr := restoreStateRecords(q, project, fields)
	if restoreErr != nil {
		err = restoreErr
		return
	}
	var facts []json.RawMessage
	rows, queryErr := q.Query("SELECT data FROM pwnmesh_fact_metadata WHERE project_id=? ORDER BY position", project)
	if queryErr != nil {
		err = queryErr
		return
	}
	defer rows.Close()
	for rows.Next() {
		var fact []byte
		if err = rows.Scan(&fact); err != nil {
			return
		}
		facts = append(facts, fact)
	}
	if err = rows.Err(); err != nil {
		return
	}
	if len(facts) != 0 {
		if bytes.Equal(bytes.TrimSpace(fields["facts"]), []byte("null")) {
			err = fmt.Errorf("current Fact records conflict with a null header marker")
			return
		}
		fields["facts"], err = json.Marshal(facts)
		changed = true
	}
	if err == nil && changed {
		raw, err = json.Marshal(fields)
	}
	return
}

func SaveStateMetadata(q Queries, project, raw string, revision, decision int64) error {
	remainder, facts, err := splitStateFacts(raw)
	if err != nil {
		return err
	}
	remainder, records, err := splitStateRecords(remainder)
	if err != nil {
		return err
	}
	return writeStateMetadata(q, func() error {
		if _, err := q.Exec("INSERT INTO pwnmesh_state(project_id,data,revision,decision_revision) VALUES(?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET data=excluded.data,revision=excluded.revision,decision_revision=excluded.decision_revision", project, remainder, revision, decision); err != nil {
			return err
		}
		if err := saveStateFacts(q, project, facts); err != nil {
			return err
		}
		return saveStateRecords(q, project, records)
	})
}

func UpdateStateMetadata(q Queries, project, raw string) error {
	remainder, facts, err := splitStateFacts(raw)
	if err != nil {
		return err
	}
	remainder, records, err := splitStateRecords(remainder)
	if err != nil {
		return err
	}
	return writeStateMetadata(q, func() error {
		result, err := q.Exec("UPDATE pwnmesh_state SET data=? WHERE project_id=?", remainder, project)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		if err := saveStateFacts(q, project, facts); err != nil {
			return err
		}
		return saveStateRecords(q, project, records)
	})
}

// Metadata remains one atomic write even if the surrounding caller handles an
// error and commits other work. Invalidation triggers share this savepoint.
func writeStateMetadata(q Queries, write func() error) (err error) {
	if _, err = q.Exec("SAVEPOINT pwnmesh_state_metadata"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, rollbackErr := q.Exec("ROLLBACK TO pwnmesh_state_metadata")
			err = errors.Join(err, rollbackErr)
		}
		_, releaseErr := q.Exec("RELEASE pwnmesh_state_metadata")
		err = errors.Join(err, releaseErr)
	}()
	return write()
}

// Ordered records preserve duplicate IDs and orphan observations.
func splitStateFacts(raw string) (string, []json.RawMessage, error) {
	fields, err := decodeStateFields([]byte(raw))
	if err != nil {
		return "", nil, err
	}
	if fields["facts"] == nil || bytes.Equal(bytes.TrimSpace(fields["facts"]), []byte("null")) {
		return raw, nil, nil
	}
	var facts []json.RawMessage
	if err := json.Unmarshal(fields["facts"], &facts); err != nil {
		return "", nil, err
	}
	for n := range facts {
		var compact bytes.Buffer
		if err := json.Compact(&compact, facts[n]); err != nil {
			return "", nil, err
		}
		facts[n] = append(json.RawMessage(nil), compact.Bytes()...)
	}
	delete(fields, "facts")
	remainder, err := json.Marshal(fields)
	return string(remainder), facts, err
}

func saveStateFacts(q Queries, project string, facts []json.RawMessage) error {
	rows, err := q.Query("SELECT position,data FROM pwnmesh_fact_metadata WHERE project_id=? ORDER BY position", project)
	if err != nil {
		return err
	}
	previous := make(map[int]string, len(facts))
	trim := false
	for rows.Next() {
		var position int
		var data string
		if err := rows.Scan(&position, &data); err != nil {
			rows.Close()
			return err
		}
		previous[position] = data
		trim = trim || position >= len(facts)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for position, fact := range facts {
		if previous[position] == string(fact) {
			continue
		}
		if _, err := q.Exec("INSERT INTO pwnmesh_fact_metadata(project_id,position,data) VALUES(?,?,?) ON CONFLICT(project_id,position) DO UPDATE SET data=excluded.data", project, position, string(fact)); err != nil {
			return err
		}
	}
	// Even a no-op DELETE compiles its invalidation trigger programs.
	if !trim {
		return nil
	}
	_, err = q.Exec("DELETE FROM pwnmesh_fact_metadata WHERE project_id=? AND position>=?", project, len(facts))
	return err
}

// Board supplies initial metadata and the decision increment. Existing metadata
// is never rewritten for an event-only change, including unknown stored fields.
func AdvanceStateRevision(q Queries, project, initial string, decisionIncrement int) (int64, error) {
	if _, err := storedStateFields([]byte(initial)); err != nil {
		return 0, err
	}
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
