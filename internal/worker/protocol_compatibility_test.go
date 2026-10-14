package worker

import (
	"context"
	"errors"
	"testing"

	"pwnmesh/internal/protocol"
)

func TestProtocolInterruptionRetainsSameRunRecoveryCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(protocol.ErrInterrupted)
	if !errors.Is(context.Cause(ctx), ErrInterrupted) || ErrInterrupted != protocol.ErrInterrupted {
		t.Fatal("runtime and transport disagree on the same-run interruption sentinel")
	}
}
