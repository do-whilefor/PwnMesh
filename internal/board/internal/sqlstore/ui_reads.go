package sqlstore

// ExecutionViewRow contains only public metadata and bounded result fields.
// Board applies character limits and the serialized response size budget.
type ExecutionViewRow struct {
	RowID      int64
	ID         string
	ProjectID  string
	Generation int64
	Kind       string
	Backend    string
	Intent     string
	Status     string
	Resumes    int
	CreatedAt  string
	UpdatedAt  string
	Text       string
	Failure    string
}

// Four UTF-8 bytes per character plus a sentinel preserve character limits.
// BLOB slicing preserves embedded NULs, unlike SQLite text substr. Historical
// Job bodies and private Result fields never enter these reads.
const executionViewColumns = `rowid,id,project_id,generation,kind,backend,intent,status,resumes,created_at,updated_at,
COALESCE(CASE WHEN json_valid(result) THEN CASE WHEN json_type(result,'$.text')='text' THEN substr(CAST(json_extract(result,'$.text') AS BLOB),1,262148) END END,''),
COALESCE(CASE WHEN json_valid(result) THEN CASE WHEN json_type(result,'$.error')='text' THEN substr(CAST(json_extract(result,'$.error') AS BLOB),1,16388) END END,'')`

func scanExecutionView(s scanner) (ExecutionViewRow, error) {
	var row ExecutionViewRow
	err := s.Scan(&row.RowID, &row.ID, &row.ProjectID, &row.Generation, &row.Kind, &row.Backend, &row.Intent, &row.Status, &row.Resumes, &row.CreatedAt, &row.UpdatedAt, &row.Text, &row.Failure)
	return row, err
}

func ProjectIdentity(q Queries, project string) (id string, generation int64, err error) {
	err = q.QueryRow(`SELECT p.id,COALESCE(r.generation,0) FROM projects p LEFT JOIN pwnmesh_project_rounds r ON r.project_id=p.id WHERE p.id=?`, project).Scan(&id, &generation)
	return
}

func LatestExecutionRow(q Queries, project string) (row int64, err error) {
	err = q.QueryRow("SELECT COALESCE(MAX(rowid),0) FROM pwnmesh_executions WHERE project_id=?", project).Scan(&row)
	return
}

func LatestExecutionRevision(q Queries, project string) (revision int64, err error) {
	err = q.QueryRow("SELECT COALESCE((SELECT revision FROM pwnmesh_ui_execution_revisions WHERE project_id=?),0)", project).Scan(&revision)
	return
}

// Visitors keep byte-limited reads streaming. Returning false stops before
// scanning another row; all queries retain the caller's transaction snapshot.
func VisitExecutionViews(q Queries, project string, after, through int64, limit int, visit func(ExecutionViewRow) (bool, error)) error {
	rows, err := q.Query("SELECT "+executionViewColumns+" FROM pwnmesh_executions WHERE project_id=? AND rowid>? AND rowid<=? ORDER BY rowid LIMIT ?", project, after, through, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		row, err := scanExecutionView(rows)
		if err != nil {
			return err
		}
		more, err := visit(row)
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	return rows.Err()
}

type ExecutionChangeRow struct {
	ID       string
	Revision int64
	Deleted  bool
	View     ExecutionViewRow
}

func VisitExecutionChanges(q Queries, project string, after, through int64, includeDeleted bool, limit int, visit func(ExecutionChangeRow) (bool, error)) error {
	rows, err := q.Query("SELECT execution_id,revision,deleted FROM pwnmesh_ui_execution_changes WHERE project_id=? AND revision>? AND revision<=? AND (? OR deleted=0) ORDER BY revision LIMIT ?", project, after, through, includeDeleted, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var change ExecutionChangeRow
		if err := rows.Scan(&change.ID, &change.Revision, &change.Deleted); err != nil {
			return err
		}
		if !change.Deleted {
			change.View, err = scanExecutionView(q.QueryRow("SELECT "+executionViewColumns+" FROM pwnmesh_executions WHERE project_id=? AND id=?", project, change.ID))
			if err != nil {
				return err
			}
		}
		more, err := visit(change)
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	return rows.Err()
}
