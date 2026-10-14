//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/worker/dag"
	"pwnmesh/internal/workergraph"
)

func commandGraphCall(t *testing.T, ctx context.Context, job Job, dir string, spec dag.Spec) (workergraph.Checkpoint, error) {
	t.Helper()
	raw, err := marshalCommandGraphCall(spec)
	if err != nil {
		t.Fatal(err)
	}
	tool := commandGraphTool(job, Options{RunDir: dir})
	if err := agent.ValidateArguments(tool.Schema, raw); err != nil {
		t.Fatal(err)
	}
	reply, callErr := tool.Execute(ctx, raw)
	var response workergraph.Checkpoint
	if reply != "" {
		if err := json.Unmarshal([]byte(reply), &response); err != nil {
			t.Fatal(err)
		}
	}
	return response, callErr
}

// Model calls must state their input contract. Keep this boundary encoding
// separate from canonical node JSON, which omits empty inputs for old hashes.
func marshalCommandGraphCall(spec dag.Spec) ([]byte, error) {
	type toolNode struct {
		dag.Node
		Inputs []dag.Input `json:"inputs"`
	}
	nodes := make([]toolNode, 0, len(spec.Nodes))
	for _, node := range spec.Nodes {
		inputs := node.Inputs
		if inputs == nil {
			inputs = []dag.Input{}
		}
		nodes = append(nodes, toolNode{node, inputs})
	}
	return json.Marshal(struct {
		dag.Spec
		Nodes []toolNode `json:"nodes"`
	}{spec, nodes})
}

func commandNodeValue(t *testing.T, checkpoint workergraph.Checkpoint, id string) (workergraph.NodeState, dag.Output) {
	t.Helper()
	for _, node := range checkpoint.Nodes {
		if node.ID == id {
			var value dag.Output
			if len(node.Output.Value) > 0 {
				if err := json.Unmarshal(node.Output.Value, &value); err != nil {
					t.Fatal(err)
				}
			}
			return node, value
		}
	}
	t.Fatalf("missing node %s", id)
	return workergraph.NodeState{}, dag.Output{}
}

func TestCommandGraphEvidenceReceiptIsByteExactAndDoesNotFinishStep(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	j.ResultContractVersion = 2
	o := Options{RunDir: dir}
	oRuntime := configureRoleTools(t, j, &o)
	spec := dag.Spec{Key: "receipt", Nodes: []dag.Node{
		{ID: "exact", Command: `printf 'first\nsecond'`, Resources: []string{}},
		{ID: "empty", Command: `true`, Resources: []string{}},
		{ID: "large", Command: `head -c 8001 /dev/zero | tr '\0' x`, Resources: []string{}},
		{ID: "binary", Command: `printf '\377'`, Resources: []string{}},
		{ID: "failed", Command: `printf diagnostic; exit 3`, Resources: []string{}, Optional: true},
	}}
	raw, _ := json.Marshal(spec)
	result, err := commandGraphTool(j, o).Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		ObservedAt   string         `json:"observed_at"`
		Verification string         `json:"verification"`
		Evidence     []dag.Evidence `json:"verified_evidence"`
	}
	if err := json.Unmarshal([]byte(result), &receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339Nano, receipt.ObservedAt); err != nil || len(receipt.Evidence) != 4 || !strings.Contains(receipt.Verification, "not task meaning") {
		t.Fatalf("bad verified evidence receipt: %+v %v", receipt, err)
	}
	var exact dag.Evidence
	for _, ref := range receipt.Evidence {
		if ref.NodeID == "exact" {
			exact = ref
			if !ref.PreviewComplete || ref.StartLine != 1 || ref.EndLine != 2 {
				t.Fatalf("incorrect complete evidence line selection: %+v", ref)
			}
		} else if ref.PreviewComplete || ref.StartLine != 0 || ref.EndLine != 0 || ref.NodeID == "failed" {
			t.Fatalf("diagnostic, truncated, empty or binary output certified as complete evidence: %+v", ref)
		}
	}
	if oRuntime.stepFinish == nil {
		t.Fatal("missing explicit finish handoff")
	}
	if _, done := oRuntime.stepFinish.result(); done {
		t.Fatal("command success automatically declared Step completion")
	}
	if _, err := oRuntime.stepFinish.tool().Execute(context.Background(), finishStepInput(t, exact.Path)); err != nil {
		t.Fatalf("verified evidence could not be handed off directly: %v", err)
	}
	// An old succeeded status after a failed restoration must not be offered
	// as verified evidence; nor may the receipt silently certify new bytes.
	if err := os.WriteFile(exact.Path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = commandGraphTool(j, o).Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("changed graph artifact accepted on replay")
	}
	if err := json.Unmarshal([]byte(result), &receipt); err != nil || len(receipt.Evidence) != 0 {
		t.Fatalf("failed replay certified evidence: %+v %v", receipt, err)
	}
}

func TestCommandGraphCapacityAndAutomaticParallelism(t *testing.T) {
	tool := commandGraphTool(graphWrapperJob(t), Options{})
	for _, count := range []int{1, 5, 17, 64} {
		spec := dag.Spec{Key: "capacity"}
		for i := 0; i < count; i++ {
			spec.Nodes = append(spec.Nodes, dag.Node{ID: fmt.Sprintf("node-%02d", i), Command: "true", Resources: []string{}})
		}
		for i := 0; i < count-1; i++ {
			spec.Nodes[count-1].DependsOn = append(spec.Nodes[count-1].DependsOn, workergraph.Dependency{ID: spec.Nodes[i].ID})
		}
		raw, err := marshalCommandGraphCall(spec)
		if err != nil || agent.ValidateArguments(tool.Schema, raw) != nil {
			t.Fatalf("tool schema rejected %d nodes: %v", count, err)
		}
	}
}

func TestCommandGraphActualLoopUsesDefaultToolWithoutExtraPlanning(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	turns := 0
	opts := Options{RunDir: dir, Provider: scenarioProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turns++
		if !slices.ContainsFunc(definitions, func(d agent.Definition) bool { return d.Name == "run_graph" }) {
			t.Fatal("default execution tools omit run_graph")
		}
		if turns == 1 {
			return agent.Message{Role: "assistant", StopReason: "tool_use", Content: []agent.Block{{Type: "tool_use", ID: "graph-call", Name: "run_graph", Input: json.RawMessage(`{"key":"loop","nodes":[{"id":"one","command":"printf result","resources":[],"inputs":[]}]}`)}}}, nil
		}
		if turns != 2 || len(history) == 0 || history[len(history)-1].Content[0].IsError {
			t.Fatalf("graph did not complete within one tool turn: %+v", history)
		}
		return agent.Text("assistant", completedOutput), nil
	})}
	result, err := runTestWorker(context.Background(), job, opts)
	if err != nil || result.Status != "success" || turns != 2 {
		t.Fatalf("existing Loop graph execution failed: %+v %v turns=%d", result, err, turns)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph-tools", "loop", "graph.json")); err != nil {
		t.Fatal(err)
	}
	job.Kind = "reason"
	options := Options{RunDir: dir}
	if err := ConfigureRuntimeTools(job, &options); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(options.Tools, func(tool agent.Tool) bool { return tool.Name == "run_graph" }) {
		t.Fatal("command graph leaked into reason")
	}
}

func TestCommandGraphWorkerAdapterPreservesHistoricalDefinitionAndResume(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	spec := dag.Spec{Key: "legacy-boundary", Nodes: []dag.Node{{ID: "one", Command: "printf hello", Resources: []string{}}}}
	first, err := commandGraphCall(t, context.Background(), job, dir, spec)
	if err != nil || first.Status != "succeeded" {
		t.Fatalf("worker graph failed: %+v %v", first, err)
	}
	node, _ := commandNodeValue(t, first, "one")
	const hash = "f5840869843f9ccd88e30c7ef5ed4d79a9c58f1dc18f34bd78ff2eca38b37ecf"
	if node.DefinitionSHA256 != hash {
		t.Fatalf("worker changed the historical node hash: %s", node.DefinitionSHA256)
	}
	definition, err := os.ReadFile(filepath.Join(dir, "graph-tools", spec.Key, "definitions", hash+".json"))
	if err != nil || string(definition) != `{"id":"one","kind":"command","command":"printf hello","timeout":120,"resources":[]}` {
		t.Fatalf("worker changed retained canonical bytes: %s %v", definition, err)
	}
	again, err := commandGraphCall(t, context.Background(), job, dir, spec)
	if err != nil || again.Status != "succeeded" {
		t.Fatalf("worker restore failed: %+v %v", again, err)
	}
	restored, _ := commandNodeValue(t, again, "one")
	if restored.Attempt != 1 || !restored.StartedAt.Equal(node.StartedAt) || restored.InputSHA256 != node.InputSHA256 {
		t.Fatalf("worker restore replayed or rebound the node: %+v", restored)
	}
}
