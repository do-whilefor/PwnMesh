package board

import (
	"database/sql"
	"encoding/json"
	"errors"

	"pwnmesh/internal/board/internal/sqlstore"
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

func executionView(row sqlstore.ExecutionViewRow) ExecutionView {
	return ExecutionView{
		ID: row.ID, ProjectID: row.ProjectID, Generation: row.Generation,
		Kind: row.Kind, Backend: row.Backend, Intent: row.Intent, Status: row.Status,
		Resumes: row.Resumes, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		Result: executionResultView(row.Text, row.Failure),
	}
}

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
	latest, err := sqlstore.LatestExecutionRevision(t, project)
	if err != nil {
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
	used, last, count := 128, max(after, cursor), 0
	err = sqlstore.VisitExecutionChanges(t, project, max(after, cursor), out.Through, after > 0, limit+1, func(change sqlstore.ExecutionChangeRow) (bool, error) {
		var entry ExecutionView
		var raw []byte
		var err error
		if change.Deleted {
			raw, err = json.Marshal(change.ID)
		} else {
			entry = executionView(change.View)
			raw, err = json.Marshal(entry)
		}
		if err != nil {
			return false, err
		}
		if len(raw)+128 > MaxExecutionViewPageBytes {
			return false, Err(422, "execution metadata exceeds page size")
		}
		if count == limit || used+len(raw)+1 > MaxExecutionViewPageBytes {
			out.NextCursor = last
			return false, nil
		}
		if change.Deleted {
			out.Deleted = append(out.Deleted, change.ID)
		} else {
			out.Items = append(out.Items, entry)
		}
		used += len(raw) + 1
		last, count = change.Revision, count+1
		return true, nil
	})
	return out, err
}

// Through freezes the set of executions to enumerate, not their changing
// statuses. A fresh refresh starts at zero and sees current runtime metadata.
func (t *Tx) ProjectExecutionPage(project string, after, through int64, limit int) (ExecutionViewPage, error) {
	out := ExecutionViewPage{Items: []ExecutionView{}, Through: through}
	if after < 0 || through < 0 || (through != 0 && after > through) || limit < 1 || limit > 100 {
		return out, Err(422, "invalid execution page boundary (limit must be 1-100)")
	}
	exists, err := sqlstore.ProjectExists(t, project)
	if err != nil {
		return out, err
	}
	if !exists {
		return out, Err(404, "Project not found")
	}
	if through == 0 {
		out.Through, err = sqlstore.LatestExecutionRow(t, project)
		if err != nil {
			return out, err
		}
	}
	used, last := 128, after
	err = sqlstore.VisitExecutionViews(t, project, after, out.Through, limit+1, func(row sqlstore.ExecutionViewRow) (bool, error) {
		entry := executionView(row)
		raw, err := json.Marshal(entry)
		if err != nil {
			return false, err
		}
		if len(raw)+128 > MaxExecutionViewPageBytes {
			return false, Err(422, "execution metadata exceeds page size")
		}
		if len(out.Items) == limit || used+len(raw)+1 > MaxExecutionViewPageBytes {
			out.NextCursor = last
			return false, nil
		}
		out.Items = append(out.Items, entry)
		used += len(raw) + 1
		last = row.RowID
		return true, nil
	})
	return out, err
}

// ProjectIdentity is the lightweight round check used after a workspace refresh.
// It avoids loading graph records only to detect a concurrent restart.
type ProjectIdentity struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
}

func (t *Tx) ProjectIdentity(project string) (ProjectIdentity, error) {
	var identity ProjectIdentity
	var err error
	identity.ID, identity.Generation, err = sqlstore.ProjectIdentity(t, project)
	if errors.Is(err, sql.ErrNoRows) {
		err = Err(404, "Project not found")
	}
	return identity, err
}
