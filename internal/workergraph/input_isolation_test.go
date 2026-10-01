//go:build linux

package workergraph

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestLaterNodesKeepTheOriginalInitialInputSnapshot(t *testing.T) {
	options := fixtureOptions(t, 1)
	options.Input = json.RawMessage(`{"scope":"original"}`)
	definition := Definition{Version: "input-isolation", Nodes: []Node{
		{ID: "first", Kind: "function", Run: func(context.Context, Input) (Output, error) {
			copy(options.Input, []byte(`{"scope":"modified"}`))
			return Output{}, nil
		}},
		{ID: "second", Kind: "function", DependsOn: []Dependency{{ID: "first"}}, Run: func(_ context.Context, input Input) (Output, error) {
			if string(input.Initial) != `{"scope":"original"}` {
				t.Errorf("later node received a changed initial input: %s", input.Initial)
			}
			return Output{}, nil
		}},
	}}
	checkpoint, err := Run(context.Background(), definition, options)
	if err != nil || checkpoint.Status != "succeeded" || checkpoint.InputSHA256 != hash(json.RawMessage(`{"scope":"original"}`)) {
		t.Fatalf("graph lost its initial input binding: %+v %v", checkpoint, err)
	}
}

func TestCallbackPhasesCannotMutateBoundGraphInputs(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		name := "live"
		if interrupted {
			name = "reconcile"
		}
		t.Run(name, func(t *testing.T) {
			options := fixtureOptions(t, 1)
			const initial = `{"task":"test"}`
			const value = `{"scope":"original"}`
			const source = `{"source":"original"}`
			const result = `{"result":"original"}`
			artifact := Artifact{Path: "/evidence", SHA256: strings.Repeat("a", 64)}
			checkAndMutate := func(phase string, input Input) {
				t.Helper()
				if string(input.Initial) != initial || string(input.Value) != value || len(input.Dependencies) != 1 {
					t.Errorf("%s received mutated input: %+v", phase, input)
					return
				}
				dep := &input.Dependencies[0]
				if dep.ID != "source" || dep.Status != "succeeded" || string(dep.Output.Value) != source || len(dep.Output.Artifacts) != 1 || dep.Output.Artifacts[0] != artifact {
					t.Errorf("%s received mutated dependency: %+v", phase, dep)
					return
				}
				input.Initial[2], input.Value[2], dep.Output.Value[2] = 'X', 'X', 'X'
				dep.ID, dep.Status, dep.Output.Artifacts[0].Path = "changed", "failed", "/changed"
			}
			var conditions, runs, reconciles, verifies int
			definition := Definition{Version: "phase-isolation", Nodes: []Node{
				{ID: "source", Kind: "function", Verify: verified, Run: func(context.Context, Input) (Output, error) {
					return Output{Value: json.RawMessage(source), Artifacts: []Artifact{artifact}}, nil
				}},
				{ID: "join", Kind: "function", Input: json.RawMessage(value), DependsOn: []Dependency{{ID: "source"}},
					When: func(_ context.Context, input Input) (bool, string, error) {
						conditions++
						checkAndMutate("condition", input)
						return true, "", nil
					},
					Run: func(_ context.Context, input Input) (Output, error) {
						runs++
						checkAndMutate("run", input)
						if interrupted {
							return Output{}, ErrInterrupted
						}
						return Output{Value: json.RawMessage(result)}, nil
					},
					Reconcile: func(_ context.Context, input Input, _ NodeState) (Output, error) {
						reconciles++
						checkAndMutate("reconcile", input)
						return Output{Value: json.RawMessage(result)}, nil
					},
					Verify: func(_ context.Context, input Input, output Output) error {
						verifies++
						checkAndMutate("verify", input)
						if string(output.Value) != result {
							t.Errorf("verification received mutated result: %s", output.Value)
						}
						output.Value[2] = 'X'
						return nil
					},
				},
			}}
			checkpoint, err := Run(context.Background(), definition, options)
			if interrupted {
				if !errors.Is(err, ErrInterrupted) {
					t.Fatalf("expected uncertain callback: %v", err)
				}
				checkpoint, err = Run(context.Background(), definition, options)
			}
			if err != nil || checkpoint.Status != "succeeded" {
				t.Fatalf("callback mutation escaped before recovery: %+v %v", checkpoint, err)
			}
			checkpoint, err = Run(context.Background(), definition, options)
			if err != nil || checkpoint.Status != "succeeded" || string(stateByID(checkpoint, "source").Output.Value) != source || string(stateByID(checkpoint, "join").Output.Value) != result || conditions != 1 || runs != 1 || verifies != 2 || interrupted && reconciles != 1 || !interrupted && reconciles != 0 {
				t.Fatalf("callback mutation changed durable state or replayed work: %+v %v conditions=%d runs=%d reconciles=%d verifies=%d", checkpoint, err, conditions, runs, reconciles, verifies)
			}
		})
	}
}

func TestEmptyInputViewsKeepExistingCheckpointHashes(t *testing.T) {
	for _, initial := range []json.RawMessage{nil, {}} {
		for _, value := range []json.RawMessage{nil, {}} {
			input := nodeInput(Node{ID: "empty", Input: value}, NodeState{}, initial, nil)
			if input.Initial != nil || input.Value != nil || inputHash(input) != inputHash(Input{NodeID: "empty"}) {
				t.Fatalf("empty input representation changed its durable hash: %+v", input)
			}
		}
	}
}
