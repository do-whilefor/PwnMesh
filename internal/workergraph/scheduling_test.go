//go:build linux

package workergraph

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestSchedulingStartsDependencyChainBeforeIndependentLeavesFillSlots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan string, 4)
	release := make(chan struct{})
	run := func(ctx context.Context, input Input) (Output, error) {
		started <- input.NodeID
		select {
		case <-release:
			return Output{}, nil
		case <-ctx.Done():
			return Output{}, ctx.Err()
		}
	}
	definition := Definition{Version: "1", Nodes: []Node{
		{ID: "a-leaf", Kind: "function", Run: run},
		{ID: "b-leaf", Kind: "function", Run: run},
		{ID: "z-chain-root", Kind: "function", Run: run},
		{ID: "z-chain-end", Kind: "function", DependsOn: []Dependency{{ID: "z-chain-root"}}, Run: run},
	}}
	options := fixtureOptions(t, 2)
	finished := make(chan error, 1)
	go func() {
		_, err := Run(ctx, definition, options)
		finished <- err
	}()
	initial := map[string]bool{}
	for len(initial) < 2 {
		select {
		case id := <-started:
			initial[id] = true
		case err := <-finished:
			t.Fatalf("graph finished before filling slots: %v", err)
		case <-ctx.Done():
			t.Fatal("graph did not fill slots")
		}
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if !initial["z-chain-root"] || !initial["a-leaf"] {
		t.Fatalf("independent leaves delayed the dependency chain: %v", initial)
	}
}

func TestSchedulingRecomputesRemainingDepthAndKeepsStableTies(t *testing.T) {
	options := fixtureOptions(t, 1)
	options.Extend = true
	var order []string
	run := func(_ context.Context, input Input) (Output, error) {
		order = append(order, input.NodeID)
		return Output{}, nil
	}
	definition := Definition{Version: "1", Nodes: []Node{
		{ID: "b-leaf", Kind: "function", Run: run, Verify: verified},
		{ID: "z-end", Kind: "function", DependsOn: []Dependency{{ID: "z-middle"}}, Run: run, Verify: verified},
		{ID: "z-root", Kind: "function", Run: run, Verify: verified},
		{ID: "a-leaf", Kind: "function", Run: run, Verify: verified},
		{ID: "z-middle", Kind: "function", DependsOn: []Dependency{{ID: "z-root"}}, Run: run, Verify: verified},
	}}
	checkpoint, err := Run(context.Background(), definition, options)
	want := []string{"z-root", "z-middle", "a-leaf", "b-leaf", "z-end"}
	if err != nil || checkpoint.Status != "succeeded" || !reflect.DeepEqual(order, want) {
		t.Fatalf("remaining depth or ID tie order incorrect: %v %v", order, err)
	}
	// Extending a completed graph reprioritizes only new work. Reused nodes
	// keep their identity, output and single attempt despite the new order.
	definition.Nodes = append(definition.Nodes,
		Node{ID: "a-new-leaf", Kind: "function", Run: run, Verify: verified},
		Node{ID: "z-new-root", Kind: "function", DependsOn: []Dependency{{ID: "z-end"}}, Run: run, Verify: verified},
		Node{ID: "z-new-end", Kind: "function", DependsOn: []Dependency{{ID: "z-new-root"}}, Run: run, Verify: verified},
	)
	checkpoint, err = Run(context.Background(), definition, options)
	want = append(want, "z-new-root", "a-new-leaf", "z-new-end")
	if err != nil || checkpoint.Status != "succeeded" || !reflect.DeepEqual(order, want) {
		t.Fatalf("extension changed reuse or scheduling: %v %v", order, err)
	}
	for _, state := range checkpoint.Nodes {
		if state.Attempt != 1 {
			t.Fatalf("scheduling replayed node: %+v", state)
		}
	}
}
