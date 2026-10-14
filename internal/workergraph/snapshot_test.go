//go:build linux

package workergraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestValidateSnapshotIsReadOnlyAndChecksBindings(t *testing.T) {
	calls := 0
	definition := Definition{Version: "snapshot-v1", Nodes: []Node{
		{ID: "a", Kind: "function", Input: json.RawMessage(`{"task":"observe"}`), Run: func(context.Context, Input) (Output, error) {
			calls++
			return Output{Value: json.RawMessage(`{"observation":"first"}`)}, nil
		}, Verify: func(context.Context, Input, Output) error { calls++; return nil }},
		{ID: "b", Kind: "function", DependsOn: []Dependency{{ID: "a"}}, Run: func(context.Context, Input) (Output, error) {
			calls++
			return Output{}, errors.New("expected failure")
		}},
	}}
	options := Options{RunID: "snapshot-test", Input: json.RawMessage(`{"identity":"original"}`), Dir: t.TempDir(), Parallelism: 1}
	saved, err := Run(context.Background(), definition, options)
	if err == nil || saved.Status != "failed" {
		t.Fatalf("expected terminal failed source: %+v %v", saved, err)
	}
	before, err := os.ReadFile(filepath.Join(options.Dir, "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	priorCalls := calls
	if err := ValidateSnapshot(definition, options, saved); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(options.Dir, "graph.json"))
	if calls != priorCalls || !bytes.Equal(before, after) {
		t.Fatal("snapshot validation invoked callbacks or changed the checkpoint")
	}
	cases := []struct {
		name string
		edit func(*Checkpoint)
	}{
		{"run", func(c *Checkpoint) { c.RunID = "other-run" }},
		{"initial", func(c *Checkpoint) { c.InputSHA256 = strings.Repeat("0", 64) }},
		{"definition", func(c *Checkpoint) { c.DefinitionSHA256 = strings.Repeat("0", 64) }},
		{"node-definition", func(c *Checkpoint) { c.Nodes[0].DefinitionSHA256 = "" }},
		{"bound-result", func(c *Checkpoint) { c.Nodes[0].Output.Value = json.RawMessage(`{"observation":"different"}`) }},
		{"timing", func(c *Checkpoint) {
			c.Nodes[0].FinishedAt = time.Time{}
		}},
		{"node-input", func(c *Checkpoint) { c.Nodes[1].InputSHA256 = strings.Repeat("0", 64) }},
		{"node-count", func(c *Checkpoint) { c.Nodes = c.Nodes[:1] }},
		{"relative-artifact", func(c *Checkpoint) {
			c.Nodes[0].Output.Artifacts = []Artifact{{Path: "relative", SHA256: strings.Repeat("0", 64)}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var changed Checkpoint
			if err := json.Unmarshal(before, &changed); err != nil {
				t.Fatal(err)
			}
			tc.edit(&changed)
			if err := ValidateSnapshot(definition, options, changed); err == nil {
				t.Fatal("changed source accepted")
			}
		})
	}
}

func TestSavedNodeValidationRejectsBeforeCallbacks(t *testing.T) {
	definition := Definition{Version: "saved-node-v1", Nodes: []Node{{ID: "source", Kind: "function", Run: successful, Verify: verified}}}
	options := fixtureOptions(t, 1)
	if _, err := Run(context.Background(), definition, options); err != nil {
		t.Fatal(err)
	}
	original := checkpointBytes(t, options)
	callbacks := 0
	definition.Nodes[0].Run = func(context.Context, Input) (Output, error) { callbacks++; return Output{}, nil }
	definition.Nodes[0].Verify = func(context.Context, Input, Output) error { callbacks++; return nil }
	for _, tc := range []struct {
		name string
		edit func(*NodeState)
	}{
		{"kind", func(n *NodeState) { n.Kind = "other" }},
		{"negative-attempt", func(n *NodeState) { n.Attempt = -1 }},
		{"second-attempt", func(n *NodeState) { n.Attempt = 2 }},
		{"unknown-status", func(n *NodeState) { n.Status = "unknown" }},
		{"missing-start", func(n *NodeState) { n.StartedAt = time.Time{} }},
		{"missing-finish", func(n *NodeState) { n.FinishedAt = time.Time{} }},
		{"running-without-attempt", func(n *NodeState) { n.Status, n.Attempt = "running", 0 }},
		{"pending-with-attempt", func(n *NodeState) { n.Status = "pending" }},
		{"skipped-with-attempt", func(n *NodeState) { n.Status, n.Reason = "skipped", "condition" }},
		{"blocked-with-attempt", func(n *NodeState) { n.Status = "blocked" }},
		{"relative-artifact", func(n *NodeState) {
			n.Output.Artifacts = []Artifact{{Path: "relative", SHA256: strings.Repeat("0", 64)}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changed Checkpoint
			if err := json.Unmarshal(original, &changed); err != nil {
				t.Fatal(err)
			}
			tc.edit(&changed.Nodes[0])
			raw, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(options.Dir, "graph.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := ValidateSnapshot(definition, options, changed); err == nil {
				t.Fatal("snapshot accepted malformed node")
			}
			if _, err := Run(context.Background(), definition, options); err == nil {
				t.Fatal("resume accepted malformed node")
			}
			if callbacks != 0 || !bytes.Equal(raw, checkpointBytes(t, options)) {
				t.Fatal("rejected node invoked callbacks or rewrote its checkpoint")
			}
		})
	}
}

func TestNodeDefinitionSHA256MatchesRunWithoutMutatingDependencies(t *testing.T) {
	node := Node{ID: "join", Kind: "function", DependsOn: []Dependency{{ID: "a_1"}, {ID: "a.1"}, {ID: "a-1"}, {ID: "a"}, {ID: "A"}, {ID: "1"}}, Run: func(context.Context, Input) (Output, error) { return Output{}, nil }}
	original := slices.Clone(node.DependsOn)
	digest := NodeDefinitionSHA256(node)
	if !reflect.DeepEqual(node.DependsOn, original) {
		t.Fatal("hashing reordered caller-owned dependencies")
	}
	definition := Definition{Version: "v1", Nodes: []Node{node}}
	for _, dependency := range node.DependsOn {
		definition.Nodes = append(definition.Nodes, Node{ID: dependency.ID, Kind: "function", Run: node.Run})
	}
	options := Options{RunID: "test", Dir: t.TempDir(), Parallelism: 2}
	checkpoint, err := Run(context.Background(), definition, options)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, state := range checkpoint.Nodes {
		ids = append(ids, state.ID)
		if state.ID == "join" && state.DefinitionSHA256 != digest {
			t.Fatal("exported node hash differs from Run")
		}
	}
	if !reflect.DeepEqual(ids, []string{"1", "A", "a", "a-1", "a.1", "a_1", "join"}) {
		t.Fatalf("checkpoint ID order changed: %v", ids)
	}
	slices.Reverse(node.DependsOn)
	slices.Reverse(definition.Nodes)
	if NodeDefinitionSHA256(node) != digest {
		t.Fatal("dependency permutation changed node identity")
	}
	if err := ValidateSnapshot(definition, options, checkpoint); err != nil {
		t.Fatalf("definition permutation invalidated saved snapshot: %v", err)
	}
	for i := range definition.Nodes {
		definition.Nodes[i].Verify = verified
		definition.Nodes[i].Run = func(context.Context, Input) (Output, error) {
			return Output{}, errors.New("accepted nodes must not replay after definition permutation")
		}
	}
	resumed, err := Run(context.Background(), definition, options)
	if err != nil || resumed.Status != "succeeded" || resumed.DefinitionSHA256 != checkpoint.DefinitionSHA256 {
		t.Fatalf("definition permutation changed persisted graph identity or replayed work: %+v %v", resumed, err)
	}
}

func TestNodeDefinitionSHA256NormalizesEmptyInputsAndDependencies(t *testing.T) {
	var canonical, canonicalGraph string
	for _, input := range []struct {
		name  string
		value json.RawMessage
	}{{"nil", nil}, {"empty", json.RawMessage{}}} {
		for _, dependencies := range []struct {
			name  string
			value []Dependency
		}{{"nil", nil}, {"empty", []Dependency{}}} {
			t.Run(input.name+"-input-"+dependencies.name+"-dependencies", func(t *testing.T) {
				node := Node{ID: "source", Kind: "function", Input: input.value, DependsOn: dependencies.value, Run: func(context.Context, Input) (Output, error) { return Output{}, nil }}
				digest := NodeDefinitionSHA256(node)
				checkpoint, err := Run(context.Background(), Definition{Version: "v1", Nodes: []Node{node}}, Options{RunID: "test", Dir: t.TempDir(), Parallelism: 1})
				if err != nil || checkpoint.Nodes[0].DefinitionSHA256 != digest {
					t.Fatalf("definition manifest hash differs from durable receipt: digest=%s checkpoint=%+v err=%v", digest, checkpoint, err)
				}
				if canonical == "" {
					canonical = digest
					canonicalGraph = checkpoint.DefinitionSHA256
				} else if canonical != digest {
					t.Fatal("nil and empty inputs/dependencies have different definition hashes")
				}
				if canonicalGraph != checkpoint.DefinitionSHA256 {
					t.Fatal("nil and empty inputs/dependencies have different graph hashes")
				}
			})
		}
	}
}
