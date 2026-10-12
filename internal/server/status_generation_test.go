package server

import (
	"net/http"
	"testing"
)

func TestStatusGenerationFencesPauseAndResume(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.request("POST", f.base()+"/restart", map[string]any{"expected_generation": 0}, false, http.StatusOK, nil)
	for _, status := range []string{"stopped", "active"} {
		before := f.state().Graph.Project.Status
		f.request("PUT", f.base()+"/status", map[string]any{"status": status, "expected_generation": 0}, false, http.StatusConflict, nil)
		if after := f.state().Graph.Project.Status; after != before {
			t.Fatalf("stale %s changed the current round to %s", status, after)
		}
		f.request("PUT", f.base()+"/status", map[string]any{"status": status, "expected_generation": 1}, false, http.StatusOK, nil)
		if after := f.state().Graph.Project.Status; after != status {
			t.Fatalf("matching generation did not set %s: %s", status, after)
		}
	}
	// Existing non-browser clients can continue making an unconditional change.
	f.request("PUT", f.base()+"/status", map[string]any{"status": "stopped"}, false, http.StatusOK, nil)
}

func TestStatusGenerationRejectsMalformedBoundaries(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	for _, expected := range []any{nil, -1, 0.5, "0", true} {
		f.request("PUT", f.base()+"/status", map[string]any{"status": "stopped", "expected_generation": expected}, false, http.StatusUnprocessableEntity, nil)
	}
	f.request("PUT", f.base()+"/status", map[string]any{"status": "stopped", "generation": 0}, false, http.StatusUnprocessableEntity, nil)
	if f.state().Graph.Project.Status != "active" {
		t.Fatal("invalid status request changed the project")
	}
}
