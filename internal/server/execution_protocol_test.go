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
	store, err := board.Open(filepath.Join(t.TempDir(), "protocol.db"))
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
	return f.requestWithHandler(f.handler, method, path, body, fenced, want, out)
}

func (f *executionProtocolFixture) requestWithHandler(handler http.Handler, method, path string, body any, fenced bool, want int, out any) string {
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
	handler.ServeHTTP(w, r)
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

// Seed a current immutable inline job for focused HTTP application tests.
// Production callers use server-owned snapshot preparation.
func (f *executionProtocolFixture) registerInline(body any, want int) string {
	f.t.Helper()
	server := &Server{Store: f.store}
	handler := server.wrap(func(tx *board.Tx, q *request, r *http.Request) (int, any, error) {
		var execution board.Execution
		if err := decodeFields(q, &execution); err != nil {
			return 0, nil, board.Err(422, "Invalid execution")
		}
		execution.ProjectID = f.project
		return server.registerExecution(tx, execution, r)
	})
	return f.requestWithHandler(handler, "POST", f.base()+"/executions", body, true, want, nil)
}

func (f *executionProtocolFixture) executionRecords() []board.Execution {
	f.t.Helper()
	var executions []board.Execution
	err := f.store.Do(context.Background(), func(tx *board.Tx) error {
		rows, err := tx.Query("SELECT project_id,id FROM xloom_executions WHERE namespace=? ORDER BY created_at,rowid", "protocol-test")
		if err != nil {
			return err
		}
		var identities [][2]string
		for rows.Next() {
			var identity [2]string
			if err := rows.Scan(&identity[0], &identity[1]); err != nil {
				rows.Close()
				return err
			}
			identities = append(identities, identity)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, identity := range identities {
			execution, err := tx.Execution(identity[0], identity[1])
			if err != nil {
				return err
			}
			executions = append(executions, execution)
		}
		return nil
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
	var intent board.Intent
	// Seed unrelated work through the board command. HTTP planning itself is
	// covered by the decision-batch suite; fixture creation needs no extra run.
	err := f.store.Do(context.Background(), func(tx *board.Tx) error {
		g, err := tx.Load(f.project)
		if err != nil {
			return err
		}
		previous := g.Project.Reason
		lease := "fixture-planner"
		g.Project.Reason = &board.Reason{Worker: lease, StartedAt: tx.Now, Heartbeat: tx.Now}
		if err = tx.Save(g); err != nil {
			return err
		}
		key := fmt.Sprintf("fixture-step-%d", len(g.Intents))
		description := "Observe the synthetic fixture " + key
		if len(descriptions) > 0 {
			description = descriptions[0]
		}
		payload, _ := json.Marshal(map[string]any{"action": "add", "from": []string{"origin"}, "description": description})
		created, err := tx.StateAction(f.project, board.ExecutionFence{Run: lease, Lease: "reason"}, board.StateAction{Op: "step", IdempotencyKey: key, Payload: payload})
		if err != nil {
			return err
		}
		g, err = tx.Load(f.project)
		if err != nil {
			return err
		}
		g.Project.Reason = previous
		for _, step := range g.Intents {
			if step.ID == created.ID {
				intent = step
			}
		}
		return tx.Save(g)
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return intent
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

func (f *executionProtocolFixture) planAction(op, key string, payload any) board.StateActionResult {
	f.t.Helper()
	f.kind, f.intent = "reason", ""
	f.run = fmt.Sprintf("fixture-plan-%d", f.state().Revision)
	f.lease = "planner@" + f.run
	prepareSnapshot(f.t, f, snapshotTemplate(f, "reason"))
	raw, _ := json.Marshal(payload)
	result := f.decision("commit", f.batch(board.DecisionAction{Op: op, Payload: raw}), http.StatusOK)
	return result.Results[0]
}

// nil preserves the old job format that omitted graph_rpc entirely.
func (f *executionProtocolFixture) register(kind string, graphRPC *bool, version int) {
	f.t.Helper()
	f.registerWithFields(kind, graphRPC, version, nil, http.StatusCreated)
}

func (f *executionProtocolFixture) registerWithFields(kind string, graphRPC *bool, version int, fields map[string]any, want int) {
	f.t.Helper()
	f.kind = kind
	var intent *board.Intent
	if kind == "reason" {
		f.request("POST", f.base()+"/reason/claim", map[string]string{"worker": f.lease, "trigger": "initial"}, false, http.StatusOK, nil)
	} else {
		created := f.newIntent()
		f.intent = created.ID
		f.request("POST", f.base()+"/intents/"+created.ID+"/heartbeat", map[string]string{"worker": f.lease}, false, http.StatusOK, &created)
		intent = &created
	}
	state := f.state()
	job := map[string]any{
		"run_id": f.run, "kind": kind, "workspace": "/workspace",
		"graph": state.Graph, "state": state, "intent": intent,
		"budget": map[string]int{"max_intents": 3, "conclude_timeout": 60},
	}
	if graphRPC != nil {
		job["graph_rpc"] = *graphRPC
	}
	if version != 0 {
		job["result_contract_version"] = version
	}
	if kind == "reason" {
		job["decision"] = map[string]any{"version": 2, "state_version": board.DecisionStateVersion(state)}
	}
	for key, value := range fields {
		job[key] = value
	}
	raw, err := json.Marshal(job)
	if err != nil {
		f.t.Fatal(err)
	}
	key := "reason:protocol-fixture"
	if kind != "reason" {
		key = kind + ":" + f.intent
	}
	e := board.Execution{ProjectID: f.project, ID: f.run, Namespace: "protocol-test", Backend: "planner", Kind: kind, Intent: f.intent, Lease: f.lease, Job: raw, RetryKey: key}
	f.registerInline(e, want)
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
	for _, kind := range []string{"explore"} {
		for _, outcome := range []string{"continue", "incomplete"} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				f := newExecutionProtocolFixture(t)
				live := true
				f.register(kind, &live, 2)
				f.pending(`{"accepted":true,"outcome":"` + outcome + `","reason":"Required fixture reads remain"}`)
				f.apply(http.StatusUnprocessableEntity)
				state := f.state()
				if len(state.Graph.Facts) != 2 || len(state.Steps) != 1 || state.Steps[0].Result != nil || state.Steps[0].Status != "running" || state.Graph.Project.Status != "active" {
					t.Fatal("non-completed outcome produced a fact or completed the task")
				}
			})
		}
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
			live := true
			f.register("explore", &live, 2)
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

func TestExecutionRegistrationRejectsInvalidProtocolFields(t *testing.T) {
	for _, tt := range invalidExecutionProtocols {
		t.Run(tt.name, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			live := true
			f.registerWithFields("reason", &live, 2, map[string]any{tt.field: tt.value}, http.StatusUnprocessableEntity)
			var executions []board.Execution
			executions = f.executionRecords()
			if len(executions) != 0 {
				t.Fatal("invalid protocol was saved as a resumable execution")
			}
		})
	}
}

func TestRegisteredExecutionContractCannotBeDowngraded(t *testing.T) {
	for _, kind := range []string{"explore"} {
		for _, live := range []bool{true} {
			name := kind + "/compatibility_graph"
			if live {
				name = kind + "/live_graph"
			}
			t.Run(name, func(t *testing.T) {
				f := newExecutionProtocolFixture(t)
				f.register(kind, &live, 2)
				output := `{"accepted":true,"data":{"description":"19 of 30 chunks checked"}}`
				if kind == "bootstrap" {
					output = `{"accepted":true,"data":{"fact":{"description":"19 of 30 chunks checked"},"complete":{"description":"Worker exited"}}}`
				}
				before := f.state()
				f.pending(output)
				// apply() deliberately asks to use the old result protocol. Only
				// the registered job is authoritative, regardless of graph mode.
				f.apply(http.StatusUnprocessableEntity)
				after := f.state()
				if after.Revision != before.Revision || len(after.Graph.Facts) != len(before.Graph.Facts) || after.Steps[0].Status != "running" || after.Graph.Project.Status != "active" {
					t.Fatal("unversioned partial output was applied as completion")
				}
			})
		}
	}
}
