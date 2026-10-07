package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

// Use real HTTP writes for both observations and decisions so these checks also
// cover the compatibility graph, source IDs, leases and the transaction gates.
type stepInvalidationFixture struct {
	*executionProtocolFixture
	decider    *executionProtocolFixture
	source     string
	correction string
}

func newStepInvalidationFixture(t *testing.T) *stepInvalidationFixture {
	t.Helper()
	worker := newExecutionProtocolFixture(t)
	facts := []string{}
	for _, description := range []string{"The test endpoint appears public", "A fresh session is denied access to the test endpoint"} {
		intent := worker.newIntent()
		result := worker.completeStep(intent, description)
		facts = append(facts, result.Fact.ID)
	}
	decider := *worker
	decider.run, decider.lease = "invalidation-decider", "planner@invalidation-decider"
	step := decider.planAction("step", "dependent-direction", map[string]any{
		"action": "add", "from": []string{facts[0]}, "description": "Check the public endpoint's response scope",
	})
	worker.kind, worker.intent = "explore", step.ID
	return &stepInvalidationFixture{worker, &decider, facts[0], facts[1]}
}

func (f *stepInvalidationFixture) refute() {
	f.t.Helper()
	f.curateRelations(map[string]any{
		"kind": "refutes", "source": f.correction, "target": f.source,
		"reason": "The earlier observation used an already authenticated session",
	})
}

func (f *stepInvalidationFixture) abandonWithReplacement(reason string) {
	f.t.Helper()
	planner := *f.decider
	planner.kind, planner.intent = "reason", ""
	planner.run = "fixture-abandon-" + f.intent
	planner.lease = "planner@" + planner.run
	_, job := prepareSnapshot(f.t, &planner, snapshotTemplate(&planner, "reason"))
	abandon, _ := json.Marshal(map[string]string{"action": "abandon", "id": f.intent, "reason": reason})
	replacement, _ := json.Marshal(map[string]any{"action": "add", "from": []string{f.correction}, "description": "Check the remaining requirement using the corrected observation"})
	batch := planner.batch(board.DecisionAction{Op: "step", Payload: abandon}, board.DecisionAction{Op: "step", Payload: replacement})
	if job.Decision.ClosureProtocol == 1 {
		batch.Assessment = &board.RootAssessment{Status: "missing", Description: "The invalid direction is replaced by a check using the corrected observation", Gaps: []board.RequirementGap{{ID: "fixture", InputIDs: []string{"goal"}, Description: "Verify the outstanding fixture condition"}}}
		batch.Actions[1].GapID = "fixture"
	}
	planner.decision("commit", batch, http.StatusOK)
}

func (f *stepInvalidationFixture) claim(want int) string {
	f.t.Helper()
	return f.request("POST", f.base()+"/intents/"+f.intent+"/heartbeat", map[string]string{"worker": f.lease}, false, want, nil)
}

func (f *stepInvalidationFixture) registration() board.Execution {
	f.t.Helper()
	state := f.state()
	var intent board.Intent
	for _, candidate := range state.Graph.Intents {
		if candidate.ID == f.intent {
			intent = candidate
		}
	}
	job, err := json.Marshal(map[string]any{
		"run_id": f.run, "kind": f.kind, "workspace": "/workspace",
		"graph": state.Graph, "state": state, "intent": intent,
		"graph_rpc": true, "result_contract_version": 2,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return board.Execution{ProjectID: f.project, ID: f.run, Namespace: "protocol-test", Backend: "planner", Kind: f.kind, Intent: f.intent, Lease: f.lease, Job: job, RetryKey: "explore:" + f.intent}
}

func (f *stepInvalidationFixture) step() board.Step {
	f.t.Helper()
	for _, step := range f.state().Steps {
		if step.ID == f.intent {
			return step
		}
	}
	f.t.Fatal("dependent step disappeared")
	return board.Step{}
}

func (f *stepInvalidationFixture) execution() *board.Execution {
	f.t.Helper()
	var executions []board.Execution
	executions = f.executionRecords()
	for _, execution := range executions {
		if execution.ID == f.run {
			return &execution
		}
	}
	return nil
}

func TestStepInvalidationRejectsNewHTTPClaim(t *testing.T) {
	f := newStepInvalidationFixture(t)
	f.refute()
	body := f.claim(http.StatusConflict)
	if !strings.Contains(body, "not effective evidence") {
		t.Fatalf("claim failed for an unrelated reason: %s", body)
	}
	step := f.step()
	if step.Worker != nil || step.Status != "needs_review" || len(step.InvalidSources) != 1 || step.InvalidSources[0] != f.source {
		t.Fatalf("claim either acquired invalid work or hid its cause: %+v", step)
	}
}

func TestStepInvalidationRejectsHTTPRegistrationAfterClaim(t *testing.T) {
	f := newStepInvalidationFixture(t)
	f.claim(http.StatusOK)
	job := f.registration() // Immutable input was read while its premise was valid.
	f.refute()
	body := f.registerInline(job, http.StatusConflict)
	if !strings.Contains(body, "not effective evidence") {
		t.Fatalf("registration failed for an unrelated reason: %s", body)
	}
	if stored := f.execution(); stored != nil {
		t.Fatalf("rejected registration created an execution: %+v", stored)
	}
}

func TestStepInvalidationRejectsHTTPStartAfterRegistration(t *testing.T) {
	f := newStepInvalidationFixture(t)
	f.claim(http.StatusOK)
	f.registerInline(f.registration(), http.StatusCreated)
	f.refute()
	body := f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]string{"status": "running"}, true, http.StatusConflict, nil)
	if !strings.Contains(body, "not effective evidence") {
		t.Fatalf("start failed for an unrelated reason: %s", body)
	}
	if stored := f.execution(); stored == nil || stored.Status != "prepared" {
		t.Fatalf("rejected start changed durable execution status: %+v", stored)
	}
}

func TestStepInvalidationHeartbeatRequiresRunningRegisteredOwner(t *testing.T) {
	for _, phase := range []string{"unregistered", "prepared", "running", "retryable", "result_pending"} {
		t.Run(phase, func(t *testing.T) {
			f := newStepInvalidationFixture(t)
			f.claim(http.StatusOK)
			if phase != "unregistered" {
				f.registerInline(f.registration(), http.StatusCreated)
				if phase != "prepared" {
					f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]string{"status": "running"}, true, http.StatusOK, nil)
				}
				if phase == "retryable" {
					f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]string{"status": "retryable"}, true, http.StatusOK, nil)
				} else if phase == "result_pending" {
					f.pending(`{"accepted":false,"reason":"No completed result"}`)
				}
			}
			f.refute()
			want := http.StatusConflict
			if phase == "running" || phase == "result_pending" {
				want = http.StatusOK
			}
			f.request("POST", f.base()+"/intents/"+f.intent+"/heartbeat", map[string]string{"worker": f.lease}, true, want, nil)
			f.claim(http.StatusConflict)
			other := *f.executionProtocolFixture
			other.run, other.lease = "other-run", "planner@other-run"
			other.request("POST", other.base()+"/intents/"+other.intent+"/heartbeat", map[string]string{"worker": other.lease}, true, http.StatusConflict, nil)
			if got := f.step(); board.Value(got.Worker) != f.lease || len(got.InvalidSources) != 1 {
				t.Fatalf("heartbeat changed ownership or hid the corrected premise: %+v", got)
			}
		})
	}
}

func TestStepInvalidationKeepsRunningObservationUntilHTTPAbandon(t *testing.T) {
	f := newStepInvalidationFixture(t)
	f.claim(http.StatusOK)
	f.registerInline(f.registration(), http.StatusCreated)
	f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]string{"status": "running"}, true, http.StatusOK, nil)
	f.refute()
	step := f.step()
	if step.Status != "running" || len(step.InvalidSources) != 1 || step.InvalidSources[0] != f.source || board.Value(step.Worker) != f.lease {
		t.Fatalf("correction silently stopped running work or hid its invalid source: %+v", step)
	}
	f.claim(http.StatusConflict) // An unfenced claim still requires valid premises.
	f.request("POST", f.base()+"/intents/"+f.intent+"/heartbeat", map[string]string{"worker": f.lease}, true, http.StatusOK, nil)
	payload := map[string]any{
		"description": "An independent request shows a rate-limit header", "scope": "One test endpoint response",
		"observed_at": "2026-09-22T10:00:00Z", "evidence": []map[string]string{{
			"run_id": f.run, "path": "/runs/" + f.run + "/evidence.txt", "excerpt": "RateLimit-Limit: 10",
		}},
	}
	observed := f.action("fact", "independent-observation", payload)
	f.abandonWithReplacement("The corrected premise makes further work on this direction unnecessary")
	for _, key := range []string{"late-observation", "independent-observation"} {
		f.request("POST", f.base()+"/state/actions", map[string]any{"op": "fact", "idempotency_key": key, "payload": payload}, true, http.StatusConflict, nil)
	}
	state := f.state()
	if err := state.ValidateFactSources([]string{observed.ID}, true); err != nil {
		t.Fatalf("abandonment invalidated independently observed evidence: %v", err)
	}
	for _, fact := range state.FactRecords {
		if fact.ID == observed.ID && (fact.SourceStepID != f.intent || fact.RunID != f.lease) {
			t.Fatalf("independent observation lost its origin: %+v", fact)
		}
	}
	if f.step().Status != "abandoned" {
		t.Fatal("late evidence replay changed the abandoned step")
	}
}

func TestStepInvalidationAllowsExistingHTTPRegistrationReplay(t *testing.T) {
	f := newStepInvalidationFixture(t)
	f.claim(http.StatusOK)
	registration := f.registration()
	f.registerInline(registration, http.StatusCreated)
	f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]string{"status": "running"}, true, http.StatusOK, nil)
	f.refute()
	f.registerInline(registration, http.StatusCreated)
	if stored := f.execution(); stored == nil || stored.Status != "running" || stored.Resumes != 0 {
		t.Fatalf("registration replay restarted or mutated the existing execution: %+v", stored)
	}
}

func TestStepInvalidationRejectsPendingHTTPResultRecovery(t *testing.T) {
	f := newStepInvalidationFixture(t)
	f.claim(http.StatusOK)
	f.registerInline(f.registration(), http.StatusCreated)
	f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]string{"status": "running"}, true, http.StatusOK, nil)
	raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": evidenceFixtureFact(f.run)}})
	f.pending(string(raw))
	before := f.execution()
	f.refute()
	f.request("POST", f.base()+"/executions/"+f.run+"/resume", map[string]any{}, true, http.StatusConflict, nil)
	after := f.execution()
	if after == nil || after.Status != "result_pending" || after.Resumes != before.Resumes || string(after.Job) != string(before.Job) || string(after.Result) != string(before.Result) {
		t.Fatalf("recovering pending delivery changed its original contract or result: before=%+v after=%+v", before, after)
	}
	f.apply(http.StatusConflict)
	f.apply(http.StatusConflict)
	if f.step().Result != nil {
		t.Fatal("an invalidated execution published its pending observation")
	}
}
