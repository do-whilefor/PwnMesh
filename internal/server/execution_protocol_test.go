package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/board"
)

// Exercise the HTTP application boundary with real SQLite transactions. No
// Worker or model is involved: these are the outputs a faulty Worker could send.
type executionProtocolFixture struct {
	t       *testing.T
	handler http.Handler
	store   *board.Store
	project string
	run     string
	lease   string
	kind    string
	intent  string
}

func newExecutionProtocolFixture(t *testing.T) *executionProtocolFixture {
	t.Helper()
	store, err := openFixtureStore(t, filepath.Join(t.TempDir(), "protocol.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// A fixed clock keeps these tests independent of heartbeat timing.
	store.Now = func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) }
	f := &executionProtocolFixture{t: t, handler: New(store), store: store, run: "protocol-run", lease: "planner@protocol-run"}
	var graph board.Graph
	f.request("POST", "/projects", map[string]any{"title": "Protocol regression", "origin": "Synthetic local input", "goal": "Verify a synthetic result", "bootstrap_enabled": false}, false, http.StatusCreated, &graph)
	f.project = graph.Project.ID
	return f
}

func (f *executionProtocolFixture) base() string { return "/projects/" + f.project }

func (f *executionProtocolFixture) request(method, path string, body any, fenced bool, want int, out any) string {
	f.t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if fenced {
		r.Header.Set("X-PwnMesh-Run", f.lease)
		r.Header.Set("X-PwnMesh-Lease", f.kind)
		r.Header.Set("X-PwnMesh-Intent", f.intent)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != want {
		f.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	if out != nil {
		if err = json.Unmarshal(w.Body.Bytes(), out); err != nil {
			f.t.Fatalf("decode %s: %v", path, err)
		}
	}
	return w.Body.String()
}

func (f *executionProtocolFixture) prepare(body any, want int) string {
	f.t.Helper()
	return f.request("POST", f.base()+"/executions/prepare", body, true, want, nil)
}

func (f *executionProtocolFixture) executionRecords() []board.Execution {
	f.t.Helper()
	var executions []board.Execution
	err := fixtureSQL(f.store).Do(context.Background(), func(tx *fixtureSQLTx) error {
		rows, err := tx.Query("SELECT project_id,id,namespace,backend,kind,intent,lease,job,retry_key,status,result,resumes,created_at,updated_at FROM pwnmesh_executions WHERE namespace=? ORDER BY created_at,rowid", "protocol-test")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var execution board.Execution
			var job, result []byte
			if err := rows.Scan(&execution.ProjectID, &execution.ID, &execution.Namespace, &execution.Backend, &execution.Kind, &execution.Intent, &execution.Lease, &job, &execution.RetryKey, &execution.Status, &result, &execution.Resumes, &execution.CreatedAt, &execution.UpdatedAt); err != nil {
				return err
			}
			execution.Job, execution.Result = job, result
			executions = append(executions, execution)
		}
		return rows.Err()
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return executions
}

func (f *executionProtocolFixture) state() board.State {
	f.t.Helper()
	var state board.State
	f.request("GET", f.base()+"/state", nil, false, http.StatusOK, &state)
	return state
}

func (f *executionProtocolFixture) newIntent(descriptions ...string) board.Intent {
	f.t.Helper()
	// Authorize fixture work through a real prepared planner and committed
	// decision. Caller identity remains unchanged for the operation under test.
	planner := *f
	description := fmt.Sprintf("Observe the synthetic fixture step %d", len(f.state().Steps))
	if len(descriptions) > 0 {
		description = descriptions[0]
	}
	if len(f.state().Steps) > 0 {
		f.request("POST", f.base()+"/hints", map[string]string{"creator": "user", "content": fmt.Sprintf("Authorize fixture step %d", len(f.state().Steps)+1)}, false, http.StatusCreated, nil)
	}
	created := planner.planAction("step", map[string]any{"action": "add", "from": []string{"origin"}, "description": description})
	for _, intent := range f.state().Graph.Intents {
		if intent.ID == created.ID {
			return intent
		}
	}
	f.t.Fatal("committed fixture step is missing")
	return board.Intent{}
}

func (f *executionProtocolFixture) completeStep(intent board.Intent, description string) board.Conclusion {
	f.t.Helper()
	executor := *f
	executor.kind, executor.intent = "explore", intent.ID
	executor.run = "fixture-execution-" + intent.ID
	executor.lease = "planner@" + executor.run
	executor.request("POST", executor.base()+"/intents/"+intent.ID+"/heartbeat", map[string]string{"worker": executor.lease}, false, http.StatusOK, &intent)
	job, _ := json.Marshal(map[string]any{"run_id": executor.run, "kind": "explore", "workspace": "/workspace", "graph_rpc": true, "result_contract_version": 2, "graph": board.Graph{Project: f.state().Graph.Project}, "intent": intent})
	template := board.Execution{ProjectID: f.project, ID: executor.run, Namespace: "protocol-test", Backend: "planner", Kind: "explore", Intent: intent.ID, Lease: executor.lease, Job: job}
	prepareSnapshot(f.t, &executor, template)
	fact := evidenceFixtureFact(executor.run)
	fact["description"] = description
	raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": fact}})
	executor.pending(string(raw))
	executor.apply(http.StatusOK)
	state := executor.state()
	for _, completed := range state.Graph.Intents {
		if completed.ID == intent.ID {
			for _, observed := range state.Graph.Facts {
				if observed.ID == board.Value(completed.To) {
					return board.Conclusion{Intent: completed, Fact: observed}
				}
			}
		}
	}
	f.t.Fatal("fixture Execute did not publish its result")
	return board.Conclusion{}
}

func (f *executionProtocolFixture) curateRelations(relations ...map[string]any) {
	f.t.Helper()
	curator := *f
	curator.run = fmt.Sprintf("fixture-curator-%d", f.state().Revision)
	curator.lease = "planner@" + curator.run
	_, job := prepareCurator(&curator)
	curator.action("curate", "curation:"+curator.run, map[string]any{"through_revision": job.InputSnapshot.Revision, "groups": []any{}, "relations": relations})
}

func (f *executionProtocolFixture) completeProject(fact string) board.Intent {
	f.t.Helper()
	planner := *f
	planner.run, planner.lease, planner.intent = "fixture-completion", "planner@fixture-completion", ""
	prepareSnapshot(f.t, &planner, snapshotTemplate(&planner, "reason"))
	payload, _ := json.Marshal(map[string]any{"from": []string{fact}, "description": "Required fixture checked"})
	batch := planner.batch(board.DecisionAction{Op: "complete", Payload: payload})
	batch.Assessment = &board.RootAssessment{Status: "satisfied", From: []string{fact}, Description: "The retained response satisfies the original fixture requirement"}
	planner.decision("preview", batch, http.StatusOK)
	planner.decision("commit", batch, http.StatusOK)
	for _, intent := range planner.state().Graph.Intents {
		if board.Value(intent.To) == "goal" {
			return intent
		}
	}
	f.t.Fatal("fixture project was not completed")
	return board.Intent{}
}

func (f *executionProtocolFixture) planAction(op string, payload any) board.StateActionResult {
	f.t.Helper()
	f.kind, f.intent = "reason", ""
	f.run = fmt.Sprintf("fixture-plan-%d", f.state().Revision)
	f.lease = "planner@" + f.run
	template := snapshotTemplate(f, "reason")
	var configuration map[string]any
	if err := json.Unmarshal(template.Job, &configuration); err != nil {
		f.t.Fatal(err)
	}
	configuration["budget"] = map[string]int{"max_intents": len(f.state().Steps) + 1}
	template.Job, _ = json.Marshal(configuration)
	_, job := prepareSnapshot(f.t, f, template)
	raw, _ := json.Marshal(payload)
	batch := f.batch(board.DecisionAction{Op: op, Payload: raw})
	if job.Decision.ClosureProtocol == 1 {
		batch.Assessment = &board.RootAssessment{Status: "missing", Description: "The original fixture still requires a scoped follow-up", Gaps: []board.RequirementGap{{ID: "fixture", InputIDs: []string{"goal"}, Description: "Check the outstanding fixture condition"}}}
		var fields struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(raw, &fields)
		if op == "curation_request" || op == "step" && (fields.Action == "add" || fields.Action == "retry") || op == "goal" && fields.Action == "add" {
			batch.Actions[0].GapID = "fixture"
		}
	}
	result := f.decision("commit", batch, http.StatusOK)
	return result.Results[0]
}

func (f *executionProtocolFixture) register(kind string) {
	f.t.Helper()
	prepareSnapshot(f.t, f, snapshotTemplate(f, kind))
}

func (f *executionProtocolFixture) action(op, key string, payload any) board.StateActionResult {
	f.t.Helper()
	var receipt board.StateActionResult
	f.request("POST", f.base()+"/state/actions", map[string]any{
		"op": op, "idempotency_key": key, "payload": payload,
		"expected_version": board.DecisionStateVersion(f.state()),
	}, true, http.StatusOK, &receipt)
	return receipt
}

func (f *executionProtocolFixture) pending(text string) {
	f.t.Helper()
	result := map[string]any{"status": "success", "text": text, "state_version": board.DecisionStateVersion(f.state())}
	f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]any{"status": "result_pending", "result": result}, true, http.StatusOK, nil)
}

func (f *executionProtocolFixture) apply(want int) string {
	f.t.Helper()
	// These untrusted request fields must never override the persisted Job.
	return f.request("POST", f.base()+"/executions/"+f.run+"/apply", map[string]any{"graph_rpc": false, "result_contract_version": 0}, true, want, nil)
}

func TestNonCompletedWorkerOutcomeCannotBeAppliedAsSuccess(t *testing.T) {
	for _, outcome := range []string{"continue", "incomplete"} {
		t.Run(outcome, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			f.register("explore")
			f.pending(`{"accepted":true,"outcome":"` + outcome + `","reason":"Required fixture reads remain"}`)
			f.apply(http.StatusUnprocessableEntity)
			state := f.state()
			if len(state.Graph.Facts) != 2 || len(state.Steps) != 1 || state.Steps[0].Result != nil || state.Steps[0].Status != "running" || state.Graph.Project.Status != "active" {
				t.Fatal("non-completed outcome produced a fact or completed the task")
			}
		})
	}
}

func TestIncompleteExecutionFailureNeverCreatesFact(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "worker_failed"
		if pending {
			name = "rejected_pending_result"
		}
		t.Run(name, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			f.register("explore")
			output := `{"accepted":true,"outcome":"incomplete","reason":"Required fixture reads remain"}`
			if pending {
				f.pending(output)
				f.apply(http.StatusUnprocessableEntity)
			}
			failure := map[string]any{"status": "failed", "failure_kind": "incomplete", "error": "Required fixture reads remain", "text": output}
			f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]any{"status": "failed", "result": failure}, true, http.StatusOK, nil)
			f.apply(http.StatusConflict)
			state := f.state()
			if len(state.Graph.Facts) != 2 || len(state.Steps) != 1 || state.Steps[0].Status != "failed" || state.Steps[0].Result != nil || state.Graph.Project.Status != "active" {
				t.Fatal("incomplete failure created a fact or ended the project")
			}
			if !strings.Contains(state.Steps[0].Reason, "Required fixture reads remain") {
				t.Fatalf("incomplete diagnostic was lost: %q", state.Steps[0].Reason)
			}
		})
	}
}

var invalidExecutionProtocols = []struct {
	name, field string
	value       any
}{
	{"disabled_graph_mode", "graph_rpc", false},
	{"legacy_version", "result_contract_version", 0},
	{"outcome_only_version", "result_contract_version", 1},
	{"null_graph_mode", "graph_rpc", nil},
	{"string_graph_mode", "graph_rpc", "true"},
	{"numeric_graph_mode", "graph_rpc", 1},
	{"null_version", "result_contract_version", nil},
	{"string_version", "result_contract_version", "1"},
	{"boolean_version", "result_contract_version", true},
	{"negative_version", "result_contract_version", -1},
	{"fractional_version", "result_contract_version", 1.5},
	{"unsupported_version", "result_contract_version", 3},
}

func TestRegisteredExecutionContractCannotBeDowngraded(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.register("explore")
	before := f.state()
	f.pending(`{"accepted":true,"data":{"description":"19 of 30 chunks checked"}}`)
	// apply() deliberately asks to use the old result protocol. Only the
	// registered job is authoritative.
	f.apply(http.StatusUnprocessableEntity)
	after := f.state()
	if after.Revision != before.Revision || len(after.Graph.Facts) != len(before.Graph.Facts) || after.Steps[0].Status != "running" || after.Graph.Project.Status != "active" {
		t.Fatal("unversioned partial output was applied as completion")
	}
}
