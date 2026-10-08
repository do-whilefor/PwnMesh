package server

import (
	"net/http"
	"strings"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

func TestTraceReadHTTPRequiresCurrentExecutionFence(t *testing.T) {
	f, job := prepareUpdateFixture(t, "snapshot")
	path := f.base() + "/executions/" + f.run + "/traces/read"
	read := worker.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_trace_runs", Limit: 1}
	var page struct {
		Items      []board.TraceRun `json:"items"`
		NextOffset *int             `json:"next_offset"`
	}
	f.request("POST", path, read, true, http.StatusOK, &page)
	for _, item := range page.Items {
		if item.ProjectID != job.Graph.Project.ID || item.Generation != job.Graph.Project.Generation {
			t.Fatalf("foreign trace descriptor: %+v", item)
		}
	}
	f.request("POST", path, read, false, http.StatusForbidden, nil)
	read.IDs = []string{f.run}
	f.request("POST", path, read, true, http.StatusUnprocessableEntity, nil)
	read.IDs = nil
	read.AssetIDs = []string{"fabricated"}
	f.request("POST", path, read, true, http.StatusUnprocessableEntity, nil)
	read.AssetIDs = nil
	f.request("PUT", f.base()+"/status", map[string]string{"status": "stopped"}, false, http.StatusOK, nil)
	f.request("POST", path, read, true, http.StatusConflict, nil)
}
