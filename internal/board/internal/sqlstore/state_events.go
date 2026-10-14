package sqlstore

import "pwnmesh/internal/board/model"

func InsertStateEvent(q Queries, project string, revision int64, raw string) error {
	_, err := q.Exec("INSERT INTO pwnmesh_state_events(project_id,revision,event) VALUES(?,?,?)", project, revision, raw)
	return err
}

// A nil cursor and zero limit visit the entire history for archival. Ordinary
// discovery supplies a cursor and page limit. Both retain SQL revision order
// and exact event bytes without buffering the full history or decoding JSON.
func VisitStateEvents(q Queries, project string, after *int64, limit int, visit func(int64, []byte) error) error {
	query := "SELECT revision,event FROM pwnmesh_state_events WHERE project_id=?"
	args := []any{project}
	if after != nil {
		query += " AND revision>?"
		args = append(args, *after)
	}
	query += " ORDER BY revision"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := q.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var revision int64
		var raw []byte
		if err := rows.Scan(&revision, &raw); err != nil {
			return err
		}
		if err := visit(revision, raw); err != nil {
			return err
		}
	}
	return rows.Err()
}

func StateChanges(q Queries, project string, after, through int64, limit int) ([]model.StateChange, error) {
	rows, err := q.Query(`SELECT revision,json_extract(event,'$.op'),json_extract(event,'$.id') FROM pwnmesh_state_events WHERE project_id=? AND revision>? AND revision<=? ORDER BY revision LIMIT ?`, project, after, through, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.StateChange{}
	for rows.Next() {
		var change model.StateChange
		if err := rows.Scan(&change.Revision, &change.Op, &change.ID); err != nil {
			return nil, err
		}
		out = append(out, change)
	}
	return out, rows.Err()
}

type CurationEvent struct {
	Op, ID, RunID, Result string
}

// Read only completion and correction markers. Board decides whether their
// current sources can support a review or invalidate a retained conclusion.
func VisitCurationEvents(q Queries, project string, after int64, visit func(CurationEvent) error) error {
	rows, err := q.Query(`SELECT json_extract(event,'$.op'),json_extract(event,'$.id'),json_extract(event,'$.run_id'),
		CASE WHEN json_extract(event,'$.op')='fact_relation' THEN json_extract(event,'$.result') ELSE '{}' END
		FROM pwnmesh_state_events WHERE project_id=? AND revision>? AND (
		json_extract(event,'$.op') IN ('step_completed','fact_relation') OR
		(json_extract(event,'$.op')='curate' AND json_array_length(event,'$.payload.relations')>0))`, project, after)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var event CurationEvent
		if err := rows.Scan(&event.Op, &event.ID, &event.RunID, &event.Result); err != nil {
			return err
		}
		if err := visit(event); err != nil {
			return err
		}
	}
	return rows.Err()
}
