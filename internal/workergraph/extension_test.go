//go:build linux

package workergraph

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func checkpointBytes(t *testing.T, options Options) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(options.Dir, "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestExtendGraphAcrossDecisionStagesWithoutReplaying(t *testing.T) {
	options := fixtureOptions(t, 1)
	options.Extend = true
	runs := map[string]int{}
	definition := Definition{Version: "dynamic-v1"}
	addNode := func(id string, dependencies ...Dependency) {
		definition.Nodes = append(definition.Nodes, Node{ID: id, Kind: "agent", Input: json.RawMessage(`{"task":"` + id + `"}`), DependsOn: dependencies, Verify: verified, Run: func(_ context.Context, input Input) (Output, error) {
			runs[id]++
			for _, dependency := range input.Dependencies {
				if dependency.Status != "succeeded" || string(dependency.Output.Value) != `"`+dependency.ID+`"` {
					t.Errorf("new node lost old dependency result: %+v", dependency)
				}
			}
			// The complete extension is durable before any new callback starts.
			persisted := persistedCheckpoint(t, options)
			if len(persisted.Nodes) != len(definition.Nodes) || persisted.Status != "running" {
				t.Errorf("new node started before extension was committed: %+v", persisted)
			}
			for _, state := range persisted.Nodes {
				if len(state.DefinitionSHA256) != 64 {
					t.Errorf("missing definition binding: %+v", state)
				}
			}
			return Output{Value: json.RawMessage(`"` + id + `"`)}, nil
		}})
	}
	addNode("seed")
	first, err := Run(context.Background(), definition, options)
	if err != nil {
		t.Fatal(err)
	}
	// These IDs sort before the existing seed; merging is by identity, not index.
	for _, id := range []string{"analyst-1", "analyst-2", "analyst-3"} {
		addNode(id, Dependency{ID: "seed"})
	}
	second, err := Run(context.Background(), definition, options)
	if err != nil {
		t.Fatal(err)
	}
	addNode("decision-2", Dependency{ID: "analyst-1"}, Dependency{ID: "analyst-3"})
	addNode("followup", Dependency{ID: "decision-2"})
	third, err := Run(context.Background(), definition, options)
	if err != nil || third.Status != "succeeded" || len(third.Nodes) != 6 {
		t.Fatalf("multi-stage extension failed: %+v %v", third, err)
	}
	if first.DefinitionSHA256 == second.DefinitionSHA256 || second.DefinitionSHA256 == third.DefinitionSHA256 {
		t.Fatal("extension did not bind the full updated definition")
	}
	before, after := stateByID(first, "seed"), stateByID(third, "seed")
	after.RecoveryVerifyDurationMS = before.RecoveryVerifyDurationMS
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("extension rewrote completed state: before=%+v after=%+v", before, after)
	}
	// A later unchanged resume keeps every execution at one attempt.
	if _, err := Run(context.Background(), definition, options); err != nil {
		t.Fatal(err)
	}
	for _, node := range definition.Nodes {
		if runs[node.ID] != 1 {
			t.Errorf("node %s executed %d times", node.ID, runs[node.ID])
		}
	}
}

func TestExtendRejectsChangesToEverySavedNodeDefinition(t *testing.T) {
	mutations := []struct {
		name string
		edit func(*Definition)
	}{
		{"delete", func(d *Definition) { d.Nodes = d.Nodes[:1] }},
		{"kind", func(d *Definition) { d.Nodes[1].Kind = "agent" }},
		{"input", func(d *Definition) { d.Nodes[1].Input = json.RawMessage(`{"changed":true}`) }},
		{"dependencies", func(d *Definition) { d.Nodes[1].DependsOn = nil }},
		{"dependency_optional", func(d *Definition) { d.Nodes[1].DependsOn[0].Optional = true }},
		{"optional", func(d *Definition) { d.Nodes[1].Optional = !d.Nodes[1].Optional }},
		{"condition", func(d *Definition) {
			d.Nodes[1].When = func(context.Context, Input) (bool, string, error) { return true, "", nil }
		}},
	}
	for _, status := range []string{"succeeded", "failed", "pending"} {
		for _, mutation := range mutations {
			t.Run(status+"/"+mutation.name, func(t *testing.T) {
				options := fixtureOptions(t, 1)
				options.Extend = true
				runs := 0
				definition := Definition{Version: "1", Nodes: []Node{
					{ID: "seed", Kind: "function", Run: successful, Verify: verified},
					{ID: "target", Kind: "function", Input: json.RawMessage(`{"original":true}`), DependsOn: []Dependency{{ID: "seed"}}, Optional: true, Verify: verified, Run: func(context.Context, Input) (Output, error) {
						runs++
						if status == "failed" {
							return Output{}, errors.New("optional rejection")
						}
						return Output{}, nil
					}},
				}}
				if status == "pending" {
					definition.Nodes[0].Run = func(context.Context, Input) (Output, error) { return Output{}, ErrInterrupted }
				}
				checkpoint, err := Run(context.Background(), definition, options)
				if err != nil && status != "pending" || stateByID(checkpoint, "target").Status != status {
					t.Fatalf("unexpected initial status: %+v %v", checkpoint, err)
				}
				before := string(checkpointBytes(t, options))
				beforeRuns := runs
				mutation.edit(&definition)
				definition.Nodes = append(definition.Nodes, Node{ID: "new", Kind: "agent", Run: func(context.Context, Input) (Output, error) {
					runs++
					return Output{}, nil
				}})
				if _, err := Run(context.Background(), definition, options); err == nil {
					t.Fatal("changed saved definition accepted")
				}
				if runs != beforeRuns || string(checkpointBytes(t, options)) != before {
					t.Fatal("rejected extension executed work or rewrote checkpoint")
				}
			})
		}
	}
}

func TestExtendPreservesFailureSemantics(t *testing.T) {
	for _, optional := range []bool{false, true} {
		t.Run(fmt.Sprint(optional), func(t *testing.T) {
			options := fixtureOptions(t, 1)
			options.Extend = true
			failures, remedies := 0, 0
			definition := Definition{Version: "1", Nodes: []Node{{ID: "failed", Kind: "agent", Optional: optional, Run: func(context.Context, Input) (Output, error) {
				failures++
				return Output{}, errors.New("evidence unavailable")
			}}}}
			_, _ = Run(context.Background(), definition, options)
			definition.Nodes = append(definition.Nodes, Node{ID: "remedy", Kind: "agent", DependsOn: []Dependency{{ID: "failed", Optional: true}}, Run: func(_ context.Context, input Input) (Output, error) {
				remedies++
				if len(input.Dependencies) != 1 || input.Dependencies[0].Status != "failed" || input.Dependencies[0].Error != "evidence unavailable" {
					t.Errorf("failure not available to follow-up: %+v", input)
				}
				return Output{}, nil
			}})
			checkpoint, err := Run(context.Background(), definition, options)
			if failures != 1 || stateByID(checkpoint, "failed").Status != "failed" {
				t.Fatalf("extension replayed or erased failure: %+v failures=%d", checkpoint, failures)
			}
			if optional {
				if err != nil || checkpoint.Status != "succeeded" || remedies != 1 {
					t.Fatalf("optional failure could not support follow-up: %+v %v remedies=%d", checkpoint, err, remedies)
				}
			} else if err == nil || checkpoint.Status != "failed" || remedies != 0 || stateByID(checkpoint, "remedy").Status != "blocked" {
				t.Fatalf("extension laundered required failure: %+v %v remedies=%d", checkpoint, err, remedies)
			}
		})
	}
}

func TestExtensionRecoveryErrorsNeverPersistOrRunNewNodes(t *testing.T) {
	for _, mode := range []string{"input", "artifact", "missing_verifier", "unreconciled", "reconcile_error"} {
		t.Run(mode, func(t *testing.T) {
			options := fixtureOptions(t, 1)
			options.Extend = true
			artifactPath := filepath.Join(options.Dir, "receipt")
			if err := os.WriteFile(artifactPath, []byte("valid"), 0600); err != nil {
				t.Fatal(err)
			}
			oldRuns, newRuns := 0, 0
			definition := Definition{Version: "1", Nodes: []Node{{ID: "old", Kind: "agent", Run: func(context.Context, Input) (Output, error) {
				oldRuns++
				if mode == "unreconciled" || mode == "reconcile_error" {
					return Output{}, ErrInterrupted
				}
				return Output{Artifacts: []Artifact{{Path: artifactPath, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("valid")))}}}, nil
			}, Verify: func(_ context.Context, _ Input, output Output) error {
				raw, err := os.ReadFile(output.Artifacts[0].Path)
				if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != output.Artifacts[0].SHA256 {
					return errors.New("artifact changed")
				}
				return nil
			}}}}
			first, err := Run(context.Background(), definition, options)
			if err != nil && !errors.Is(err, ErrInterrupted) {
				t.Fatal(err)
			}
			switch mode {
			case "input":
				first.Nodes[0].InputSHA256 = strings.Repeat("0", 64)
				if err := save(options.Dir, first); err != nil {
					t.Fatal(err)
				}
			case "artifact":
				if err := os.WriteFile(artifactPath, []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing_verifier":
				definition.Nodes[0].Verify = nil
			case "reconcile_error":
				definition.Nodes[0].Reconcile = func(context.Context, Input, NodeState) (Output, error) { return Output{}, ErrInterrupted }
			}
			definition.Nodes = append(definition.Nodes, Node{ID: "new", Kind: "agent", Run: func(context.Context, Input) (Output, error) { newRuns++; return Output{}, nil }})
			if _, err := Run(context.Background(), definition, options); err == nil {
				t.Fatal("invalid previous result accepted")
			}
			saved := persistedCheckpoint(t, options)
			if oldRuns != 1 || newRuns != 0 || len(saved.Nodes) != 1 || saved.DefinitionSHA256 != first.DefinitionSHA256 {
				t.Fatalf("failed recovery committed extension: %+v old=%d new=%d", saved, oldRuns, newRuns)
			}
		})
	}
}

func TestExtensionReconcilesBeforeCommittingNewNodes(t *testing.T) {
	options := fixtureOptions(t, 1)
	options.Extend = true
	starts, recoveries, followups := 0, 0, 0
	definition := Definition{Version: "1", Nodes: []Node{{ID: "old", Kind: "agent", Verify: verified, Run: func(context.Context, Input) (Output, error) {
		starts++
		return Output{}, ErrInterrupted
	}}}}
	first, err := Run(context.Background(), definition, options)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatal(err)
	}
	definition.Nodes[0].Reconcile = func(context.Context, Input, NodeState) (Output, error) {
		recoveries++
		saved := persistedCheckpoint(t, options)
		if len(saved.Nodes) != 1 || saved.DefinitionSHA256 != first.DefinitionSHA256 {
			t.Errorf("extension committed before reconciliation: %+v", saved)
		}
		return Output{Value: json.RawMessage(`"receipt"`)}, nil
	}
	definition.Nodes = append(definition.Nodes, Node{ID: "new", Kind: "agent", DependsOn: []Dependency{{ID: "old"}}, Verify: verified, Run: func(_ context.Context, input Input) (Output, error) {
		followups++
		if len(input.Dependencies) != 1 || string(input.Dependencies[0].Output.Value) != `"receipt"` {
			t.Errorf("follow-up did not receive reconciled result: %+v", input)
		}
		return Output{}, nil
	}})
	for i := 0; i < 2; i++ {
		checkpoint, err := Run(context.Background(), definition, options)
		if err != nil || checkpoint.Status != "succeeded" || starts != 1 || recoveries != 1 || followups != 1 {
			t.Fatalf("unsafe extension after recovery: %+v %v starts=%d recoveries=%d followups=%d", checkpoint, err, starts, recoveries, followups)
		}
	}
}

func TestExtensionRequiresExplicitOptionAndStableRunIdentity(t *testing.T) {
	for _, mode := range []string{"default", "run_id", "version", "initial_input"} {
		t.Run(mode, func(t *testing.T) {
			options := fixtureOptions(t, 1)
			definition := Definition{Version: "1", Nodes: []Node{{ID: "old", Kind: "agent", Run: successful, Verify: verified}}}
			if _, err := Run(context.Background(), definition, options); err != nil {
				t.Fatal(err)
			}
			before := string(checkpointBytes(t, options))
			newRuns := 0
			definition.Nodes = append(definition.Nodes, Node{ID: "new", Kind: "agent", Run: func(context.Context, Input) (Output, error) { newRuns++; return Output{}, nil }})
			options.Extend = mode != "default"
			switch mode {
			case "run_id":
				options.RunID = "another-run"
			case "version":
				definition.Version = "2"
			case "initial_input":
				options.Input = json.RawMessage(`{"changed":true}`)
			}
			if _, err := Run(context.Background(), definition, options); err == nil || newRuns != 0 || string(checkpointBytes(t, options)) != before {
				t.Fatalf("unauthorized extension changed run: %v new=%d", err, newRuns)
			}
		})
	}
}

func TestLegacyCheckpointNeedsStrictRecoveryBeforeExtension(t *testing.T) {
	options := fixtureOptions(t, 1)
	definition := Definition{Version: "1", Nodes: []Node{{ID: "old", Kind: "agent", Run: successful, Verify: verified}}}
	first, err := Run(context.Background(), definition, options)
	if err != nil {
		t.Fatal(err)
	}
	first.Nodes[0].DefinitionSHA256 = ""
	if err := save(options.Dir, first); err != nil {
		t.Fatal(err)
	}
	before := string(checkpointBytes(t, options))
	extended := Definition{Version: definition.Version, Nodes: append(append([]Node(nil), definition.Nodes...), Node{ID: "new", Kind: "agent", Run: successful, Verify: verified})}
	options.Extend = true
	if _, err := Run(context.Background(), extended, options); err == nil || !strings.Contains(err.Error(), "legacy") || string(checkpointBytes(t, options)) != before {
		t.Fatalf("legacy extension bypassed original definition validation: %v", err)
	}
	options.Extend = false
	restored, err := Run(context.Background(), definition, options)
	if err != nil || len(restored.Nodes[0].DefinitionSHA256) != 64 {
		t.Fatalf("strict legacy recovery failed: %+v %v", restored, err)
	}
	options.Extend = true
	if _, err := Run(context.Background(), extended, options); err != nil {
		t.Fatalf("verified legacy checkpoint could not extend: %v", err)
	}
}

func TestExtensionKeepsInputAndDependencySnapshotsIsolated(t *testing.T) {
	options := fixtureOptions(t, 1)
	options.Extend = true
	definition := Definition{Version: "1", Nodes: []Node{{ID: "old", Kind: "agent", Run: successful, Verify: verified}}}
	if _, err := Run(context.Background(), definition, options); err != nil {
		t.Fatal(err)
	}
	newInput := json.RawMessage(`"original"`)
	definition.Nodes[0].Verify = func(_ context.Context, input Input, output Output) error {
		copy(newInput, []byte(`"modified"`))
		copy(input.Initial, []byte(`{"task":"evil"}`))
		copy(output.Value, []byte(`{"ok":null}`))
		return nil
	}
	definition.Nodes = append(definition.Nodes, Node{ID: "new", Kind: "agent", Input: newInput, DependsOn: []Dependency{{ID: "old"}}, Run: func(_ context.Context, input Input) (Output, error) {
		if string(input.Value) != `"original"` || string(input.Initial) != `{"task":"test"}` || string(input.Dependencies[0].Output.Value) != `{"ok":true}` {
			t.Errorf("recovery callback mutated later inputs: %+v", input)
		}
		input.Dependencies[0].Output.Value[0] = '['
		return Output{}, nil
	}})
	checkpoint, err := Run(context.Background(), definition, options)
	if err != nil || string(stateByID(checkpoint, "old").Output.Value) != `{"ok":true}` {
		t.Fatalf("new callback mutated old checkpoint: %+v %v", checkpoint, err)
	}
}
