package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"pwnmesh/internal/board"
)

func TestUnsupportedProjectDownloadPreservesStoredBytes(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	want := []byte{0, 1, 2, 255, '\n'}
	var file board.InputFile
	if err := f.store.Do(context.Background(), func(tx *board.Tx) error {
		var err error
		file, err = tx.AddInput(f.project, "retained.bin", want)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixtureSQL(f.store).Do(context.Background(), func(tx *fixtureSQLTx) error {
		_, err := tx.Exec("UPDATE pwnmesh_project_orchestration SET version=0 WHERE project_id=?", f.project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "HEAD"} {
		f.request(method, f.base()+"/inputs/"+file.ID, nil, false, http.StatusConflict, nil)
	}
	if err := fixtureSQL(f.store).Do(context.Background(), func(tx *fixtureSQLTx) error {
		var data []byte
		if err := tx.QueryRow("SELECT data FROM pwnmesh_input_files WHERE project_id=? AND id=?", f.project, file.ID).Scan(&data); err != nil {
			return err
		}
		if !bytes.Equal(data, want) {
			t.Fatal("rejected download changed the retained input bytes")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedProjectRequestsPreserveStoredState(t *testing.T) {
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
		original = board.Execution{ProjectID: f.project, ID: f.run, Namespace: "protocol-test", Backend: "planner", Kind: "reason", Lease: f.lease, Job: job, RetryKey: "reason:historical", Status: "prepared", CreatedAt: tx.Now, UpdatedAt: tx.Now}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Retained records are imported storage, not newly authorized executions.
	if err := fixtureSQL(f.store).Do(context.Background(), func(tx *fixtureSQLTx) error {
		_, err := tx.Exec(`INSERT INTO pwnmesh_executions(project_id,id,namespace,backend,kind,intent,lease,job,retry_key,status,created_at,updated_at,fact_count,metadata_version)
			VALUES(?,?,?,?,?,'',?,?,?,?,?,?,2,1)`, original.ProjectID, original.ID, original.Namespace, original.Backend, original.Kind, original.Lease, []byte(original.Job), original.RetryKey, original.Status, original.CreatedAt, original.UpdatedAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	storedState := func() board.State {
		t.Helper()
		var state board.State
		if err := f.store.View(context.Background(), func(tx *board.Tx) error {
			var err error
			state, err = tx.State(f.project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := storedState()
	for _, method := range []string{"GET", "HEAD"} {
		for _, path := range []string{f.base(), f.base() + "/state", f.base() + "/export?format=timeline", f.base() + "/executions/" + f.run + "?namespace=protocol-test"} {
			f.request(method, path, nil, false, http.StatusConflict, nil)
		}
	}
	for _, operation := range []struct{ method, path string }{
		{"PUT", "/title"}, {"PUT", "/status"}, {"POST", "/hints"}, {"POST", "/reopen"}, {"POST", "/restart"}, {"POST", "/terminate"}, {"POST", "/reason/claim"}, {"POST", "/reason/heartbeat"}, {"POST", "/reason/release"}, {"POST", "/curate/claim"}, {"POST", "/state/actions"}, {"POST", "/state/decisions/commit"}, {"POST", "/executions/prepare"}, {"POST", "/executions/" + f.run + "/resume"}, {"POST", "/executions/" + f.run + "/status"}, {"POST", "/executions/" + f.run + "/apply"}, {"POST", "/executions/" + f.run + "/retry"}, {"DELETE", ""},
	} {
		f.request(operation.method, f.base()+operation.path, map[string]any{}, true, http.StatusConflict, nil)
	}
	if after := storedState(); !reflect.DeepEqual(after, before) || after.Graph.Project.OrchestrationVersion != 0 || after.Graph.Project.Reason == nil {
		t.Fatal("rejected access rewrote data, protocol or expired its saved lease")
	}
	var retained board.Execution
	if err := f.store.View(context.Background(), func(tx *board.Tx) error {
		var err error
		retained, err = tx.Execution(f.project, f.run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(retained, original) {
		t.Fatal("historical execution changed")
	}
}

func TestRetiredExecutionCannotResumeApplyOrRetryInCurrentProject(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.register("explore")
	var original board.Execution
	if err := f.store.View(context.Background(), func(tx *board.Tx) error {
		var err error
		original, err = tx.Execution(f.project, f.run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(original.Job, &job); err != nil {
		t.Fatal(err)
	}
	job["result_contract_version"] = 1
	raw, _ := json.Marshal(job)
	if err := fixtureSQL(f.store).Do(context.Background(), func(tx *fixtureSQLTx) error {
		_, err := tx.Exec("UPDATE pwnmesh_executions SET job=? WHERE project_id=? AND id=?", raw, f.project, f.run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	original.Job = raw

	for _, op := range []string{"resume", "status", "apply", "retry"} {
		f.request("POST", f.base()+"/executions/"+f.run+"/"+op, map[string]any{"status": "running"}, op != "retry", http.StatusConflict, nil)
	}
	// Even the idempotent apply path must reject a retired protocol before
	// returning an already-settled status.
	if err := fixtureSQL(f.store).Do(context.Background(), func(tx *fixtureSQLTx) error {
		_, err := tx.Exec("UPDATE pwnmesh_executions SET status='succeeded' WHERE project_id=? AND id=?", f.project, f.run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.request("POST", f.base()+"/executions/"+f.run+"/apply", map[string]any{}, true, http.StatusConflict, nil)
	if err := fixtureSQL(f.store).Do(context.Background(), func(tx *fixtureSQLTx) error {
		_, err := tx.Exec("UPDATE pwnmesh_executions SET status=? WHERE project_id=? AND id=?", original.Status, f.project, f.run)
		return err
	}); err != nil {
		t.Fatal(err)
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
	f.register("explore")
	before := f.state()
	for _, role := range []string{"bootstrap", "intent"} {
		f.kind = role
		f.request("POST", f.base()+"/state/actions", map[string]any{"op": "fact", "idempotency_key": "old-role", "payload": evidenceFixtureFact(f.run)}, true, http.StatusForbidden, nil)
		for _, op := range []string{"heartbeat", "release"} {
			f.request("POST", f.base()+"/intents/"+f.intent+"/"+op, map[string]string{"worker": f.lease, "description": "Old alias"}, true, http.StatusForbidden, nil)
		}
	}
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("retired lease role changed the current project")
	}
}
