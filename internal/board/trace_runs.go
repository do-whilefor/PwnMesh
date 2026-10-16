package board

import (
	"encoding/json"

	"pwnmesh/internal/board/internal/sqlstore"
	"pwnmesh/internal/board/model"
)

// TraceRun is an internal capability descriptor, not a model-visible record.
// Workspace and JobSHA256 bind the registered execution to its local journal.
type TraceRun = model.TraceRun

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
	out := []TraceRun{}
	err = sqlstore.VisitTraceRuns(t.transactionQueries, project, generation, offset, limit, runID, func(run TraceRun) error {
		var job struct {
			RunID     string  `json:"run_id"`
			Kind      string  `json:"kind"`
			Workspace string  `json:"workspace"`
			Graph     Graph   `json:"graph"`
			Intent    *Intent `json:"intent"`
		}
		// Malformed or legacy unbound jobs never become file capabilities.
		if json.Unmarshal(run.RegisteredJob, &job) != nil || job.RunID != run.RunID || job.Kind != "explore" || job.Graph.Project.ID != project || job.Graph.Project.Generation != generation || job.Intent == nil || job.Intent.ID != run.StepID || run.StepID == "" || job.Workspace == "" {
			return Err(409, "trace unavailable: registered execution identity is incomplete")
		}
		run.Workspace = job.Workspace
		out = append(out, run)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
