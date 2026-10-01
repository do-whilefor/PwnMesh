package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

func dependencyHTTPPlan(t *testing.T) (*executionProtocolFixture, string, string) {
	t.Helper()
	f := newCurationHTTPFixture(t)
	_, planner := prepareSnapshot(t, f, snapshotTemplate(f, "reason"))
	var plan board.DecisionReceipt
	f.request("POST", f.base()+"/state/decisions/commit", board.DecisionBatch{
		ExpectedVersion: planner.Decision.StateVersion,
		Actions: []board.DecisionAction{
			{Op: "step", Ref: "produce", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"Produce the checked input"}`)},
			{Op: "step", Ref: "consume", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"Consume the checked input","depends_on":["$produce"]}`)},
		},
	}, true, http.StatusOK, &plan)
	return f, plan.IDs["produce"], plan.IDs["consume"]
}

func dependencyHTTPTemplate(f *executionProtocolFixture, step, run string) board.Execution {
	f.t.Helper()
	f.kind, f.intent, f.run, f.lease = "explore", step, run, "planner@"+run
	var intent board.Intent
	f.request("POST", f.base()+"/intents/"+step+"/heartbeat", map[string]string{"worker": f.lease}, false, http.StatusOK, &intent)
	job, _ := json.Marshal(worker.Job{RunID: run, Kind: "explore", Workspace: "/workspace", GraphRPC: true, ResultContractVersion: 2, Graph: board.Graph{Project: f.state().Graph.Project}, Intent: &intent})
	return board.Execution{ProjectID: f.project, ID: run, Namespace: "protocol-test", Backend: "planner", Kind: "explore", Intent: step, Lease: f.lease, Job: job}
}

func completeDependencyHTTPProducer(t *testing.T, f *executionProtocolFixture, step string) string {
	t.Helper()
	prepareSnapshot(t, f, dependencyHTTPTemplate(f, step, "producer"))
	fact := f.action("fact", "producer-evidence", evidenceFixtureFact(f.run))
	raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]string{"fact_id": fact.ID}})
	f.pending(string(raw))
	f.apply(http.StatusOK)
	return fact.ID
}

// Inject a durable correction at the storage boundary. New-mode role APIs do
// not yet expose relation editing; readiness must still fail closed when a
// retained premise is corrected by a future curator or migration.
func invalidateDependencyHTTPFact(t *testing.T, f *executionProtocolFixture, id string) {
	t.Helper()
	err := f.store.Do(context.Background(), func(tx *board.Tx) error {
		_, err := tx.Exec(`UPDATE xloom_state SET data=json_set(data,
			'$.facts['||(SELECT key FROM json_each(data,'$.facts') WHERE json_extract(value,'$.id')=?)||'].status','refuted'),
			revision=revision+1,decision_revision=decision_revision+1 WHERE project_id=?`, id, f.project)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDependencyHTTPPreparationBindsAcceptedUpstreamAndReplays(t *testing.T) {
	f, producer, consumer := dependencyHTTPPlan(t)
	f.kind, f.intent, f.run, f.lease = "explore", consumer, "consumer", "planner@consumer"
	f.request("POST", f.base()+"/intents/"+consumer+"/heartbeat", map[string]string{"worker": f.lease}, false, http.StatusConflict, nil)
	fact := completeDependencyHTTPProducer(t, f, producer)
	template := dependencyHTTPTemplate(f, consumer, "consumer")
	saved, job := prepareSnapshot(t, f, template)
	want := []board.DependencyResult{{StepID: producer, FactID: fact, RunID: "producer"}}
	if !reflect.DeepEqual(job.DependencyResults, want) {
		t.Fatalf("upstream result was not frozen by the server: got %+v, want %+v", job.DependencyResults, want)
	}
	var view struct {
		Steps []board.Step `json:"steps"`
	}
	if err := json.Unmarshal(job.InputView, &view); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, step := range view.Steps {
		found = found || step.ID == consumer && reflect.DeepEqual(step.DependsOn, []string{producer})
	}
	if !found {
		t.Fatal("bounded execution input omitted the current Step's dependency contract")
	}
	invalidateDependencyHTTPFact(t, f, fact)
	var replay board.Execution
	f.request("POST", f.base()+"/executions/prepare", template, true, http.StatusOK, &replay)
	if !reflect.DeepEqual(saved, replay) {
		t.Fatal("preparation replay silently rebound the upstream result")
	}
	f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]string{"status": "running"}, true, http.StatusConflict, nil)
}

func TestDependencyHTTPInvalidationStopsHeartbeatAndRejectsPendingSuccess(t *testing.T) {
	f, producer, consumer := dependencyHTTPPlan(t)
	fact := completeDependencyHTTPProducer(t, f, producer)
	prepareSnapshot(t, f, dependencyHTTPTemplate(f, consumer, "consumer"))
	observation := f.action("fact", "consumer-evidence", evidenceFixtureFact(f.run))
	raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]string{"fact_id": observation.ID}})
	f.pending(string(raw))
	invalidateDependencyHTTPFact(t, f, fact)
	response := f.request("POST", f.base()+"/intents/"+consumer+"/heartbeat", map[string]string{"worker": f.lease}, true, http.StatusConflict, nil)
	if !strings.Contains(response, "dependency_invalidated:") {
		t.Fatalf("heartbeat lost the invalidation cause: %s", response)
	}
	f.apply(http.StatusConflict)
	after := f.state()
	retained := false
	for _, record := range after.FactRecords {
		retained = retained || record.ID == observation.ID
	}
	for _, step := range after.Steps {
		if step.ID == consumer && (step.Result != nil || step.Status == "completed") {
			t.Fatal("late success bypassed invalidated dependencies")
		}
	}
	if !retained {
		t.Fatal("dependency invalidation deleted the independent raw observation")
	}
}

func TestDependencyHTTPInvalidationStopsRunningHeartbeat(t *testing.T) {
	f, producer, consumer := dependencyHTTPPlan(t)
	fact := completeDependencyHTTPProducer(t, f, producer)
	prepareSnapshot(t, f, dependencyHTTPTemplate(f, consumer, "consumer"))
	f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]string{"status": "running"}, true, http.StatusOK, nil)
	invalidateDependencyHTTPFact(t, f, fact)
	response := f.request("POST", f.base()+"/intents/"+consumer+"/heartbeat", map[string]string{"worker": f.lease}, true, http.StatusConflict, nil)
	if !strings.Contains(response, "dependency_invalidated:") {
		t.Fatalf("running heartbeat ignored its accepted upstream dependency: %s", response)
	}
}
