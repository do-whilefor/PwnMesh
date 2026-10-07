package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

func TestCuratorHeartbeatBindsRegisteredInputAndPreservesLegacyRenewal(t *testing.T) {
	f := newCurationHTTPFixture(t)
	_, job := prepareCurator(f)
	body := map[string]string{"worker": f.lease, "expected_version": job.InputSnapshot.StateVersion}
	// Other control-role lease activity is not new shared content.
	f.request("POST", f.base()+"/reason/claim", map[string]string{"worker": "parallel-planner", "trigger": "initial"}, false, http.StatusOK, nil)
	f.request("POST", f.base()+"/curate/heartbeat", body, true, http.StatusOK, nil)
	f.request("POST", f.base()+"/reason/release", map[string]string{"worker": "parallel-planner"}, false, http.StatusOK, nil)
	f.request("POST", f.base()+"/curate/heartbeat", body, true, http.StatusOK, nil)
	if board.DecisionStateVersion(f.state()) != body["expected_version"] {
		t.Fatal("lease-only traffic changed curator input")
	}
	f.request("POST", f.base()+"/hints", map[string]string{"content": "Concurrent observation", "creator": "fixture"}, false, http.StatusCreated, nil)
	before := f.state()
	response := f.request("POST", f.base()+"/curate/heartbeat", body, true, http.StatusConflict, nil)
	var problem struct{ Detail string }
	if json.Unmarshal([]byte(response), &problem) != nil || !strings.HasPrefix(problem.Detail, "state_changed:") {
		t.Fatalf("stale curator heartbeat did not identify changed input: %s", response)
	}
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("stale heartbeat changed state or renewed the stale lease")
	}
	// Supplying the latest graph hash must not replace a registered input.
	body["expected_version"] = board.DecisionStateVersion(before)
	f.request("POST", f.base()+"/curate/heartbeat", body, true, http.StatusConflict, nil)
	f.request("POST", f.base()+"/curate/heartbeat", map[string]string{"worker": f.lease}, true, http.StatusOK, nil)
	f.request("GET", f.base()+"/state/curation/receipt", nil, true, http.StatusNotFound, nil)
}

func TestCuratorHeartbeatRejectsMalformedStateVersion(t *testing.T) {
	for name, version := range map[string]any{"null": nil, "number": 123, "empty": "", "short": "abc", "nonhex": strings.Repeat("g", 64)} {
		t.Run(name, func(t *testing.T) {
			f := newCurationHTTPFixture(t)
			prepareCurator(f)
			before := f.state()
			f.request("POST", f.base()+"/curate/heartbeat", map[string]any{"worker": f.lease, "expected_version": version}, true, http.StatusUnprocessableEntity, nil)
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("malformed heartbeat renewed its lease")
			}
		})
	}
}

func TestCuratorHeartbeatOwnCommitWinsBeforeResultApplication(t *testing.T) {
	f := newCurationHTTPFixture(t)
	_, job := prepareCurator(f)
	receipt := f.action("curate", "committed", board.CuratePayload{ThroughRevision: job.InputSnapshot.Revision, Groups: []board.CurateGroup{}})
	body := map[string]string{"worker": f.lease, "expected_version": job.InputSnapshot.StateVersion}
	if !receipt.Committed || board.DecisionStateVersion(f.state()) == body["expected_version"] {
		t.Fatal("fixture did not change input through its own curation")
	}
	// A new producer event after commit also cannot invalidate an accepted batch.
	f.request("POST", f.base()+"/hints", map[string]string{"content": "Next input after commit", "creator": "fixture"}, false, http.StatusCreated, nil)
	for range 2 {
		f.request("POST", f.base()+"/curate/heartbeat", body, true, http.StatusOK, nil)
	}
	f.pending(`{"accepted":true,"data":{"curated":true}}`)
	f.apply(http.StatusOK)
	before := f.state()
	response := f.request("POST", f.base()+"/curate/heartbeat", body, true, http.StatusConflict, nil)
	if strings.Contains(response, "state_changed:") || !reflect.DeepEqual(before, f.state()) {
		t.Fatal("completed curation reclaimed its lease or became stale")
	}
	var ack board.StateActionResult
	f.request("GET", f.base()+"/state/curation/receipt", nil, true, http.StatusOK, &ack)
	if !ack.Committed || f.executionRecords()[0].Status != "succeeded" {
		t.Fatal("heartbeat lost the committed receipt or successful execution")
	}
}

func TestCuratorHeartbeatReceiptCannotCrossStopOrGeneration(t *testing.T) {
	for _, action := range []string{"stop", "restart", "different_owner"} {
		t.Run(action, func(t *testing.T) {
			f := newCurationHTTPFixture(t)
			_, job := prepareCurator(f)
			f.action("curate", "committed", board.CuratePayload{ThroughRevision: job.InputSnapshot.Revision, Groups: []board.CurateGroup{}})
			switch action {
			case "stop":
				f.request("PUT", f.base()+"/status", map[string]string{"status": "stopped"}, false, http.StatusOK, nil)
				f.request("PUT", f.base()+"/status", map[string]string{"status": "active"}, false, http.StatusOK, nil)
			case "restart":
				f.request("POST", f.base()+"/restart", map[string]any{}, false, http.StatusOK, nil)
			case "different_owner":
				f.request("POST", f.base()+"/curate/release", map[string]string{"worker": f.lease}, true, http.StatusOK, nil)
				f.request("POST", f.base()+"/curate/claim", map[string]string{"worker": "another-curator", "trigger": "later_input"}, false, http.StatusOK, nil)
			}
			before := f.state()
			f.request("POST", f.base()+"/curate/heartbeat", map[string]string{"worker": f.lease, "expected_version": job.InputSnapshot.StateVersion}, true, http.StatusConflict, nil)
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("receipt bypassed the current execution boundary")
			}
		})
	}
}
