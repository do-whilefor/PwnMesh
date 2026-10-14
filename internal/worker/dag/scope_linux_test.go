//go:build linux

package dag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"pwnmesh/internal/workergraph"
)

func graphScope(t *testing.T) Scope {
	t.Helper()
	workspace := t.TempDir()
	return Scope{RunID: "run", Workspace: workspace, Identity: func(dir string) (json.RawMessage, error) {
		return json.Marshal(struct {
			RunID     string `json:"run_id"`
			Workspace string `json:"workspace"`
			RunDir    string `json:"run_dir"`
		}{"run", workspace, dir})
	}}
}

func scopeFor(scope Scope, dir string) Scope { scope.RunDir = dir; return scope }

func runGraph(ctx context.Context, scope Scope, dir string, raw json.RawMessage) (string, error) {
	return Run(ctx, scopeFor(scope, dir), raw, nil)
}

// These bytes and hashes were verified against the pre-extraction Worker
// implementation. Expected values do not come from the current encoder.
func TestPersistedGraphIdentityAndNodeDefinitionRemainCompatible(t *testing.T) {
	const identity = `{"project_id":"project","step_id":"step","run_id":"run","previous_run_id":"prior","kind":"explore","workspace":"/workspace","run_dir":"/workspace/run","workspace_target":"/workspace","run_dir_target":"/workspace/run","job_digest":"job","original_budget":{"timeout":120,"conclude_timeout":30,"max_intents":2,"reasoning_effort":"high"}}`
	const initial = `{"identity":{"project_id":"project","step_id":"step","run_id":"run","previous_run_id":"prior","kind":"explore","workspace":"/workspace","run_dir":"/workspace/run","workspace_target":"/workspace","run_dir_target":"/workspace/run","job_digest":"job","original_budget":{"timeout":120,"conclude_timeout":30,"max_intents":2,"reasoning_effort":"high"}},"key":"legacy"}`
	input, err := commandGraphInitialInput(json.RawMessage(identity), "legacy")
	if err != nil || string(input) != initial {
		t.Fatalf("persisted graph input changed: %s %v", input, err)
	}
	digest := sha256.Sum256(input)
	if hex.EncodeToString(digest[:]) != "9e8839bb11ab127ff095875296dcec7553fdd6204ead0b83963b26ab13efd906" {
		t.Fatal("graph input hash changed")
	}
	spec := Spec{Key: "legacy", Nodes: []Node{{ID: "one", Command: "printf hello", Resources: []string{}}}}
	if err := validateCommandGraph(&spec); err != nil {
		t.Fatal(err)
	}
	const canonical = `{"id":"one","kind":"command","command":"printf hello","timeout":120,"resources":[]}`
	raw, err := json.Marshal(spec.Nodes[0])
	if err != nil || string(raw) != canonical {
		t.Fatalf("persisted node definition changed: %s %v", raw, err)
	}
	if got := workergraph.NodeDefinitionSHA256(commandNodeDefinition(spec.Nodes[0])); got != "f5840869843f9ccd88e30c7ef5ed4d79a9c58f1dc18f34bd78ff2eca38b37ecf" {
		t.Fatalf("node definition hash changed: %s", got)
	}
	if commandGraphVersion != "mixed-dag-v3" {
		t.Fatalf("graph protocol version changed: %s", commandGraphVersion)
	}
}

func TestAgentBoundaryFreezesInputsAndReusesWithoutCallingExecutor(t *testing.T) {
	scope := scopeFor(graphScope(t), t.TempDir())
	spec := Spec{Key: "callback", Nodes: []Node{
		{ID: "source", Command: `printf original > result.txt; printf observed`, Resources: []string{}, Artifacts: []string{"result.txt"}},
		{ID: "child", Kind: "agent", Task: "Read the original bytes", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source"}}, Inputs: []Input{{Node: "source", Artifact: "result.txt"}}, Artifacts: []string{"report.txt"}},
	}}
	raw, err := marshalCommandGraphCall(spec)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	execute := func(ctx context.Context, node AgentNode) (workergraph.Output, error) {
		calls++
		if node.Key != spec.Key || node.Spec.ID != "child" || node.Spec.Timeout != 600 {
			return workergraph.Output{}, errors.New("child assignment changed")
		}
		views, dependencies, err := node.PrepareInputs(ctx)
		if err != nil {
			return workergraph.Output{}, err
		}
		retained, err := os.ReadFile(filepath.Join(node.Dir, "dependencies.json"))
		if err != nil || string(retained) != string(dependencies) || len(views) != 1 {
			return workergraph.Output{}, errors.New("dependency bytes changed at callback boundary")
		}
		frozen := views[0].Output.Files["result.txt"]
		if frozen.Path == views[0].Output.Artifacts[1].Path {
			return workergraph.Output{}, errors.New("child received mutable producer path")
		}
		if !slices.Equal(node.Environment(), []string{"PWNMESH_WORKSPACE=" + scope.Workspace, "PWNMESH_NODE_DIR=" + node.Dir, "PWNMESH_DEPENDENCIES=" + filepath.Join(node.Dir, "dependencies.json")}) {
			return workergraph.Output{}, errors.New("child environment changed")
		}
		content, err := os.ReadFile(frozen.Path)
		if err != nil || string(content) != "original" {
			return workergraph.Output{}, errors.New("child input lost its verified bytes")
		}
		if err := os.WriteFile(filepath.Join(node.Dir, "report.txt"), content, 0600); err != nil {
			return workergraph.Output{}, err
		}
		return node.RetainOutput(ctx, "Retained report.txt", true)
	}
	for range 2 {
		reply, err := Run(context.Background(), scope, raw, execute)
		var state workergraph.Checkpoint
		if err != nil || json.Unmarshal([]byte(reply), &state) != nil || state.Status != "succeeded" {
			t.Fatalf("callback graph failed: %s %v", reply, err)
		}
	}
	if calls != 1 {
		t.Fatalf("restoration replayed the Agent executor: %d calls", calls)
	}
}
