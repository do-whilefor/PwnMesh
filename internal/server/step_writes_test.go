package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

func TestStepWriteHTTPClaimSchedulingAndSuccessfulHandoff(t *testing.T) {
	f := newCurationHTTPFixture(t)
	_, planner := prepareSnapshot(t, f, snapshotTemplate(f, "reason"))
	var plan board.DecisionReceipt
	f.request("POST", f.base()+"/state/decisions/commit", board.DecisionBatch{
		ExpectedVersion: planner.Decision.StateVersion,
		Actions: []board.DecisionAction{
			{Op: "step", Ref: "writer", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"Produce shared report","write_paths":["/workspace/reports"]}`)},
			{Op: "step", Ref: "waiting", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"Update shared report","write_paths":["/workspace/reports/tmp/../report.json"]}`)},
			{Op: "step", Ref: "independent", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"Independent result","write_paths":["/workspace/reports-other/result.json"]}`)},
		},
	}, true, http.StatusOK, &plan)
	writer, waiting, independent := plan.IDs["writer"], plan.IDs["waiting"], plan.IDs["independent"]
	_, job := prepareSnapshot(t, f, dependencyHTTPTemplate(f, writer, "writer"))
	var view struct {
		Steps []board.Step `json:"steps"`
	}
	if err := json.Unmarshal(job.InputView, &view); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(view.Steps, func(step board.Step) bool {
		return step.ID == writer && slices.Equal(step.WritePaths, []string{"/workspace/reports"})
	}) {
		t.Fatal("Worker input omitted its shared write scope")
	}
	before := board.DecisionStateVersion(f.state())
	f.kind, f.intent, f.run, f.lease = "explore", waiting, "waiting", "planner@waiting"
	response := f.request("POST", f.base()+"/intents/"+waiting+"/heartbeat", map[string]string{"worker": f.lease}, false, http.StatusConflict, nil)
	if !strings.Contains(response, "write_conflict:") {
		t.Fatalf("claim did not explain its temporary write conflict: %s", response)
	}
	var scheduling board.SchedulePage
	f.request("GET", f.base()+"/scheduling?namespace=protocol-test", nil, false, http.StatusOK, &scheduling)
	if !scheduling.ExecutionChecks["explore:"+waiting].Blocked || scheduling.ExecutionChecks["explore:"+independent].Blocked {
		t.Fatalf("waiting writer prevented independent dispatch: %+v", scheduling.ExecutionChecks)
	}
	if board.DecisionStateVersion(f.state()) != before {
		t.Fatal("a temporary output conflict created a failed attempt or changed the plan")
	}
	prepareSnapshot(t, f, dependencyHTTPTemplate(f, independent, "independent"))
	f.kind, f.intent, f.run, f.lease = "explore", writer, "writer", "planner@writer"
	f.request("POST", f.base()+"/intents/"+writer+"/heartbeat", map[string]string{"worker": f.lease}, true, http.StatusOK, nil)
	f.request("POST", f.base()+"/executions/writer/status", map[string]string{"status": "running"}, true, http.StatusOK, nil)
	fact := f.action("fact", "written-result", evidenceFixtureFact(f.run))
	raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]string{"fact_id": fact.ID}})
	f.pending(string(raw))
	f.apply(http.StatusOK)
	// Historical worker metadata on the concluded writer must not retain its
	// reservation. A fresh writer can now claim while the independent task runs.
	prepareSnapshot(t, f, dependencyHTTPTemplate(f, waiting, "waiting"))
	f.request("POST", f.base()+"/executions/waiting/status", map[string]string{"status": "running"}, true, http.StatusOK, nil)
}
