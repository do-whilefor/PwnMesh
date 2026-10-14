package board

import (
	"database/sql"
	"encoding/json"
	"errors"
)

func ValidScenario(scenario string) bool {
	return scenario == "ctf" || scenario == "pentest" || scenario == "audit"
}

// ExecutionView is a read-only projection. It deliberately cannot serialize
// dispatch inputs, lease identities, retry authorization, or environment data.
type ExecutionView struct {
	ID         string               `json:"id"`
	ProjectID  string               `json:"project_id"`
	Generation int64                `json:"generation"`
	Kind       string               `json:"kind"`
	Backend    string               `json:"backend"`
	Intent     string               `json:"intent"`
	Status     string               `json:"status"`
	Resumes    int                  `json:"resumes"`
	CreatedAt  string               `json:"created_at"`
	UpdatedAt  string               `json:"updated_at"`
	Result     *ExecutionResultView `json:"result,omitempty"`
}

type ExecutionResultView struct {
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func executionResultView(text, failure string) *ExecutionResultView {
	result := ExecutionResultView{Text: text, Error: failure}
	if result.Text == "" && result.Error == "" {
		return nil
	}
	result.Truncated = false
	for _, field := range []struct {
		text  *string
		limit int
	}{{&result.Text, 64 * 1024}, {&result.Error, 4 * 1024}} {
		if runes := []rune(*field.text); len(runes) > field.limit {
			*field.text = string(runes[:field.limit])
			result.Truncated = true
		}
	}
	return &result
}

// The SQL boundary extracts only bounded public result fields. Four UTF-8
// bytes per character plus a sentinel preserve the existing character limits;
// slicing BLOBs also preserves embedded NULs (SQLite text substr does not).
// Historical Job bodies never enter this read.
const executionViewColumns = `rowid,id,project_id,generation,kind,backend,intent,status,resumes,created_at,updated_at,
COALESCE(CASE WHEN json_valid(result) THEN CASE WHEN json_type(result,'$.text')='text' THEN substr(CAST(json_extract(result,'$.text') AS BLOB),1,262148) END END,''),
COALESCE(CASE WHEN json_valid(result) THEN CASE WHEN json_type(result,'$.error')='text' THEN substr(CAST(json_extract(result,'$.error') AS BLOB),1,16388) END END,'')`

const MaxExecutionViewPageBytes = 1 << 20

type ExecutionViewPage struct {
	Items      []ExecutionView `json:"items"`
	NextCursor int64           `json:"next_cursor,omitempty"`
	Through    int64           `json:"through"`
}

type ExecutionChangePage struct {
	Items      []ExecutionView `json:"items"`
	Deleted    []string        `json:"deleted"`
	NextCursor int64           `json:"next_cursor,omitempty"`
	Through    int64           `json:"through"`
	Generation int64           `json:"generation"`
}

// A row updated after Through moves to the next refresh rather than being lost
// behind an append-only rowid cursor. Changes and projections share one Tx.
func (t *Tx) ProjectExecutionChanges(project string, generation, after, cursor, through int64, limit int) (ExecutionChangePage, error) {
	out := ExecutionChangePage{Items: []ExecutionView{}, Deleted: []string{}, Through: through, Generation: generation}
	if generation < 0 || after < 0 || cursor < 0 || through < 0 || limit < 1 || limit > 100 || (through != 0 && max(after, cursor) > through) {
		return out, Err(422, "invalid execution change boundary")
	}
	identity, err := t.ProjectIdentity(project)
	if err != nil {
		return out, err
	}
	if identity.Generation != generation {
		return out, Err(409, "Project round changed; refresh before reading execution changes")
	}
	var latest int64
	if err = t.QueryRow("SELECT COALESCE((SELECT revision FROM xloom_ui_execution_revisions WHERE project_id=?),0)", project).Scan(&latest); err != nil {
		return out, err
	}
	if through == 0 {
		out.Through = latest
	}
	if max(after, cursor) > out.Through || out.Through > latest {
		return out, Err(409, "Execution change cursor is no longer current")
	}
	// An initial snapshot has no cached rows to remove, so old-round tombstones
	// must not make a new round reload every prior execution's deletion.
	rows, err := t.Query("SELECT execution_id,revision,deleted FROM xloom_ui_execution_changes WHERE project_id=? AND revision>? AND revision<=? AND (? > 0 OR deleted=0) ORDER BY revision LIMIT ?", project, max(after, cursor), out.Through, after, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	used, last, count := 128, max(after, cursor), 0
	for rows.Next() {
		var id string
		var revision int64
		var deleted bool
		if err = rows.Scan(&id, &revision, &deleted); err != nil {
			return out, err
		}
		var entry ExecutionView
		var raw []byte
		if deleted {
			raw, err = json.Marshal(id)
		} else {
			var row int64
			var text, failure string
			err = t.QueryRow("SELECT "+executionViewColumns+" FROM xloom_executions WHERE project_id=? AND id=?", project, id).Scan(&row, &entry.ID, &entry.ProjectID, &entry.Generation, &entry.Kind, &entry.Backend, &entry.Intent, &entry.Status, &entry.Resumes, &entry.CreatedAt, &entry.UpdatedAt, &text, &failure)
			if err == nil {
				entry.Result = executionResultView(text, failure)
				raw, err = json.Marshal(entry)
			}
		}
		if err != nil {
			return out, err
		}
		if len(raw)+128 > MaxExecutionViewPageBytes {
			return out, Err(422, "execution metadata exceeds page size")
		}
		if count == limit || used+len(raw)+1 > MaxExecutionViewPageBytes {
			out.NextCursor = last
			break
		}
		if deleted {
			out.Deleted = append(out.Deleted, id)
		} else {
			out.Items = append(out.Items, entry)
		}
		used += len(raw) + 1
		last, count = revision, count+1
	}
	return out, rows.Err()
}

// Through freezes the set of executions to enumerate, not their changing
// statuses. A fresh refresh starts at zero and sees current runtime metadata.
func (t *Tx) ProjectExecutionPage(project string, after, through int64, limit int) (ExecutionViewPage, error) {
	out := ExecutionViewPage{Items: []ExecutionView{}, Through: through}
	if after < 0 || through < 0 || (through != 0 && after > through) || limit < 1 || limit > 100 {
		return out, Err(422, "invalid execution page boundary (limit must be 1-100)")
	}
	var exists bool
	if err := t.QueryRow("SELECT EXISTS(SELECT 1 FROM projects WHERE id=?)", project).Scan(&exists); err != nil {
		return out, err
	}
	if !exists {
		return out, Err(404, "Project not found")
	}
	if through == 0 {
		if err := t.QueryRow("SELECT COALESCE(MAX(rowid),0) FROM xloom_executions WHERE project_id=?", project).Scan(&out.Through); err != nil {
			return out, err
		}
	}
	rows, err := t.Query("SELECT "+executionViewColumns+" FROM xloom_executions WHERE project_id=? AND rowid>? AND rowid<=? ORDER BY rowid LIMIT ?", project, after, out.Through, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	used, last := 128, after
	for rows.Next() {
		var entry ExecutionView
		var row int64
		var text, failure string
		if err := rows.Scan(&row, &entry.ID, &entry.ProjectID, &entry.Generation, &entry.Kind, &entry.Backend, &entry.Intent, &entry.Status, &entry.Resumes, &entry.CreatedAt, &entry.UpdatedAt, &text, &failure); err != nil {
			return out, err
		}
		entry.Result = executionResultView(text, failure)
		raw, err := json.Marshal(entry)
		if err != nil {
			return out, err
		}
		if len(raw)+128 > MaxExecutionViewPageBytes {
			return out, Err(422, "execution metadata exceeds page size")
		}
		if len(out.Items) == limit || used+len(raw)+1 > MaxExecutionViewPageBytes {
			out.NextCursor = last
			break
		}
		out.Items = append(out.Items, entry)
		used += len(raw) + 1
		last = row
	}
	return out, rows.Err()
}

// ProjectIdentity is the lightweight round check used after a workspace refresh.
// It avoids loading graph records only to detect a concurrent restart.
type ProjectIdentity struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
}

func (t *Tx) ProjectIdentity(project string) (ProjectIdentity, error) {
	var identity ProjectIdentity
	err := t.QueryRow(`SELECT p.id,COALESCE(r.generation,0) FROM projects p LEFT JOIN xloom_project_rounds r ON r.project_id=p.id WHERE p.id=?`, project).Scan(&identity.ID, &identity.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		err = Err(404, "Project not found")
	}
	return identity, err
}
