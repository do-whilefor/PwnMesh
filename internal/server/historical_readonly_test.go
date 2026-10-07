package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"pwnmesh/internal/board"
)

func TestHistoricalProjectReadsAndRejectedWritesPreserveStoredState(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.project, f.run, f.lease, f.kind = "historical", "saved-run", "planner@saved-run", "reason"
	var original board.Execution
	var err error
	err = f.store.Do(context.Background(), func(tx *board.Tx) error {
		old := f.store.Now().Add(-time.Hour).Format(time.RFC3339)
		graph := board.Graph{Project: board.Project{ID: f.project, Title: "Retained project", Status: "active", Bootstrap: true, CreatedAt: old, Reason: &board.Reason{Worker: f.lease, StartedAt: old, Heartbeat: old}}, Facts: []board.Fact{{ID: "origin", Description: "Original input"}, {ID: "goal", Description: "Original goal"}}}
		if err := tx.Save(graph); err != nil {
			return err
		}
		job, _ := json.Marshal(map[string]any{"run_id": f.run, "kind": "reason", "graph": graph, "workspace": "/workspace"})
		original = board.Execution{ProjectID: f.project, ID: f.run, Namespace: "protocol-test", Backend: "planner", Kind: "reason", Lease: f.lease, Job: job, RetryKey: "reason:historical"}
		if err := tx.RegisterExecution(original); err != nil {
			return err
		}
		original, err = tx.Execution(f.project, f.run)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	before := f.state()
	for _, path := range []string{f.base(), f.base() + "/state", f.base() + "/export?format=timeline", f.base() + "/executions/" + f.run + "?namespace=protocol-test", "/projects", "/executions/pending?namespace=protocol-test"} {
		f.request("GET", path, nil, false, http.StatusOK, nil)
	}
	for _, operation := range []struct{ method, path string }{
		{"PUT", "/title"}, {"PUT", "/status"}, {"POST", "/hints"}, {"POST", "/intents"}, {"POST", "/complete"}, {"POST", "/reopen"}, {"POST", "/restart"}, {"POST", "/terminate"}, {"POST", "/reason/claim"}, {"POST", "/reason/heartbeat"}, {"POST", "/reason/release"}, {"POST", "/curate/claim"}, {"POST", "/state/actions"}, {"POST", "/state/decisions/commit"}, {"POST", "/executions/prepare"}, {"POST", "/executions/" + f.run + "/resume"}, {"POST", "/executions/" + f.run + "/status"}, {"POST", "/executions/" + f.run + "/apply"}, {"POST", "/executions/" + f.run + "/retry"}, {"DELETE", ""},
	} {
		f.request(operation.method, f.base()+operation.path, map[string]any{}, true, http.StatusConflict, nil)
	}
	if after := f.state(); !reflect.DeepEqual(after, before) || after.Graph.Project.OrchestrationVersion != 0 || after.Graph.Project.Reason == nil {
		t.Fatal("read-only historical access rewrote data, protocol or expired its saved lease")
	}
	var retained board.Execution
	f.request("GET", f.base()+"/executions/"+f.run+"?namespace=protocol-test", nil, false, http.StatusOK, &retained)
	if !reflect.DeepEqual(retained, original) {
		t.Fatal("historical execution changed")
	}
}

func TestRetiredExecutionCannotResumeApplyOrRetryInCurrentProject(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	live := true
	f.register("explore", &live, 2)
	var original board.Execution
	if err := f.store.Do(context.Background(), func(tx *board.Tx) error {
		e, err := tx.Execution(f.project, f.run)
		if err != nil {
			return err
		}
		var job map[string]any
		if err = json.Unmarshal(e.Job, &job); err != nil {
			return err
		}
		job["result_contract_version"] = 1
		raw, _ := json.Marshal(job)
		if _, err = tx.Exec("UPDATE xloom_executions SET job=? WHERE project_id=? AND id=?", raw, f.project, f.run); err != nil {
			return err
		}
		original, err = tx.Execution(f.project, f.run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"resume", "status", "apply", "retry"} {
		f.request("POST", f.base()+"/executions/"+f.run+"/"+op, map[string]any{"status": "running"}, op != "retry", http.StatusConflict, nil)
	}
	f.request("POST", f.base()+"/executions/"+f.run+"/retry", map[string]any{"automatic": true}, false, http.StatusConflict, nil)
	var retained board.Execution
	f.request("GET", f.base()+"/executions/"+f.run+"?namespace=protocol-test", nil, false, http.StatusOK, &retained)
	if !reflect.DeepEqual(retained, original) {
		t.Fatal("retired protocol execution changed")
	}
}

func TestRetiredLeaseRolesCannotWriteCurrentProject(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	live := true
	f.register("explore", &live, 2)
	before := f.state()
	for _, role := range []string{"bootstrap", "intent"} {
		f.kind = role
		f.request("POST", f.base()+"/state/actions", map[string]any{"op": "fact", "idempotency_key": "old-role", "payload": evidenceFixtureFact(f.run)}, true, http.StatusForbidden, nil)
		for _, op := range []string{"heartbeat", "release", "conclude"} {
			f.request("POST", f.base()+"/intents/"+f.intent+"/"+op, map[string]string{"worker": f.lease, "description": "Old alias"}, true, http.StatusForbidden, nil)
		}
	}
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("retired lease role changed the current project")
	}
}
