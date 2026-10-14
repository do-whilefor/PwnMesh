//go:build linux

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/worker/dag"
	"pwnmesh/internal/workergraph"
)

func reuseSource(t *testing.T, failed bool) (Job, string, workergraph.Checkpoint) {
	t.Helper()
	j, dir := graphWrapperJob(t), t.TempDir()
	spec := dag.Spec{Key: "original", Parallelism: 1, Nodes: []dag.Node{
		{ID: "download", Command: `printf 'once\n' >> "$PWNMESH_WORKSPACE/effects"; printf '{"version":1}\n' > result.json; printf 'downloaded\n'`, Resources: []string{}, Artifacts: []string{"result.json"}},
	}}
	if failed {
		spec.Nodes = append(spec.Nodes, dag.Node{ID: "analysis", Command: "exit 3", DependsOn: []workergraph.Dependency{{ID: "download"}}, Resources: []string{}, Artifacts: []string{}})
	}
	checkpoint, err := commandGraphCall(t, context.Background(), j, dir, spec)
	if failed && (err == nil || checkpoint.Status != "failed") || !failed && (err != nil || checkpoint.Status != "succeeded") {
		t.Fatalf("source did not reach expected state: %+v %v", checkpoint, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "graph-tools", "original", "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal(err)
	}
	return j, dir, checkpoint
}

func TestCommandGraphPreservesOriginalInputBinding(t *testing.T) {
	j, dir, checkpoint := reuseSource(t, false)
	identity, err := identityFor(j, dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	// This is the original checkpoint wire format, including field order.
	original := []byte(`{"identity":` + string(raw) + `,"key":"original"}`)
	digest := sha256.Sum256(original)
	if checkpoint.InputSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("command graph input changed its persisted binding: %s, checkpoint=%s, err=%v", original, checkpoint.InputSHA256, err)
	}
}

func TestCommandGraphReusePreservesAgentOriginWithoutAnotherModelCall(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	calls := 0
	opts := Options{RunDir: dir, graphProvider: func(string) (agent.Provider, error) {
		return scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
			calls++
			return agent.Message{Role: "assistant", Content: []agent.Block{{Type: "text", Text: "An interpretation, not raw evidence."}}}, nil
		}), nil
	}}
	source := dag.Spec{Key: "original", Nodes: []dag.Node{{ID: "interpret", Kind: "agent", Task: "Interpret", Resources: []string{}, Artifacts: []string{}}}}
	if _, err := mixedGraphCall(t, context.Background(), j, opts, source); err != nil {
		t.Fatal(err)
	}
	spec := dag.Spec{Key: "repair", Nodes: []dag.Node{{ID: "reused", Kind: "reuse", ReuseFrom: &dag.Reuse{Key: "original", Node: "interpret"}, Resources: []string{}}}}
	raw, _ := json.Marshal(spec)
	response, err := runCommandGraph(context.Background(), j, opts, raw)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Nodes    []workergraph.NodeState `json:"nodes"`
		Evidence []dag.Evidence          `json:"verified_evidence"`
	}
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(result.Nodes) != 1 || result.Nodes[0].Kind != "agent" || len(result.Evidence) != 0 {
		t.Fatalf("reuse reran model or promoted interpretation to evidence: calls=%d result=%+v", calls, result)
	}
}
