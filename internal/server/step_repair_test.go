package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/board"
	"pwnmesh/internal/protocol"
)

func TestRepairPreparationBindsServerContractAndRejectsClientInjection(t *testing.T) {
	f := newCurationHTTPFixture(t)
	_, planner := prepareSnapshot(t, f, snapshotTemplate(f, "reason"))
	spec := &artifactcheck.Spec{Path: "/workspace/report.json", SHA256: strings.Repeat("a", 64), Rules: []artifactcheck.Rule{{Kind: "json_valid"}}}
	raw, _ := json.Marshal(map[string]any{"action": "add", "from": []string{"origin"}, "description": "repair report", "repair": spec})
	var plan board.DecisionReceipt
	f.request("POST", f.base()+"/state/decisions/commit", board.DecisionBatch{ExpectedVersion: planner.Decision.StateVersion, Actions: []board.DecisionAction{{Op: "step", Ref: "repair", Payload: raw}}}, true, http.StatusOK, &plan)
	template := dependencyHTTPTemplate(f, plan.IDs["repair"], "repair")
	var injected protocol.Job
	json.Unmarshal(template.Job, &injected)
	injected.Repair = spec
	bad := template
	bad.Job, _ = json.Marshal(injected)
	f.request("POST", f.base()+"/executions/prepare", bad, true, http.StatusUnprocessableEntity, nil)
	_, job := prepareSnapshot(t, f, template)
	if job.Repair == nil || job.Repair.Path != spec.Path || job.Repair.SHA256 != spec.SHA256 {
		t.Fatal("server dropped repair contract")
	}
	f.request("POST", f.base()+"/executions/repair/status", map[string]any{"status": "result_pending", "result": protocol.Result{Status: "success", Text: `{"accepted":true}`}}, true, http.StatusUnprocessableEntity, nil)
	f.request("POST", f.base()+"/executions/repair/status", map[string]any{"status": "failed", "result": protocol.Result{Status: "failed", FailureKind: "repair_stale"}}, true, http.StatusOK, nil)
	response := f.request("POST", f.base()+"/executions/repair/retry", map[string]any{}, false, http.StatusConflict, nil)
	if !strings.Contains(response, "stale repair requires a new assessed Step") {
		t.Fatalf("management retry was not rejected by the stale-contract guard: %s", response)
	}
}
