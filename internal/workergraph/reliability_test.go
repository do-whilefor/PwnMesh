//go:build linux

package workergraph

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func persistedCheckpoint(t *testing.T, options Options) Checkpoint {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(options.Dir, "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint Checkpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

func TestRequiredFailurePreservesUncertainSiblingAndRemainsFailedAfterRecovery(t *testing.T) {
	for _, mode := range []string{"interrupted", "panic", "cancelled", "interruption_first"} {
		t.Run(mode, func(t *testing.T) {
			options := fixtureOptions(t, 2)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started, failureStarted := make(chan struct{}), make(chan struct{})
			failure := errors.New("required independent evidence is invalid")
			var starts, failures, recoveries, joins atomic.Int32
			definition := Definition{Version: "1", Nodes: []Node{
				{ID: "a", Kind: "agent", Optional: true, Verify: verified, Run: func(ctx context.Context, _ Input) (Output, error) {
					starts.Add(1) // Represents one already-performed external operation.
					close(started)
					if mode == "interruption_first" {
						<-failureStarted
						return Output{}, ErrInterrupted
					}
					// Cancellation proves the required sibling failure was processed.
					<-ctx.Done()
					if mode == "panic" {
						panic("receipt lost after external operation")
					}
					if mode == "cancelled" {
						return Output{}, ctx.Err()
					}
					return Output{}, ErrInterrupted
				}},
				{ID: "b", Kind: "function", Run: func(ctx context.Context, _ Input) (Output, error) {
					failures.Add(1)
					<-started
					close(failureStarted)
					if mode == "interruption_first" {
						<-ctx.Done()
					}
					return Output{}, failure
				}},
				{ID: "join", Kind: "function", DependsOn: []Dependency{{ID: "a"}, {ID: "b"}}, Run: func(context.Context, Input) (Output, error) {
					joins.Add(1)
					return Output{}, nil
				}},
			}}
			checkpoint, err := Run(ctx, definition, options)
			if !errors.Is(err, failure) || checkpoint.Status != "failed" || stateByID(checkpoint, "b").Status != "failed" || stateByID(checkpoint, "join").Status != "blocked" {
				t.Fatalf("terminal failure lost: %+v %v", checkpoint, err)
			}
			uncertain := stateByID(persistedCheckpoint(t, options), "a")
			if uncertain.Status != "running" || uncertain.Attempt != 1 || !uncertain.FinishedAt.IsZero() || !strings.Contains(uncertain.Error, "reconciliation") {
				t.Fatalf("uncertain sibling became terminal: %+v", uncertain)
			}
			if _, err := Run(ctx, definition, options); err == nil || !strings.Contains(err.Error(), "reconciliation required") || starts.Load() != 1 || failures.Load() != 1 {
				t.Fatalf("unreconciled operation replayed: %v starts=%d failures=%d", err, starts.Load(), failures.Load())
			}
			definition.Nodes[0].Reconcile = func(_ context.Context, input Input, saved NodeState) (Output, error) {
				recoveries.Add(1)
				if saved.Status != "running" || input.Attempt != 1 {
					t.Fatalf("lost recovery identity: %+v %+v", input, saved)
				}
				return Output{Value: json.RawMessage(`{"external_receipt":"confirmed"}`)}, nil
			}
			for attempt := 0; attempt < 2; attempt++ {
				checkpoint, err = Run(ctx, definition, options)
				if err == nil || !strings.Contains(err.Error(), failure.Error()) || checkpoint.Status != "failed" || stateByID(checkpoint, "a").Status != "succeeded" || stateByID(checkpoint, "b").Status != "failed" || starts.Load() != 1 || failures.Load() != 1 || recoveries.Load() != 1 || joins.Load() != 0 {
					t.Fatalf("recovery retried failed work or accepted graph: %+v %v starts=%d failures=%d recoveries=%d joins=%d", checkpoint, err, starts.Load(), failures.Load(), recoveries.Load(), joins.Load())
				}
			}
		})
	}
}

func TestSynchronousCallbackPanicCancelsAndJoinsStartedSibling(t *testing.T) {
	for _, phase := range []string{"condition", "verification"} {
		t.Run(phase, func(t *testing.T) {
			options := fixtureOptions(t, 2)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var exited atomic.Bool
			definition := Definition{Version: "1", Nodes: []Node{
				{ID: "a", Kind: "agent", Run: func(ctx context.Context, _ Input) (Output, error) {
					close(started)
					<-ctx.Done()
					close(cancelled)
					<-release
					exited.Store(true)
					return Output{}, ctx.Err()
				}},
				{ID: "b", Kind: "function", Run: successful},
			}}
			if phase == "condition" {
				definition.Nodes[1].When = func(context.Context, Input) (bool, string, error) {
					<-started
					panic("condition crashed")
				}
			} else {
				definition.Nodes[1].Verify = func(context.Context, Input, Output) error {
					<-started
					panic("verification crashed")
				}
			}
			type result struct {
				checkpoint Checkpoint
				err        error
			}
			finished := make(chan result, 1)
			go func() { checkpoint, err := Run(ctx, definition, options); finished <- result{checkpoint, err} }()
			select {
			case <-cancelled:
			case result := <-finished:
				close(release)
				t.Fatalf("returned before cancelling sibling: %+v %v", result.checkpoint, result.err)
			case <-ctx.Done():
				close(release)
				t.Fatal("panic did not cancel sibling")
			}
			select {
			case result := <-finished:
				close(release)
				t.Fatalf("returned before sibling exited: %+v %v", result.checkpoint, result.err)
			default:
			}
			close(release)
			got := <-finished
			wantGraph, wantNode := "interrupted", "running"
			if phase == "condition" {
				wantGraph, wantNode = "failed", "failed"
			}
			if got.err == nil || !strings.Contains(got.err.Error(), phase+" callback panic") || !exited.Load() || got.checkpoint.Status != wantGraph || stateByID(got.checkpoint, "b").Status != wantNode || stateByID(persistedCheckpoint(t, options), "a").Status != "running" {
				t.Fatalf("panic escaped safe shutdown: %+v %v exited=%v", got.checkpoint, got.err, exited.Load())
			}
		})
	}
}

func TestRecoveryCallbackPanicsPreserveCheckpointAndDoNotReplay(t *testing.T) {
	for _, phase := range []string{"reconciliation", "verification"} {
		t.Run(phase, func(t *testing.T) {
			options := fixtureOptions(t, 1)
			var starts, recoveries int
			definition := Definition{Version: "1", Nodes: []Node{{ID: "a", Kind: "agent", Run: func(context.Context, Input) (Output, error) {
				starts++
				return Output{}, ErrInterrupted
			}, Verify: verified}}}
			if _, err := Run(context.Background(), definition, options); !errors.Is(err, ErrInterrupted) {
				t.Fatal(err)
			}
			definition.Nodes[0].Reconcile = func(context.Context, Input, NodeState) (Output, error) {
				recoveries++
				if phase == "reconciliation" {
					panic("external receipt lookup crashed")
				}
				return Output{Value: json.RawMessage(`{"ok":true}`)}, nil
			}
			if phase == "verification" {
				definition.Nodes[0].Verify = func(context.Context, Input, Output) error { panic("receipt verifier crashed") }
			}
			checkpoint, err := Run(context.Background(), definition, options)
			if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), phase+" callback panic") || checkpoint.Status != "interrupted" || stateByID(persistedCheckpoint(t, options), "a").Status != "running" || starts != 1 || recoveries != 1 {
				t.Fatalf("recovery panic lost unresolved boundary: %+v %v starts=%d recoveries=%d", checkpoint, err, starts, recoveries)
			}
			definition.Nodes[0].Verify = verified
			definition.Nodes[0].Reconcile = func(context.Context, Input, NodeState) (Output, error) {
				recoveries++
				return Output{Value: json.RawMessage(`{"ok":true}`)}, nil
			}
			checkpoint, err = Run(context.Background(), definition, options)
			if err != nil || checkpoint.Status != "succeeded" || starts != 1 || recoveries != 2 {
				t.Fatalf("safe recovery failed: %+v %v starts=%d recoveries=%d", checkpoint, err, starts, recoveries)
			}
		})
	}
}

func TestInterruptedSiblingCancelsLiveVerificationWithoutCancellingParent(t *testing.T) {
	options := fixtureOptions(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started, verified := make(chan struct{}), make(chan struct{})
	definition := Definition{Version: "1", Nodes: []Node{
		{ID: "a", Kind: "agent", Run: func(context.Context, Input) (Output, error) {
			<-started
			return Output{}, ErrInterrupted
		}},
		{ID: "b", Kind: "function", Run: func(ctx context.Context, _ Input) (Output, error) {
			close(started)
			<-ctx.Done()
			return Output{Value: json.RawMessage(`{"operation":"settled"}`)}, nil
		}, Verify: func(ctx context.Context, _ Input, _ Output) error {
			<-ctx.Done()
			close(verified)
			return ctx.Err()
		}},
	}}
	checkpoint, err := Run(ctx, definition, options)
	if ctx.Err() != nil || !errors.Is(err, ErrInterrupted) || checkpoint.Status != "interrupted" || stateByID(checkpoint, "a").Status != "running" || stateByID(checkpoint, "b").Status != "running" {
		t.Fatalf("verification escaped graph cancellation: %+v %v parent=%v", checkpoint, err, ctx.Err())
	}
	select {
	case <-verified:
	default:
		t.Fatal("Run returned before live verifier exited")
	}
}

func TestSiblingFailureCancelsAlreadyRunningVerificationAndWaitsForExit(t *testing.T) {
	for _, mode := range []string{"failed", "interrupted", "panic"} {
		t.Run(mode, func(t *testing.T) {
			options := fixtureOptions(t, 2)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			verifying, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var exited, joined atomic.Bool
			failure := errors.New("required sibling rejected its evidence")
			definition := Definition{Version: "1", Nodes: []Node{
				{ID: "a", Kind: "agent", Run: func(context.Context, Input) (Output, error) {
					// The failure occurs after the sibling has entered verification.
					<-verifying
					if mode == "panic" {
						panic("lost receipt")
					}
					if mode == "interrupted" {
						return Output{}, ErrInterrupted
					}
					return Output{}, failure
				}},
				{ID: "b", Kind: "function", Run: successful, Verify: func(ctx context.Context, _ Input, _ Output) error {
					close(verifying)
					<-ctx.Done()
					close(cancelled)
					<-release
					exited.Store(true)
					return ctx.Err()
				}},
				{ID: "join", Kind: "function", DependsOn: []Dependency{{ID: "a"}, {ID: "b"}}, Run: func(context.Context, Input) (Output, error) {
					joined.Store(true)
					return Output{}, nil
				}},
			}}
			type result struct {
				checkpoint Checkpoint
				err        error
			}
			finished := make(chan result, 1)
			go func() { checkpoint, err := Run(ctx, definition, options); finished <- result{checkpoint, err} }()
			select {
			case <-cancelled:
			case <-time.After(3 * time.Second):
				cancel()
				close(release)
				<-finished
				t.Fatal("live verification blocked sibling failure cancellation")
			}
			if ctx.Err() != nil {
				close(release)
				<-finished
				t.Fatal("sibling failure required parent cancellation")
			}
			select {
			case got := <-finished:
				close(release)
				t.Fatalf("Run returned before verifier exited: %+v %v", got.checkpoint, got.err)
			default:
			}
			close(release)
			got := <-finished
			wantStatus, wantError := "failed", failure
			if mode != "failed" {
				wantStatus, wantError = "interrupted", ErrInterrupted
			}
			if !errors.Is(got.err, wantError) || got.checkpoint.Status != wantStatus || !exited.Load() || joined.Load() || stateByID(persistedCheckpoint(t, options), "b").Status != "running" {
				t.Fatalf("verification failure boundary lost: %+v %v exited=%v joined=%v", got.checkpoint, got.err, exited.Load(), joined.Load())
			}
		})
	}
}
