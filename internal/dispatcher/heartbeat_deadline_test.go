package dispatcher

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/worker"
)

func TestHeartbeatRenewsWithNarrowLeaseSlack(t *testing.T) {
	for _, timeout := range []time.Duration{1500 * time.Millisecond, 2 * time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			t.Parallel()
			renewed := make(chan struct{}, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/projects/fixture/intents/step/heartbeat" {
					t.Errorf("unexpected heartbeat path: %s", r.URL.Path)
				}
				renewed <- struct{}{}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			scheduler := &Scheduler{Config: config.Config{Runtime: config.Runtime{Interval: 1}}, Client: &Client{Base: server.URL}}
			run := &task{Job: worker.Job{Kind: "explore", Graph: board.Graph{Project: board.Project{ID: "fixture"}}}, Lease: Lease{Run: "fixture@run", Kind: "explore", Intent: "step"}, LeaseTimeout: timeout}
			ctx, cancel := context.WithCancelCause(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); scheduler.heartbeat(ctx, run, cancel) }()
			defer func() { cancel(nil); <-done }()
			deadline := time.NewTimer(4 * time.Second)
			defer deadline.Stop()
			for range 2 {
				select {
				case <-renewed:
				case <-ctx.Done():
					t.Fatalf("healthy lease was cancelled: %v", context.Cause(ctx))
				case <-deadline.C:
					t.Fatal("healthy lease did not receive two renewals")
				}
			}
		})
	}
}

func TestHeartbeatFailedRenewalStopsBeforeLeaseExpiry(t *testing.T) {
	requests := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requests <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	scheduler := &Scheduler{Config: config.Config{Runtime: config.Runtime{Interval: 1}}, Client: &Client{Base: server.URL}}
	run := &task{Job: worker.Job{Kind: "explore", Graph: board.Graph{Project: board.Project{ID: "fixture"}}}, Lease: Lease{Run: "fixture@run", Kind: "explore", Intent: "step"}, LeaseTimeout: 2 * time.Second}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); scheduler.heartbeat(ctx, run, cancel) }()
	defer func() { cancel(nil); <-done }()
	deadline := time.NewTimer(run.LeaseTimeout)
	defer deadline.Stop()
	select {
	case <-requests:
	case <-ctx.Done():
		t.Fatalf("renewal was cancelled before reaching the server: %v", context.Cause(ctx))
	case <-deadline.C:
		t.Fatal("lease expired before attempting renewal")
	}
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			t.Fatalf("unexpected heartbeat cancellation: %v", context.Cause(ctx))
		}
	case <-deadline.C:
		t.Fatal("worker remained active after its lease expired")
	}
}
