package server

import (
	"net/http"

	b "pwnmesh/internal/board"
	"pwnmesh/internal/protocol"
)

// Authorize retained process reads using the caller's current execution lease.
// Paths and source identities come from registered Jobs, never model arguments.
func (s *Server) executionTraces(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	var read protocol.GraphRequest
	if err := decodeStrictFields(q, &read); err != nil || read.Op != "read_trace_runs" {
		return 0, nil, b.Err(422, "expected a read_trace_runs request")
	}
	e, err := t.Execution(r.PathValue("pid"), r.PathValue("rid"))
	if err != nil {
		return 0, nil, err
	}
	if decisionFence(r) != e.Fence() {
		return 0, nil, b.Err(403, "Trace read requires its execution lease")
	}
	if e.Kind != "explore" || e.Intent == "" || !e.Pending() || e.Status == "result_pending" {
		return 0, nil, b.Err(409, "Trace read requires an active Execute run")
	}
	job, _, err := executionReadState(t, e, r, read, "Trace read belongs to another execution or project round")
	if err != nil {
		return 0, nil, err
	}
	limit := read.Limit
	if limit == 0 {
		limit = 10
	}
	runID := ""
	if len(read.IDs) == 1 {
		runID = read.IDs[0]
	}
	items, err := t.TraceRuns(e.ProjectID, job.Graph.Project.Generation, read.Offset, limit, runID)
	if err != nil {
		return 0, nil, err
	}
	for i := range items {
		items[i], err = protocol.BindTraceRun(items[i])
		if err != nil {
			return 0, nil, err
		}
	}
	page := struct {
		Items      []b.TraceRun `json:"items"`
		NextOffset *int         `json:"next_offset,omitempty"`
	}{Items: items}
	if runID == "" && len(items) == limit {
		next := read.Offset + len(items)
		page.NextOffset = &next
	}
	return http.StatusOK, page, nil
}
