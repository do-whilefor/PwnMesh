package sqlstore

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// StateStepMetadataByID keeps admission proportional to the requested Steps.
// Positions preserve duplicate-ID order: callers choose the existing first or
// last match policy. Legacy inline arrays remain readable until normalized.
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
	// Match Go's key casing and last-key semantics for historical headers.
	// Normalized headers contain only markers and small scalar metadata.
	var fields struct {
		Steps json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(header, &fields); err != nil {
		return nil, err
	}
	inline := fields.Steps
	if inline == nil {
		return nil, nil
	}
	if !emptyStateArray(inline) {
		var steps []json.RawMessage
		if err := json.Unmarshal(inline, &steps); err != nil {
			return nil, err
		}
		wanted := make(map[string]bool, len(ids))
		for _, id := range ids {
			wanted[id] = true
		}
		selected := make([]json.RawMessage, 0)
		for _, raw := range steps {
			var step struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &step); err != nil {
				return nil, err
			}
			if wanted[step.ID] {
				selected = append(selected, raw)
			}
		}
		return selected, nil
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
