package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

func TestReasonHeartbeatVersionIgnoresLeaseActivity(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	intent := f.newIntent()
	f.request("POST", f.base()+"/hints", map[string]string{"creator": "user", "content": "Review the pending fixture while its lease changes"}, false, http.StatusCreated, nil)
	f.register("reason")
	version := board.DecisionStateVersion(f.state())
	body := map[string]string{"worker": f.lease, "expected_version": version}
	for _, op := range []string{"heartbeat", "heartbeat", "release", "heartbeat"} {
		f.request("POST", f.base()+"/intents/"+intent.ID+"/"+op, map[string]string{"worker": "parallel-executor"}, false, http.StatusOK, nil)
		f.request("POST", f.base()+"/reason/heartbeat", body, true, http.StatusOK, nil)
		if got := board.DecisionStateVersion(f.state()); got != version {
			t.Fatalf("%s changed decision version: %s != %s", op, got, version)
		}
	}
}

func TestReasonLeaseRejectsMismatchedExecutionIdentity(t *testing.T) {
	for _, op := range []string{"claim", "heartbeat", "release"} {
		for _, mismatch := range []string{"worker", "role", "intent"} {
			t.Run(op+"/"+mismatch, func(t *testing.T) {
				f := newDecisionBatchFixture(t)
				before := f.state()
				body := map[string]string{"worker": f.lease, "trigger": "initial"}
				switch mismatch {
				case "worker":
					f.lease = "different-execution"
				case "role":
					f.kind = "explore"
				case "intent":
					f.intent = "different-step"
				}
				f.request("POST", f.base()+"/reason/"+op, body, true, http.StatusForbidden, nil)
				if !reflect.DeepEqual(before, f.state()) {
					t.Fatal("mismatched execution identity changed the planner lease")
				}
			})
		}
	}
}

func TestReasonHeartbeatDetectsChangedInputWithoutPublishingState(t *testing.T) {
	for _, change := range []string{"hint", "step_completed"} {
		t.Run(change, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			intent := f.newIntent()
			f.request("POST", f.base()+"/hints", map[string]string{"creator": "user", "content": "Review the pending fixture before more evidence arrives"}, false, http.StatusCreated, nil)
			f.register("reason")
			version := board.DecisionStateVersion(f.state())
			if change == "hint" {
				f.request("POST", f.base()+"/hints", map[string]string{"creator": "user", "content": "New evidence changes the plan"}, false, http.StatusCreated, nil)
			} else {
				f.completeStep(intent, "Observed the synthetic result")
			}
			before := f.state()
			response := f.request("POST", f.base()+"/reason/heartbeat", map[string]string{"worker": f.lease, "expected_version": version}, true, http.StatusConflict, nil)
			var problem struct{ Detail string }
			if err := json.Unmarshal([]byte(response), &problem); err != nil || !strings.HasPrefix(problem.Detail, "state_changed:") {
				t.Fatalf("stale heartbeat did not identify state_changed: %s", response)
			}
			if !reflect.DeepEqual(before, f.state()) || f.decisionReceipt(http.StatusOK).Committed {
				t.Fatal("stale heartbeat modified the graph or published a decision")
			}
			// Existing callers may continue renewing a lease without opting into
			// version checks; the dispatcher supplies its immutable input version.
			f.request("POST", f.base()+"/reason/heartbeat", map[string]string{"worker": f.lease}, true, http.StatusOK, nil)
		})
	}
}

func TestReasonHeartbeatRejectsMalformedStateVersion(t *testing.T) {
	for name, version := range map[string]any{"null": nil, "number": 123, "empty": "", "short": "abc", "nonhex": strings.Repeat("g", 64)} {
		t.Run(name, func(t *testing.T) {
			f := newDecisionBatchFixture(t)
			before := f.state()
			f.request("POST", f.base()+"/reason/heartbeat", map[string]any{"worker": f.lease, "expected_version": version}, true, http.StatusUnprocessableEntity, nil)
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("invalid heartbeat changed the decision input")
			}
		})
	}
}

func TestReasonHeartbeatPreservesCommitReceiptAndLeaseFence(t *testing.T) {
	f := newDecisionBatchFixture(t)
	version := board.DecisionStateVersion(f.state())
	receipt := f.decision("commit", f.batch(batchAction("step", "next", `{"action":"add","from":["origin"],"description":"Check the fixture"}`)), http.StatusOK)
	response := f.request("POST", f.base()+"/reason/heartbeat", map[string]string{"worker": f.lease, "expected_version": version}, true, http.StatusConflict, nil)
	if strings.Contains(response, "state_changed:") {
		t.Fatal("the planner's own commit was reported as stale input")
	}
	if !reflect.DeepEqual(receipt, f.decisionReceipt(http.StatusOK)) || f.state().Graph.Project.Reason != nil {
		t.Fatal("heartbeat changed the successful receipt or reclaimed its lease")
	}
	var executions []board.Execution
	executions = f.executionRecords()
	if len(executions) != 1 || executions[0].Status != "succeeded" {
		t.Fatal("heartbeat replaced the successful execution")
	}
}
