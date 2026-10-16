package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"pwnmesh/internal/board/model"
	"pwnmesh/internal/protocol"
)

func TestClientPreservesAuthoritativeErrorCodes(t *testing.T) {
	for _, test := range []struct {
		name, body, code string
		status           int
		conflict         bool
	}{
		{"coded", `{"code":"state_changed","detail":"view expired"}`, model.CodeStateChanged, 409, true},
		{"uncoded_text", `{"code":"request_failed","detail":"state_changed: user text"}`, protocol.CodeRequestFailed, 409, false},
		{"legacy", `{"detail":"state_changed: old server"}`, model.CodeStateChanged, 409, true},
		{"validation", `{"code":"state_changed","detail":"stale snapshot"}`, model.CodeStateChanged, 422, false},
		{"invalid_code", `{"code":3,"detail":"state_changed: invalid code"}`, protocol.CodeRequestFailed, 409, false},
		{"invalid_json", `{"code":"state_changed",`, protocol.CodeRequestFailed, 409, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			err := (&Client{Base: server.URL}).Do(context.Background(), "POST", "/state/read", nil, nil, nil)
			var failure *protocol.RPCError
			if !errors.As(fmt.Errorf("request: %w", err), &failure) || failure.Code != test.code || decisionStateChanged(err) != test.conflict {
				t.Fatalf("lost structured failure: %+v err=%v", failure, err)
			}
			if err.Error() != fmt.Sprintf("board HTTP %d: %s", test.status, test.body) {
				t.Fatalf("display detail changed: %v", err)
			}
		})
	}
}
