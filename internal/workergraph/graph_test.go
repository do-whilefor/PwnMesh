//go:build linux

package workergraph

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func fixtureOptions(t *testing.T, parallelism int) Options {
	t.Helper()
	return Options{RunID: "run-1", Dir: t.TempDir(), Input: json.RawMessage(`{"task":"test"}`), Parallelism: parallelism}
}

func successful(_ context.Context, _ Input) (Output, error) {
	return Output{Value: json.RawMessage(`{"ok":true}`)}, nil
}
func verified(_ context.Context, _ Input, _ Output) error { return nil }
func stateByID(checkpoint Checkpoint, id string) NodeState {
	for _, state := range checkpoint.Nodes {
		if state.ID == id {
			return state
		}
	}
	return NodeState{}
}

func TestParallelStableJoinAndCapacity(t *testing.T) {
	options := fixtureOptions(t, 2)
	started, released := make(chan string, 2), map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
	bVerified := make(chan struct{})
	var active, maximum atomic.Int32
	var joined []string
	branch := func(ctx context.Context, input Input) (Output, error) {
		count := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); count > old && !maximum.CompareAndSwap(old, count); old = maximum.Load() {
		}
		started <- input.NodeID
		select {
		case <-released[input.NodeID]:
		case <-ctx.Done():
			return Output{}, ctx.Err()
		}
		return Output{Value: json.RawMessage(`"` + input.NodeID + `"`)}, nil
	}
	definition := Definition{Version: "1", Nodes: []Node{
		{ID: "join", Kind: "function", DependsOn: []Dependency{{ID: "b"}, {ID: "a"}}, Run: func(_ context.Context, input Input) (Output, error) {
			for _, state := range input.Dependencies {
				joined = append(joined, state.ID+":"+string(state.Output.Value))
			}
			return Output{}, nil
		}},
		{ID: "b", Kind: "agent", Run: branch, Verify: func(context.Context, Input, Output) error { close(bVerified); return nil }}, {ID: "a", Kind: "agent", Run: branch},
	}}
	type result struct {
		checkpoint Checkpoint
		err        error
	}
	finished := make(chan result, 1)
	go func() { cp, err := Run(context.Background(), definition, options); finished <- result{cp, err} }()
	nextStarted := func() string {
		select {
		case id := <-started:
			return id
		case result := <-finished:
			t.Fatalf("graph exited before starting branches: %v", result.err)
			return ""
		}
	}
	first, second := nextStarted(), nextStarted()
	if first == second {
		t.Fatal("same branch started twice")
	}
	close(released["b"])
	select {
	case <-bVerified:
	case result := <-finished:
		t.Fatalf("graph exited before branch b was accepted: %v", result.err)
	}
	close(released["a"])
	got := <-finished
	if got.err != nil || got.checkpoint.Status != "succeeded" || maximum.Load() != 2 {
		t.Fatalf("parallel run: %+v %v max=%d", got.checkpoint, got.err, maximum.Load())
	}
	if !reflect.DeepEqual(joined, []string{`a:"a"`, `b:"b"`}) {
		t.Fatalf("unstable dependency order: %v", joined)
	}
	if got.checkpoint.Nodes[0].ID != "a" || got.checkpoint.Nodes[1].ID != "b" || got.checkpoint.Nodes[2].ID != "join" {
		t.Fatal("unstable checkpoint ordering")
	}
	for _, state := range got.checkpoint.Nodes {
		if state.Attempt != 1 || len(state.InputSHA256) != 64 || state.StartedAt.IsZero() || state.FinishedAt.Before(state.StartedAt) {
			t.Fatalf("missing identity/timing: %+v", state)
		}
	}
}

func TestRequiredFailureCancelsAndWaitsForOtherBranch(t *testing.T) {
	options := fixtureOptions(t, 2)
	started, exited := make(chan struct{}), make(chan struct{})
	var joined atomic.Bool
	definition := Definition{Version: "1", Nodes: []Node{
		{ID: "a", Kind: "agent", Run: func(ctx context.Context, _ Input) (Output, error) {
			close(started)
			<-ctx.Done()
			close(exited)
			return Output{}, ctx.Err()
		}},
		{ID: "b", Kind: "function", Run: func(context.Context, Input) (Output, error) {
			<-started
			return Output{}, errors.New("required evidence missing")
		}},
		{ID: "join", Kind: "function", DependsOn: []Dependency{{ID: "a"}, {ID: "b"}}, Run: func(context.Context, Input) (Output, error) { joined.Store(true); return Output{}, nil }},
	}}
	cp, err := Run(context.Background(), definition, options)
	if err == nil || cp.Status != "failed" || joined.Load() || stateByID(cp, "join").Status != "blocked" {
		t.Fatalf("required failure accepted: %+v %v", cp, err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("Run returned before branch exited")
	}
}

func TestOptionalFailureAndConditionalSkipAreVisibleAtJoin(t *testing.T) {
	options := fixtureOptions(t, 2)
	var joined []string
	definition := Definition{Version: "1", Nodes: []Node{
		{ID: "optional", Kind: "function", Optional: true, Run: func(context.Context, Input) (Output, error) { return Output{}, errors.New("optional unavailable") }},
		{ID: "skip", Kind: "function", Run: successful, When: func(context.Context, Input) (bool, string, error) { return false, "predicate already satisfied", nil }},
		{ID: "join", Kind: "function", DependsOn: []Dependency{{ID: "skip", Optional: true}, {ID: "optional", Optional: true}}, Run: func(_ context.Context, input Input) (Output, error) {
			for _, dep := range input.Dependencies {
				joined = append(joined, dep.ID+":"+dep.Status+":"+dep.Reason)
			}
			return Output{}, nil
		}},
	}}
	cp, err := Run(context.Background(), definition, options)
	if err != nil || cp.Status != "succeeded" || !reflect.DeepEqual(joined, []string{"optional:failed:", "skip:skipped:predicate already satisfied"}) {
		t.Fatalf("partial results lost: %+v %v %v", cp, err, joined)
	}
}

func TestRequiredDependencyCannotUseSkippedNode(t *testing.T) {
	cp, err := Run(context.Background(), Definition{Version: "1", Nodes: []Node{
		{ID: "skip", Kind: "function", Run: successful, When: func(context.Context, Input) (bool, string, error) { return false, "unselected branch", nil }},
		{ID: "join", Kind: "function", DependsOn: []Dependency{{ID: "skip"}}, Run: successful},
	}}, fixtureOptions(t, 1))
	if err == nil || cp.Status != "failed" || stateByID(cp, "join").Status != "blocked" {
		t.Fatalf("skip treated as successful evidence: %+v %v", cp, err)
	}
}

func TestSkipRequiresReason(t *testing.T) {
	cp, err := Run(context.Background(), Definition{Version: "1", Nodes: []Node{{ID: "skip", Kind: "function", Run: successful, When: func(context.Context, Input) (bool, string, error) { return false, "", nil }}}}, fixtureOptions(t, 1))
	if err == nil || cp.Status != "failed" {
		t.Fatalf("silent skip accepted: %+v %v", cp, err)
	}
}

func TestSuccessRecoveryVerifiesAndDoesNotExecuteAgain(t *testing.T) {
	options := fixtureOptions(t, 1)
	var runs, verifies int
	definition := Definition{Version: "1", Nodes: []Node{{ID: "agent", Kind: "agent", Run: func(ctx context.Context, input Input) (Output, error) { runs++; return successful(ctx, input) }, Verify: func(context.Context, Input, Output) error { verifies++; return nil }}}}
	first, err := Run(context.Background(), definition, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(context.Background(), definition, options)
	if stateByID(second, "agent").RecoveryVerifyDurationMS <= 0 {
		t.Fatal("recovery verification was not timed separately")
	}
	second.Nodes[0].RecoveryVerifyDurationMS = 0
	if err != nil || runs != 1 || verifies != 2 || !reflect.DeepEqual(first.Nodes, second.Nodes) {
		t.Fatalf("success replayed: runs=%d verifies=%d %v", runs, verifies, err)
	}
	definition.Nodes[0].Verify = func(context.Context, Input, Output) error { return errors.New("artifact changed") }
	if _, err := Run(context.Background(), definition, options); err == nil || !strings.Contains(err.Error(), "artifact changed") {
		t.Fatal("changed artifact reused")
	}
	definition.Nodes[0].Verify = nil
	if _, err := Run(context.Background(), definition, options); err == nil || !strings.Contains(err.Error(), "requires verification") {
		t.Fatal("unverified result reused")
	}
}

func TestInterruptedRecoveryRequiresReconciliation(t *testing.T) {
	options := fixtureOptions(t, 1)
	var starts, recoveries int
	definition := Definition{Version: "1", Nodes: []Node{{ID: "agent", Kind: "agent", Run: func(context.Context, Input) (Output, error) { starts++; return Output{}, ErrInterrupted }, Verify: verified}}}
	cp, err := Run(context.Background(), definition, options)
	if !errors.Is(err, ErrInterrupted) || cp.Status != "interrupted" || stateByID(cp, "agent").Status != "running" {
		t.Fatalf("interruption became terminal: %+v %v", cp, err)
	}
	if _, err := Run(context.Background(), definition, options); err == nil || !strings.Contains(err.Error(), "reconciliation required") || starts != 1 {
		t.Fatalf("uncertain side effect replayed: %v starts=%d", err, starts)
	}
	definition.Nodes[0].Reconcile = func(_ context.Context, input Input, state NodeState) (Output, error) {
		recoveries++
		if input.Attempt != 1 || state.Status != "running" {
			t.Fatal("reconciliation identity changed")
		}
		if recoveries == 1 {
			return Output{}, ErrInterrupted
		}
		return Output{Value: json.RawMessage(`{"reconciled":true}`)}, nil
	}
	if _, err := Run(context.Background(), definition, options); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("retryable reconciliation lost: %v", err)
	}
	cp, err = Run(context.Background(), definition, options)
	if err != nil || cp.Status != "succeeded" || starts != 1 || recoveries != 2 || stateByID(cp, "agent").Attempt != 1 {
		t.Fatalf("incorrect recovery: %+v %v", cp, err)
	}
}

func TestContextCancellationWaitsAndPreservesUncertainBoundary(t *testing.T) {
	options := fixtureOptions(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	started, exited := make(chan struct{}), make(chan struct{})
	definition := Definition{Version: "1", Nodes: []Node{{ID: "agent", Kind: "agent", Run: func(ctx context.Context, _ Input) (Output, error) {
		close(started)
		<-ctx.Done()
		close(exited)
		return Output{}, ctx.Err()
	}}}}
	go func() { <-started; cancel() }()
	cp, err := Run(ctx, definition, options)
	if !errors.Is(err, context.Canceled) || cp.Status != "interrupted" || stateByID(cp, "agent").Status != "running" {
		t.Fatalf("wrong cancellation: %+v %v", cp, err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("cancelled graph left child running")
	}
}

func TestIdentityDefinitionAndInputBoundOnRecovery(t *testing.T) {
	for _, test := range []string{"run", "version", "input", "node_input", "kind", "edge", "conditional", "corrupt_input_hash"} {
		t.Run(test, func(t *testing.T) {
			options := fixtureOptions(t, 1)
			definition := Definition{Version: "1", Nodes: []Node{{ID: "a", Kind: "function", Run: successful, Verify: verified}, {ID: "b", Kind: "agent", Run: successful, Verify: verified}}}
			if _, err := Run(context.Background(), definition, options); err != nil {
				t.Fatal(err)
			}
			switch test {
			case "run":
				options.RunID = "run-2"
			case "version":
				definition.Version = "2"
			case "input":
				options.Input = json.RawMessage(`{"task":"changed"}`)
			case "node_input":
				definition.Nodes[0].Input = json.RawMessage(`"changed"`)
			case "kind":
				definition.Nodes[0].Kind = "agent"
			case "edge":
				definition.Nodes[1].DependsOn = []Dependency{{ID: "a"}}
			case "conditional":
				definition.Nodes[0].When = func(context.Context, Input) (bool, string, error) { return true, "", nil }
			case "corrupt_input_hash":
				raw, _ := os.ReadFile(filepath.Join(options.Dir, "graph.json"))
				var cp Checkpoint
				_ = json.Unmarshal(raw, &cp)
				cp.Nodes[0].InputSHA256 = strings.Repeat("0", 64)
				if err := save(options.Dir, cp); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Run(context.Background(), definition, options); err == nil {
				t.Fatal("changed identity accepted")
			}
		})
	}
}

func TestRejectInvalidGraphBeforeCallbacks(t *testing.T) {
	for _, test := range []struct {
		name  string
		nodes []Node
	}{
		{"duplicate", []Node{{ID: "a", Kind: "function", Run: successful}, {ID: "a", Kind: "function", Run: successful}}},
		{"self", []Node{{ID: "a", Kind: "function", Run: successful, DependsOn: []Dependency{{ID: "a"}}}}},
		{"missing", []Node{{ID: "a", Kind: "function", Run: successful, DependsOn: []Dependency{{ID: "missing"}}}}},
		{"cycle", []Node{{ID: "a", Kind: "function", Run: successful, DependsOn: []Dependency{{ID: "b"}}}, {ID: "b", Kind: "function", Run: successful, DependsOn: []Dependency{{ID: "a"}}}}},
		{"unsafe_id", []Node{{ID: "../node", Kind: "function", Run: successful}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Run(context.Background(), Definition{Version: "1", Nodes: test.nodes}, fixtureOptions(t, 1)); err == nil {
				t.Fatal("invalid graph accepted")
			}
		})
	}
}

func TestCallbackInputsCannotMutateSiblingOrCheckpoint(t *testing.T) {
	options := fixtureOptions(t, 2)
	definition := Definition{Version: "1", Nodes: []Node{
		{ID: "source", Kind: "function", Run: successful, Verify: verified},
		{ID: "a", Kind: "function", DependsOn: []Dependency{{ID: "source"}}, Run: func(_ context.Context, input Input) (Output, error) {
			input.Dependencies[0].Output.Value[2] = 'X'
			input.Initial[2] = 'X'
			return Output{}, nil
		}},
		{ID: "b", Kind: "function", DependsOn: []Dependency{{ID: "source"}}, Run: func(_ context.Context, input Input) (Output, error) {
			if string(input.Dependencies[0].Output.Value) != `{"ok":true}` || string(input.Initial) != `{"task":"test"}` {
				return Output{}, errors.New("shared mutable input")
			}
			return Output{}, nil
		}},
	}}
	cp, err := Run(context.Background(), definition, options)
	if err != nil || string(stateByID(cp, "source").Output.Value) != `{"ok":true}` {
		t.Fatalf("input alias: %+v %v", cp, err)
	}
}

func TestMalformedOutputCannotBecomeEmptySuccess(t *testing.T) {
	cp, err := Run(context.Background(), Definition{Version: "1", Nodes: []Node{{ID: "bad", Kind: "function", Run: func(context.Context, Input) (Output, error) {
		return Output{Value: json.RawMessage(`{"unfinished":`)}, nil
	}}}}, fixtureOptions(t, 1))
	if err == nil || cp.Status != "failed" || stateByID(cp, "bad").Status != "failed" {
		t.Fatalf("malformed JSON accepted: %+v %v", cp, err)
	}
}

func TestPanicLeavesReconciliationBoundary(t *testing.T) {
	cp, err := Run(context.Background(), Definition{Version: "1", Nodes: []Node{{ID: "uncertain", Kind: "function", Run: func(context.Context, Input) (Output, error) { panic("after external operation") }}}}, fixtureOptions(t, 1))
	if !errors.Is(err, ErrInterrupted) || cp.Status != "interrupted" || stateByID(cp, "uncertain").Status != "running" {
		t.Fatalf("panic lost uncertain side effects: %+v %v", cp, err)
	}
}

func TestRecoveryBindsOptionalFailureDetails(t *testing.T) {
	options := fixtureOptions(t, 1)
	definition := Definition{Version: "1", Nodes: []Node{
		{ID: "optional", Kind: "function", Optional: true, Run: func(context.Context, Input) (Output, error) {
			return Output{}, errors.New("original unavailable resource")
		}},
		{ID: "join", Kind: "function", DependsOn: []Dependency{{ID: "optional", Optional: true}}, Run: successful, Verify: verified},
	}}
	cp, err := Run(context.Background(), definition, options)
	if err != nil {
		t.Fatal(err)
	}
	for n := range cp.Nodes {
		if cp.Nodes[n].ID == "optional" {
			cp.Nodes[n].Error = "different blocker"
		}
	}
	if err = save(options.Dir, cp); err != nil {
		t.Fatal(err)
	}
	if _, err = Run(context.Background(), definition, options); err == nil || !strings.Contains(err.Error(), "input binding changed") {
		t.Fatalf("changed optional input reused: %v", err)
	}
}
