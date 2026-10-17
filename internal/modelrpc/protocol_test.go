package modelrpc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/modelwire"
)

func TestErrorRoundTripPreservesRecoveryClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wire  *Error
		cause error
	}{
		{"status only", &Error{HTTPStatus: 401}, nil},
		{"deadline with rate limit", &Error{Kind: agent.ErrorRateLimit, HTTPStatus: 429, Deadline: true}, context.DeadlineExceeded},
		{"human cancellation", &Error{Canceled: true}, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.wire)
			if err != nil {
				t.Fatal(err)
			}
			var restored Error
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			failure := restored.Err()
			var status *modelwire.HTTPError
			if tc.wire.HTTPStatus != 0 && (!errors.As(failure, &status) || status.Status != tc.wire.HTTPStatus) {
				t.Fatalf("endpoint status lost: %v", failure)
			}
			var model *agent.ModelError
			if tc.wire.Kind != "" && (!errors.As(failure, &model) || model.Kind != tc.wire.Kind) {
				t.Fatalf("model classification lost: %v", failure)
			}
			if tc.cause != nil && !errors.Is(failure, tc.cause) {
				t.Fatalf("cancellation or deadline lost: %v", failure)
			}
		})
	}
	if err := (*Error)(nil).Err(); err != nil {
		t.Fatalf("successful response acquired an error: %v", err)
	}
}
