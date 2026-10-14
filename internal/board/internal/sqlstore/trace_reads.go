package sqlstore

import "pwnmesh/internal/board/model"

// Trace rows carry the registered Job for Board to validate before exposing
// any file capability. Stop on its first rejection without loading later Jobs.
func VisitTraceRuns(q Queries, project string, generation int64, offset, limit int, runID string, visit func(model.TraceRun) error) error {
	query := `SELECT id,intent,status,job FROM pwnmesh_executions WHERE project_id=? AND generation=? AND kind='explore'`
	args := []any{project, generation}
	if runID != "" {
		query += " AND id=?"
		args = append(args, runID)
	}
	query += " ORDER BY rowid LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := q.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		run := model.TraceRun{ProjectID: project, Generation: generation}
		var raw []byte
		if err := rows.Scan(&run.RunID, &run.StepID, &run.Status, &raw); err != nil {
			return err
		}
		run.RegisteredJob = raw
		if err := visit(run); err != nil {
			return err
		}
	}
	return rows.Err()
}
