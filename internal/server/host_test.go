package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

func TestHostGuardValidatesAuthorityAndExactNames(t *testing.T) {
	guard, err := HostGuard("server", "Pwn.Example.COM.", "::1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host string
		want int
	}{
		{"localhost", 204}, {"LOCALHOST.:8000", 204}, {"127.0.0.1:8000", 204},
		{"192.0.2.1", 204}, {"[::1]", 204}, {"[::1]:8000", 204},
		{"[2001:db8::1]:65535", 204}, {"[::ffff:127.0.0.1]:80", 204},
		{"server:8000", 204}, {"PWN.EXAMPLE.COM.:443", 204},
		{"audit-rebind.invalid:8000", 403}, {"localhost.evil.invalid", 403},
		{"server.evil.invalid", 403}, {"sub.pwn.example.com", 403},
		{"", 403}, {":8000", 403}, {"localhost:", 403}, {"localhost:0", 403},
		{"localhost:65536", 403}, {"localhost:+80", 403}, {"localhost:http", 403},
		{"localhost:8000:90", 403}, {"localhost..", 403}, {" localhost", 403},
		{"localhost\t", 403}, {"localhost\x00", 403}, {"localhost/", 403},
		{"http://localhost", 403}, {"user@localhost", 403}, {"localhost%2e", 403},
		{"::1", 403}, {"[::1", 403}, {"::1]", 403}, {"[::1]:", 403},
		{"[::1]:65536", 403}, {"[localhost]:80", 403}, {"[127.0.0.1]:80", 403},
		{"[fe80::1%eth0]:80", 403}, {"localhost,server", 403},
	} {
		t.Run(tc.host, func(t *testing.T) {
			called := false
			handler := guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			r := httptest.NewRequest(http.MethodGet, "/projects", nil)
			r.Host = tc.host
			// Forwarded authority is never evidence that the request is trusted.
			r.Header.Set("X-Forwarded-Host", "localhost")
			r.Header.Set("Forwarded", "host=localhost")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			if response.Code != tc.want || called != (tc.want == http.StatusNoContent) {
				t.Fatalf("host %q: HTTP %d, called=%v; want HTTP %d", tc.host, response.Code, called, tc.want)
			}
		})
	}
}

func TestHostGuardRejectsInvalidConfiguration(t *testing.T) {
	for _, value := range []string{"", "*", "*.example.com", "http://example.com", "example.com:8000", "[::1]", "-host", "host-", "a..b", "host/path", "host name", "host\n", "host,other", strings.Repeat("a", 64) + ".com"} {
		if _, err := HostGuard(value); err == nil {
			t.Errorf("accepted invalid configured hostname %q", value)
		}
	}
}

func TestHostGuardBlocksRebindingWithoutBreakingTrustedBrowserMutations(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	guard, err := HostGuard("server", "pwn.example.com")
	if err != nil {
		t.Fatal(err)
	}
	handler := guard(f.handler)
	for _, tc := range []struct {
		host, origin string
		want         int
	}{
		{"audit-rebind.invalid:8000", "http://audit-rebind.invalid:8000", http.StatusForbidden},
		{"localhost:8000", "http://localhost:8000", http.StatusCreated},
		{"127.0.0.1:8000", "http://127.0.0.1:8000", http.StatusCreated},
		{"server:8000", "", http.StatusCreated},
		{"pwn.example.com", "https://pwn.example.com", http.StatusCreated},
		{"pwn.example.com", "https://attacker.invalid", http.StatusForbidden},
	} {
		t.Run(tc.host+tc.origin, func(t *testing.T) {
			var before []board.Summary
			f.request("GET", "/projects", nil, false, http.StatusOK, &before)
			r := httptest.NewRequest(http.MethodPost, "/projects", strings.NewReader(`{"title":"Host test","origin":"Synthetic input","goal":"Inspect fixture"}`))
			r.Host = tc.host
			r.Header.Set("Content-Type", "text/plain")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
				r.Header.Set("Sec-Fetch-Site", "same-origin")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			if response.Code != tc.want {
				t.Fatalf("HTTP %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
			var after []board.Summary
			f.request("GET", "/projects", nil, false, http.StatusOK, &after)
			added := 0
			if tc.want == http.StatusCreated {
				added = 1
			}
			if len(after) != len(before)+added {
				t.Fatalf("unexpected mutation: before=%d after=%d", len(before), len(after))
			}
		})
	}
}
