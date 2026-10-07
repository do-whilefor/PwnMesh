package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

func TestBrowserMutationsRequireSameOrigin(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	for _, tc := range []struct {
		name, origin, site string
		allowed            bool
	}{
		{name: "API client", allowed: true},
		{name: "same origin", origin: "http://127.0.0.1:8000", site: "same-origin", allowed: true},
		{name: "legacy browser", origin: "http://127.0.0.1:8000", allowed: true},
		{name: "proxy TLS termination", origin: "https://127.0.0.1:8000", site: "same-origin", allowed: true},
		{name: "external origin", origin: "https://example.invalid"},
		{name: "different port", origin: "http://127.0.0.1:9000"},
		{name: "opaque origin", origin: "null"},
		{name: "missing origin with cross-site metadata", site: "cross-site"},
		{name: "same-site is not same-origin", origin: "http://127.0.0.1:9000", site: "same-site"},
		{name: "contradictory headers", origin: "https://example.invalid", site: "same-origin"},
		{name: "malformed origin", origin: "http://127.0.0.1:8000/path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before []board.Summary
			f.request("GET", "/projects", nil, false, http.StatusOK, &before)
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8000/projects", strings.NewReader(`{"title":"Browser task","origin":"Synthetic input","goal":"Inspect fixture"}`))
			// A browser can send this request without a CORS preflight.
			r.Header.Set("Content-Type", "text/plain")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.site != "" {
				r.Header.Set("Sec-Fetch-Site", tc.site)
			}
			response := httptest.NewRecorder()
			f.handler.ServeHTTP(response, r)
			want := http.StatusForbidden
			added := 0
			if tc.allowed {
				want, added = http.StatusCreated, 1
			}
			if response.Code != want || !json.Valid(response.Body.Bytes()) {
				t.Fatalf("HTTP %d, want %d: %s", response.Code, want, response.Body.String())
			}
			var after []board.Summary
			f.request("GET", "/projects", nil, false, http.StatusOK, &after)
			if len(after) != len(before)+added {
				t.Fatalf("unexpected project mutation: before=%d after=%d", len(before), len(after))
			}
		})
	}

	// Protect bodyless destructive requests at the same application boundary.
	r := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:8000"+f.base(), nil)
	r.Header.Set("Origin", "https://example.invalid")
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, r)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin DELETE returned HTTP %d", response.Code)
	}
	f.request("GET", f.base(), nil, false, http.StatusOK, nil)
}
