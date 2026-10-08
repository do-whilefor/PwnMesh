package board

import (
	"encoding/json"
)

// TraceRun is an internal capability descriptor, not a model-visible record.
// Workspace and JobSHA256 bind the registered execution to its local journal.
type TraceRun struct {
	ProjectID  string `json:"project_id"`
	Generation int64  `json:"generation"`
	RunID      string `json:"run_id"`
	StepID     string `json:"step_id"`
	Status     string `json:"status"`
	Workspace  string `json:"workspace"`
	JobSHA256  string `json:"job_sha256"`
	// Server-side only: Worker owns Job's canonical serialization. It binds
	// JobSHA256 before delivering this descriptor through the trace bridge.
	RegisteredJob json.RawMessage `json:"-"`
}

// TraceRuns lists only registered Explore attempts of the current project
// round. Callers must additionally enforce the requesting execution's fence.
// Ordering by insertion, rather than status, keeps offset pages stable when a
// running attempt finishes. A restart invalidates the generation immediately.
func (t *Tx) TraceRuns(project string, generation int64, offset, limit int, runID string) ([]TraceRun, error) {
	if project == "" || generation < 0 || offset < 0 || limit < 1 || limit > 51 || len(runID) > 256 || runID != "" && offset != 0 {
		return nil, Err(422, "invalid trace run page")
	}
	current, err := t.ProjectIdentity(project)
	if err != nil {
		return nil, err
	}
	if current.Generation != generation {
		return nil, Err(409, "trace input belongs to a previous project round")
	}
	query := `SELECT id,intent,status,job FROM xloom_executions WHERE project_id=? AND generation=? AND kind='explore'`
	args := []any{project, generation}
	if runID != "" {
		query += " AND id=?"
		args = append(args, runID)
	}
	query += " ORDER BY rowid LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := t.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TraceRun{}
	for rows.Next() {
		run := TraceRun{ProjectID: project, Generation: generation}
		var raw []byte
		if err := rows.Scan(&run.RunID, &run.StepID, &run.Status, &raw); err != nil {
			return nil, err
		}
		var job struct {
			RunID     string  `json:"run_id"`
			Kind      string  `json:"kind"`
			Workspace string  `json:"workspace"`
			Graph     Graph   `json:"graph"`
			Intent    *Intent `json:"intent"`
		}
		// Malformed or legacy unbound jobs never become file capabilities.
		if json.Unmarshal(raw, &job) != nil || job.RunID != run.RunID || job.Kind != "explore" || job.Graph.Project.ID != project || job.Graph.Project.Generation != generation || job.Intent == nil || job.Intent.ID != run.StepID || run.StepID == "" || job.Workspace == "" {
			return nil, Err(409, "trace unavailable: registered execution identity is incomplete")
		}
		run.Workspace, run.RegisteredJob = job.Workspace, raw
		out = append(out, run)
	}
	return out, rows.Err()
}
