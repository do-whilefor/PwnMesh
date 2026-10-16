package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	b "pwnmesh/internal/board"
	"pwnmesh/internal/protocol"
)

func TestHTTPErrorCodesComeFromAuthority(t *testing.T) {
	for _, test := range []struct {
		name, code string
		err        error
	}{
		{"coded", b.CodeStateChanged, b.CodedErr(409, b.CodeStateChanged, "the view expired")},
		{"display_text", protocol.CodeRequestFailed, b.Err(409, "state_changed: input supplied text")},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeError(w, httptest.NewRequest("POST", "/state/read", nil), fmt.Errorf("request: %w", test.err))
			var body struct{ Code, Detail string }
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 409 || body.Code != test.code || body.Detail != test.err.Error() {
				t.Fatalf("lost authoritative error: status=%d body=%s err=%v", w.Code, w.Body.String(), err)
			}
		})
	}
}

func TestGraphReadRoutesShareStrictRequestDecoding(t *testing.T) {
	f, _ := prepareUpdateFixture(t, "snapshot")
	for _, route := range []struct{ op, path string }{
		{"read_graph", f.base() + "/state/read"},
		{"read_snapshot", f.base() + "/executions/" + f.run + "/input/read"},
		{"read_updates", f.base() + "/executions/" + f.run + "/updates"},
		{"read_trace_runs", f.base() + "/executions/" + f.run + "/traces/read"},
	} {
		t.Run(route.op, func(t *testing.T) {
			// Send the same typed request the dispatcher's graph bridge sends,
			// including any zero-value fields emitted by GraphRequest.
			read := protocol.GraphRequest{RequestID: strings.Repeat("a", 32), Op: route.op}
			f.request("POST", route.path, read, true, http.StatusOK, nil)
			for _, invalid := range []struct {
				name, field string
				value       any
			}{
				{"unknown_field", "source_ids", []string{"forged"}},
				{"unknown_nested_field", "updates", map[string]any{"source_ids": []string{"forged"}}},
				{"string_offset", "offset", "1"},
				{"fractional_offset", "offset", json.Number("1.5")},
			} {
				t.Run(invalid.name, func(t *testing.T) {
					body := map[string]any{"request_id": read.RequestID, "op": read.Op, invalid.field: invalid.value}
					f.request("POST", route.path, body, true, http.StatusUnprocessableEntity, nil)
				})
			}
		})
	}
}

func TestExecutionRoutesRequireEveryLeaseIdentityField(t *testing.T) {
	f, _ := prepareUpdateFixture(t, "snapshot")
	base := f.base() + "/executions/" + f.run
	for _, route := range []struct {
		path string
		body any
	}{
		{base + "/input/read", protocol.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_snapshot"}},
		{base + "/updates", updateRead(nil)},
		{base + "/traces/read", protocol.GraphRequest{RequestID: strings.Repeat("b", 32), Op: "read_trace_runs"}},
		{base + "/status", map[string]string{"status": "running"}},
		{base + "/resume", map[string]any{}},
	} {
		for _, header := range []string{"X-PwnMesh-Run", "X-PwnMesh-Lease", "X-PwnMesh-Intent"} {
			for _, value := range []string{"", "another-execution"} {
				t.Run(route.path+"/"+header+"/"+value, func(t *testing.T) {
					raw, err := json.Marshal(route.body)
					if err != nil {
						t.Fatal(err)
					}
					r := httptest.NewRequest("POST", route.path, bytes.NewReader(raw))
					r.Header.Set("X-PwnMesh-Run", f.lease)
					r.Header.Set("X-PwnMesh-Lease", f.kind)
					r.Header.Set("X-PwnMesh-Intent", f.intent)
					r.Header.Set(header, value)
					w := httptest.NewRecorder()
					f.handler.ServeHTTP(w, r)
					if w.Code != http.StatusForbidden {
						t.Fatalf("HTTP %d, want 403: %s", w.Code, w.Body.String())
					}
				})
			}
		}
	}
}
