//go:build linux

package workergraph

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDrainOnFailurePreservesStartedResultsAndBlocksNewWork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started, conditionStarted, draining := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var runs, verifies, lateRuns atomic.Int32
	failure := errors.New("required deterministic command failed")
	definition := Definition{Version: "drain-v1", Nodes: []Node{
		{ID: "a-expensive", Kind: "agent", Run: func(ctx context.Context, _ Input) (Output, error) {
			runs.Add(1)
			close(started)
			select {
			case <-draining:
			case <-ctx.Done():
				return Output{}, ctx.Err()
			}
			if err := ctx.Err(); err != nil {
				return Output{}, err
			}
			return Output{Value: json.RawMessage(`{"completed":"expensive analysis"}`)}, nil
		}, Verify: func(ctx context.Context, _ Input, out Output) error {
			verifies.Add(1)
			if string(out.Value) != `{"completed":"expensive analysis"}` {
				return errors.New("lost completed output")
			}
			return ctx.Err()
		}},
		{ID: "b-failure", Kind: "function", Run: func(context.Context, Input) (Output, error) {
			<-started
			<-conditionStarted
			return Output{}, failure
		}},
		{ID: "c-conditioned", Kind: "function", When: func(ctx context.Context, _ Input) (bool, string, error) {
			close(conditionStarted)
			<-ctx.Done()
			close(draining)
			return true, "", nil // An affirmative answer after failure cannot start Run.
		}, Run: func(context.Context, Input) (Output, error) { lateRuns.Add(1); return Output{}, nil }},
		{ID: "d-join", Kind: "function", DependsOn: []Dependency{{ID: "a-expensive"}, {ID: "b-failure"}}, Run: func(context.Context, Input) (Output, error) { lateRuns.Add(1); return Output{}, nil }},
	}}
	options := fixtureOptions(t, 3)
	options.DrainOnFailure = true
	for attempt := 0; attempt < 2; attempt++ {
		checkpoint, err := Run(ctx, definition, options)
		if err == nil || attempt == 0 && !errors.Is(err, failure) || checkpoint.Status != "failed" || stateByID(checkpoint, "a-expensive").Status != "succeeded" || stateByID(checkpoint, "b-failure").Status != "failed" || stateByID(checkpoint, "c-conditioned").Status != "blocked" || stateByID(checkpoint, "d-join").Status != "blocked" || runs.Load() != 1 || lateRuns.Load() != 0 || verifies.Load() != int32(attempt+1) || ctx.Err() != nil {
			t.Fatalf("failure did not drain only already-started work: attempt=%d checkpoint=%+v error=%v runs=%d verifies=%d late=%d", attempt, checkpoint, err, runs.Load(), verifies.Load(), lateRuns.Load())
		}
		if saved := persistedCheckpoint(t, options); stateByID(saved, "a-expensive").Status != "succeeded" || saved.Status != "failed" {
			t.Fatalf("drained success or required failure was not durable: %+v", saved)
		}
	}
}

func TestDrainOnFailureStillHonorsParentCancellationAndDeadline(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			budget := 3 * time.Second
			if mode == "deadline" {
				budget = time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			started, conditionStarted, draining, exited := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			failure := errors.New("required command failed")
			definition := Definition{Version: "drain-v1", Nodes: []Node{
				{ID: "a", Kind: "agent", Run: func(ctx context.Context, _ Input) (Output, error) {
					close(started)
					<-ctx.Done()
					close(exited)
					return Output{}, ctx.Err()
				}},
				{ID: "b", Kind: "function", Run: func(context.Context, Input) (Output, error) {
					<-started
					<-conditionStarted
					return Output{}, failure
				}},
				{ID: "c", Kind: "function", When: func(ctx context.Context, _ Input) (bool, string, error) {
					close(conditionStarted)
					<-ctx.Done()
					close(draining)
					return false, "", ctx.Err()
				}, Run: successful},
			}}
			options := fixtureOptions(t, 3)
			options.DrainOnFailure = true
			if mode == "cancel" {
				go func() { <-draining; cancel() }()
			}
			checkpoint, err := Run(ctx, definition, options)
			if !errors.Is(err, failure) || checkpoint.Status != "failed" || stateByID(checkpoint, "a").Status != "running" || ctx.Err() == nil {
				t.Fatalf("draining ignored cancellation or erased definite failure: %+v %v parent=%v", checkpoint, err, ctx.Err())
			}
			select {
			case <-exited:
			default:
				t.Fatal("returned without joining cancelled Run")
			}
		})
	}
}

func TestDrainOnFailureStillCancelsOnUncertainOrInvalidResults(t *testing.T) {
	for _, mode := range []string{"interrupted", "panic", "verification", "condition"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			started, exited := make(chan struct{}), make(chan struct{})
			failure := errors.New("invalid evidence")
			var lateRuns atomic.Int32
			definition := Definition{Version: "drain-v1", Nodes: []Node{
				{ID: "a", Kind: "agent", Run: func(ctx context.Context, _ Input) (Output, error) {
					close(started)
					<-ctx.Done()
					close(exited)
					return Output{}, ctx.Err()
				}},
				{ID: "b", Kind: "function", Run: func(context.Context, Input) (Output, error) {
					<-started
					if mode == "panic" {
						panic("uncertain callback outcome")
					}
					if mode == "interrupted" {
						return Output{}, ErrInterrupted
					}
					return Output{}, nil
				}},
				{ID: "join", Kind: "function", DependsOn: []Dependency{{ID: "a"}, {ID: "b"}}, Run: func(context.Context, Input) (Output, error) { lateRuns.Add(1); return Output{}, nil }},
			}}
			if mode == "verification" {
				definition.Nodes[1].Verify = func(context.Context, Input, Output) error { return failure }
			}
			if mode == "condition" {
				definition.Nodes[1].When = func(context.Context, Input) (bool, string, error) { <-started; return false, "", failure }
			}
			options := fixtureOptions(t, 2)
			options.DrainOnFailure = true
			checkpoint, err := Run(ctx, definition, options)
			wantStatus := "failed"
			if mode == "panic" || mode == "interrupted" {
				wantStatus = "interrupted"
			}
			if err == nil || checkpoint.Status != wantStatus || ctx.Err() != nil || stateByID(checkpoint, "a").Status != "running" || lateRuns.Load() != 0 {
				t.Fatalf("draining weakened uncertain/invalid-result cancellation: %+v %v parent=%v", checkpoint, err, ctx.Err())
			}
			select {
			case <-exited:
			default:
				t.Fatal("returned before interrupted sibling exited")
			}
		})
	}
}
