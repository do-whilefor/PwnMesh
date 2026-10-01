//go:build linux

package workergraph

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestConditionDoesNotBlockIndependentNodeDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	siblingStarted := make(chan struct{})
	var ran atomic.Bool
	definition := Definition{Version: "1", Nodes: []Node{
		{ID: "a", Kind: "function", When: func(ctx context.Context, input Input) (bool, string, error) {
			if input.Attempt != 0 {
				t.Error("pure condition acquired a side effect attempt")
			}
			select {
			case <-siblingStarted:
				return true, "", nil
			case <-ctx.Done():
				return false, "", ctx.Err()
			}
		}, Run: func(context.Context, Input) (Output, error) { ran.Store(true); return Output{}, nil }},
		{ID: "b", Kind: "function", Run: func(context.Context, Input) (Output, error) {
			close(siblingStarted)
			return Output{}, nil
		}},
	}}
	cp, err := Run(ctx, definition, fixtureOptions(t, 2))
	if err != nil || cp.Status != "succeeded" || !ran.Load() || ctx.Err() != nil {
		t.Fatalf("condition blocked an independent branch: %+v %v", cp, err)
	}
	for _, state := range cp.Nodes {
		if state.ReadyAt.IsZero() || state.StartedAt.Before(state.ReadyAt) || state.RunDurationMS <= 0 || state.VerifyDurationMS <= 0 {
			t.Fatalf("missing node phase timing: %+v", state)
		}
	}
	if stateByID(cp, "a").ConditionDurationMS <= 0 || stateByID(cp, "b").ConditionDurationMS != 0 {
		t.Fatal("condition timing does not match executed callbacks")
	}
}

func TestRequiredFailureCancelsConditionBeforeSideEffectsAndJoinsIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var exited, ran atomic.Bool
	failure := errors.New("required sibling evidence rejected")
	definition := Definition{Version: "1", Nodes: []Node{
		{ID: "a", Kind: "function", When: func(ctx context.Context, _ Input) (bool, string, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			<-release
			exited.Store(true)
			return true, "", nil // Even a late affirmative answer cannot start Run.
		}, Run: func(context.Context, Input) (Output, error) { ran.Store(true); return Output{}, nil }},
		{ID: "b", Kind: "function", Run: func(context.Context, Input) (Output, error) {
			<-started
			return Output{}, failure
		}},
	}}
	type result struct {
		checkpoint Checkpoint
		err        error
	}
	options := fixtureOptions(t, 2)
	finished := make(chan result, 1)
	go func() { cp, err := Run(ctx, definition, options); finished <- result{cp, err} }()
	select {
	case <-cancelled:
	case <-ctx.Done():
		close(release)
		<-finished
		t.Fatal("condition blocked failure cancellation")
	}
	select {
	case got := <-finished:
		close(release)
		t.Fatalf("returned before condition exited: %+v %v", got.checkpoint, got.err)
	default:
	}
	close(release)
	got := <-finished
	state := stateByID(got.checkpoint, "a")
	if !errors.Is(got.err, failure) || got.checkpoint.Status != "failed" || !exited.Load() || ran.Load() || ctx.Err() != nil || state.Status != "blocked" || state.Attempt != 0 || !state.StartedAt.IsZero() {
		t.Fatalf("condition escaped cancellation boundary: %+v %v", got.checkpoint, got.err)
	}
}

func TestInterruptedPureConditionResumesWithoutReconciliation(t *testing.T) {
	options := fixtureOptions(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var conditions, runs int
	definition := Definition{Version: "1", Nodes: []Node{{ID: "a", Kind: "function", When: func(ctx context.Context, _ Input) (bool, string, error) {
		conditions++
		if conditions == 1 {
			cancel()
			<-ctx.Done()
			return true, "", nil
		}
		return true, "", nil
	}, Run: func(_ context.Context, input Input) (Output, error) {
		runs++
		intent := stateByID(persistedCheckpoint(t, options), "a")
		if input.Attempt != 1 || intent.Status != "running" || intent.Attempt != 1 || intent.InputSHA256 != inputHash(input) || intent.ConditionDurationMS <= 0 {
			t.Errorf("Run preceded durable condition decision and intent: %+v %+v", input, intent)
		}
		return Output{}, nil
	}, Verify: verified}}}
	first, err := Run(ctx, definition, options)
	before := stateByID(first, "a")
	if !errors.Is(err, context.Canceled) || first.Status != "interrupted" || before.Status != "pending" || before.Attempt != 0 || before.ReadyAt.IsZero() || before.ConditionDurationMS <= 0 || !before.StartedAt.IsZero() || runs != 0 {
		t.Fatalf("pure condition became an uncertain side effect: %+v %v", first, err)
	}
	second, err := Run(context.Background(), definition, options)
	after := stateByID(second, "a")
	if err != nil || second.Status != "succeeded" || conditions != 2 || runs != 1 || !before.ReadyAt.Equal(after.ReadyAt) || after.ConditionDurationMS <= before.ConditionDurationMS {
		t.Fatalf("pure condition could not safely resume: %+v %v", second, err)
	}
}

func TestConditionAndRunShareParallelSlot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan string, 3)
	releases := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{}), "c": make(chan struct{})}
	runStarted, releaseRun := make(chan struct{}), make(chan struct{})
	var active, maximum atomic.Int32
	enter := func() {
		n := active.Add(1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
	}
	definition := Definition{Version: "1"}
	for _, id := range []string{"a", "b", "c"} {
		definition.Nodes = append(definition.Nodes, Node{ID: id, Kind: "function", When: func(ctx context.Context, input Input) (bool, string, error) {
			enter()
			defer active.Add(-1)
			started <- input.NodeID
			select {
			case <-releases[input.NodeID]:
				return true, "", nil
			case <-ctx.Done():
				return false, "", ctx.Err()
			}
		}, Run: func(ctx context.Context, input Input) (Output, error) {
			enter()
			defer active.Add(-1)
			if input.NodeID == "a" {
				close(runStarted)
				select {
				case <-releaseRun:
				case <-ctx.Done():
					return Output{}, ctx.Err()
				}
			}
			return Output{}, nil
		}})
	}
	options := fixtureOptions(t, 2)
	finished := make(chan error, 1)
	go func() { _, err := Run(ctx, definition, options); finished <- err }()
	for range 2 {
		select {
		case id := <-started:
			if id == "c" {
				t.Error("third condition overtook reserved first slots")
			}
		case <-ctx.Done():
			t.Fatal("independent conditions did not run in parallel")
		}
	}
	close(releases["a"])
	select {
	case <-runStarted:
	case <-ctx.Done():
		t.Fatal("condition did not hand its slot to Run")
	}
	select {
	case id := <-started:
		t.Errorf("condition %s exceeded capacity while Run owned the slot", id)
	default:
	}
	close(releaseRun)
	select {
	case id := <-started:
		if id != "c" {
			t.Errorf("wrong ready condition: %s", id)
		}
	case <-ctx.Done():
		t.Fatal("released slot did not dispatch ready condition")
	}
	close(releases["b"])
	close(releases["c"])
	if err := <-finished; err != nil || maximum.Load() != 2 {
		t.Fatalf("shared condition/run capacity failed: %v maximum=%d", err, maximum.Load())
	}
}

func TestConditionFailureSurvivesSiblingInterruption(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan struct{})
	failure := errors.New("condition rejected required input")
	definition := Definition{Version: "1", Nodes: []Node{
		{ID: "a", Kind: "function", Run: func(context.Context, Input) (Output, error) {
			<-started
			return Output{}, ErrInterrupted
		}},
		{ID: "b", Kind: "function", When: func(ctx context.Context, _ Input) (bool, string, error) {
			close(started)
			<-ctx.Done() // The sibling interruption is processed first.
			return false, "", failure
		}, Run: successful},
	}}
	cp, err := Run(ctx, definition, fixtureOptions(t, 2))
	if !errors.Is(err, failure) || ctx.Err() != nil || cp.Status != "failed" || stateByID(cp, "b").Status != "failed" || stateByID(cp, "b").Attempt != 0 || stateByID(cp, "a").Status != "running" {
		t.Fatalf("definite condition failure disappeared after sibling interruption: %+v %v", cp, err)
	}
}

func TestRecoveryTimingAccumulatesWithoutOverwritingLivePhases(t *testing.T) {
	options := fixtureOptions(t, 1)
	var reconciles int
	definition := Definition{Version: "1", Nodes: []Node{{ID: "a", Kind: "function", Run: func(context.Context, Input) (Output, error) {
		return Output{}, ErrInterrupted
	}, Verify: verified, Reconcile: func(context.Context, Input, NodeState) (Output, error) {
		reconciles++
		if reconciles == 1 {
			return Output{}, ErrInterrupted
		}
		return Output{}, nil
	}}}}
	first, err := Run(context.Background(), definition, options)
	before := stateByID(first, "a")
	if !errors.Is(err, ErrInterrupted) || before.RunDurationMS <= 0 || before.VerifyDurationMS != 0 || before.ReconcileDurationMS != 0 || before.RecoveryVerifyDurationMS != 0 {
		t.Fatalf("wrong interrupted live timing: %+v %v", first, err)
	}
	second, err := Run(context.Background(), definition, options)
	partial := stateByID(second, "a")
	if !errors.Is(err, ErrInterrupted) || partial.ReconcileDurationMS <= 0 || partial.RecoveryVerifyDurationMS != 0 {
		t.Fatalf("failed recovery timing missing: %+v %v", second, err)
	}
	third, err := Run(context.Background(), definition, options)
	after := stateByID(third, "a")
	if err != nil || after.ReconcileDurationMS <= partial.ReconcileDurationMS || after.RecoveryVerifyDurationMS <= 0 || after.RunDurationMS != before.RunDurationMS || after.VerifyDurationMS != before.VerifyDurationMS || !after.ReadyAt.Equal(before.ReadyAt) || !after.StartedAt.Equal(before.StartedAt) {
		t.Fatalf("recovery changed original timing or omitted recovery: %+v %v", third, err)
	}
	fourth, err := Run(context.Background(), definition, options)
	final := stateByID(fourth, "a")
	if err != nil || final.RecoveryVerifyDurationMS <= after.RecoveryVerifyDurationMS || final.ReconcileDurationMS != after.ReconcileDurationMS || final.RunDurationMS != before.RunDurationMS {
		t.Fatalf("revalidation did not retain distinct cumulative timing: %+v %v", fourth, err)
	}
}

func TestLegacyCheckpointDoesNotInventOriginalTiming(t *testing.T) {
	options := fixtureOptions(t, 1)
	definition := Definition{Version: "1", Nodes: []Node{{ID: "a", Kind: "function", Run: successful, Verify: verified}}}
	cp, err := Run(context.Background(), definition, options)
	if err != nil {
		t.Fatal(err)
	}
	// Schema 1 predates phase metrics and ReadyAt. Missing fields stay unknown
	// while the current resume contributes only separately observed timing.
	cp.Nodes[0].ReadyAt = time.Time{}
	cp.Nodes[0].RunDurationMS, cp.Nodes[0].VerifyDurationMS = 0, 0
	if err := save(options.Dir, cp); err != nil {
		t.Fatal(err)
	}
	cp, err = Run(context.Background(), definition, options)
	state := stateByID(cp, "a")
	if err != nil || cp.Status != "succeeded" || !state.ReadyAt.IsZero() || state.RunDurationMS != 0 || state.VerifyDurationMS != 0 || state.RecoveryVerifyDurationMS <= 0 {
		t.Fatalf("legacy checkpoint timing was fabricated: %+v %v", cp, err)
	}
}
