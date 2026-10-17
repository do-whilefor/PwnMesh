package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/protocol"
)

func TestTraceReadHTTPRequiresCurrentExecutionFence(t *testing.T) {
	f, job := prepareUpdateFixture(t, "snapshot")
	path := f.base() + "/executions/" + f.run + "/traces/read"
	read := protocol.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_trace_runs", Limit: 1}
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
	page.Items, page.NextOffset = nil, nil
	f.request("POST", path, read, true, http.StatusOK, &page)
	if len(page.Items) != 1 || page.Items[0].RunID != f.run || page.NextOffset != nil {
		t.Fatalf("current execution trace was not selected exactly: %+v", page)
	}
	f.request("POST", path, read, false, http.StatusForbidden, nil)
	read.IDs = []string{f.run, "another-run"}
	f.request("POST", path, read, true, http.StatusUnprocessableEntity, nil)
	read.IDs = nil
	read.AssetIDs = []string{"fabricated"}
	f.request("POST", path, read, true, http.StatusUnprocessableEntity, nil)
	read.AssetIDs = nil
	f.request("PUT", f.base()+"/status", map[string]string{"status": "stopped"}, false, http.StatusOK, nil)
	f.request("POST", path, read, true, http.StatusConflict, nil)
}

func TestExecutionReadsRejectForeignRegisteredJob(t *testing.T) {
	for _, endpoint := range []struct{ path, op, detail string }{
		{"updates", "read_updates", "Updates input belongs to another execution or project round"},
		{"traces/read", "read_trace_runs", "Trace read belongs to another execution or project round"},
	} {
		for _, identity := range []string{"project", "generation", "run"} {
			t.Run(endpoint.op+"/"+identity, func(t *testing.T) {
				f, job := prepareUpdateFixture(t, "snapshot")
				switch identity {
				case "project":
					job.Graph.Project.ID = "another-project"
				case "generation":
					job.Graph.Project.Generation++
				case "run":
					job.RunID = "another-run"
				}
				raw, err := json.Marshal(job)
				if err != nil {
					t.Fatal(err)
				}
				if err = fixtureSQL(f.store).Do(context.Background(), func(tx *fixtureSQLTx) error {
					_, err := tx.Exec("UPDATE pwnmesh_executions SET job=? WHERE project_id=? AND id=?", raw, f.project, f.run)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				read := protocol.GraphRequest{RequestID: strings.Repeat("a", 32), Op: endpoint.op}
				body := f.request("POST", f.base()+"/executions/"+f.run+"/"+endpoint.path, read, true, http.StatusConflict, nil)
				if !strings.Contains(body, endpoint.detail) {
					t.Fatalf("identity rejection changed: %s", body)
				}
			})
		}
	}
}
