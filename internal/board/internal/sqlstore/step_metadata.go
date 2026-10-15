package sqlstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// StateStepMetadataByID keeps admission proportional to the requested Steps.
// Positions preserve duplicate-ID order: callers choose the existing first or
// last match policy.
func StateStepMetadataByID(q Queries, project string, ids []string) ([]json.RawMessage, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var header []byte
	err := q.QueryRow("SELECT data FROM pwnmesh_state WHERE project_id=?", project).Scan(&header)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fields, err := storedStateFields(header)
	if err != nil {
		return nil, err
	}
	if !emptyStateArray(fields["steps"]) {
		var records bool
		if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM pwnmesh_state_records WHERE project_id=? AND kind='steps')", project).Scan(&records); err != nil {
			return nil, err
		}
		if records {
			return nil, fmt.Errorf("current Step records require an empty array header marker")
		}
		return nil, nil
	}
	rawIDs, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(stepMetadataByIDQuery, project, string(rawIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var steps []json.RawMessage
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		steps = append(steps, raw)
	}
	return steps, rows.Err()
}

// Seek by ID before sorting the small selection. Without INDEXED BY, SQLite
// can prefer scanning all project Steps to satisfy the position ordering.
const stepMetadataByIDQuery = `SELECT data FROM pwnmesh_state_records INDEXED BY pwnmesh_state_steps_id
WHERE project_id=? AND kind='steps' AND record_id IN (SELECT value FROM json_each(?)) ORDER BY position`
