package server

import (
	"net/http"
	"strings"
	"testing"

	"pwnmesh/internal/protocol"
)

func TestStatusDecodePreservesExecutionAuthorityChecks(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.register("explore")
	endpoint := f.base() + "/executions/" + f.run + "/status"
	f.request("POST", endpoint, map[string]any{}, false, http.StatusForbidden, nil)
	f.request("POST", endpoint, map[string]any{}, true, http.StatusUnprocessableEntity, nil)
	f.request("POST", f.base()+"/executions/missing/status", map[string]any{}, false, http.StatusNotFound, nil)
}

func TestSnapshotConflictPreservesStatusAndStructuredCode(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	prepareSnapshot(t, f, snapshotTemplate(f, "reason"))
	var response struct {
		Code string `json:"code"`
	}
	f.request("POST", f.base()+"/executions/"+f.run+"/input/read", protocol.GraphRequest{
		RequestID: strings.Repeat("b", 32), Op: "read_snapshot", Section: "facts", ExpectedVersion: strings.Repeat("a", 64),
	}, true, http.StatusUnprocessableEntity, &response)
	if response.Code != "state_changed" {
		t.Fatalf("snapshot transport discarded the conflict code: %+v", response)
	}
}
