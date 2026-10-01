//go:build linux

package workergraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestNodeDefinitionSHA256MatchesRunWithoutMutatingDependencies(t *testing.T) {
	node := Node{ID: "join", Kind: "function", DependsOn: []Dependency{{ID: "b"}, {ID: "a"}}, Run: func(context.Context, Input) (Output, error) { return Output{}, nil }}
	digest := NodeDefinitionSHA256(node)
	if node.DependsOn[0].ID != "b" {
		t.Fatal("hashing reordered caller-owned dependencies")
	}
	definition := Definition{Version: "v1", Nodes: []Node{node, {ID: "a", Kind: "function", Run: node.Run}, {ID: "b", Kind: "function", Run: node.Run}}}
	checkpoint, err := Run(context.Background(), definition, Options{RunID: "test", Dir: t.TempDir(), Parallelism: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range checkpoint.Nodes {
		if state.ID == "join" && state.DefinitionSHA256 != digest {
			t.Fatal("exported node hash differs from Run")
		}
	}
}

func TestNodeDefinitionSHA256NormalizesEmptyInputsAndDependencies(t *testing.T) {
	var canonical string
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
				} else if canonical != digest {
					t.Fatal("nil and empty inputs/dependencies have different definition hashes")
				}
			})
		}
	}
}
