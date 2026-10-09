//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/workergraph"
)

// Optional delegation must remain a choice in the request the model receives,
// while a simple execution can finish without creating a local DAG.
func TestSimpleExecutionKeepsDirectToolsAndOptionalGraph(t *testing.T) {
	j, dir := outcomeJob(t, "explore"), t.TempDir()
	calls := 0
	opts := Options{RunDir: dir, Provider: scenarioProvider(func(_ context.Context, history []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		prompt := history[0].Text()
		if !strings.Contains(prompt, "direct tools for simple work") || !strings.Contains(prompt, "run_graph only when") {
			t.Fatal("request lost the optional graph selection policy")
		}
		available := map[string]bool{}
		for _, def := range defs {
			available[def.Name] = true
		}
		if !available["bash"] || !available["read"] || !available["run_graph"] {
			t.Fatal("simple execution must retain direct and graph tools")
		}
		return agent.Text("assistant", `{"accepted":false,"reason":"No graph is needed to identify this synthetic missing input."}`), nil
	})}
	result, err := runTestWorker(context.Background(), j, opts)
	if err != nil || result.Status != "success" || calls != 1 {
		t.Fatalf("direct execution failed: result=%+v err=%v calls=%d", result, err, calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph-tools")); !os.IsNotExist(err) {
		t.Fatal("simple execution created an unnecessary local DAG")
	}
}

func TestLocalGraphSuccessReturnsToParentBeforeStepAcceptance(t *testing.T) {
	job, dir := outcomeJob(t, "explore"), t.TempDir()
	calls := 0
	provider := scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls == 1 {
			return draftModelCall("inspect", "run_graph", `{"key":"inspection","nodes":[{"id":"one","command":"printf observed","resources":[],"inputs":[]}]}`), nil
		}
		if calls != 2 {
			t.Fatalf("unexpected parent turn %d", calls)
		}
		reply, err := dynamicToolReply(history, "inspect")
		var checkpoint workergraph.Checkpoint
		if err != nil || json.Unmarshal([]byte(reply), &checkpoint) != nil || checkpoint.Status != "succeeded" {
			t.Fatalf("parent did not receive successful node results: %s %v", reply, err)
		}
		_, output := commandNodeValue(t, checkpoint, "one")
		if output.Stdout != "observed" {
			t.Fatalf("parent lost the actual command observation: %+v", output)
		}
		if state := outcomeSession(t, dir); state.Result != nil {
			t.Fatal("local graph success accepted the Step before parent assessment")
		}
		raw, err := os.ReadFile(filepath.Join(dir, "graph", "acceptance.json"))
		var receipt acceptanceReceipt
		if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.Status != "running" || len(receipt.Output.Value) != 0 {
			t.Fatalf("local graph success prematurely sealed the Worker result: %s %v", raw, err)
		}
		return agent.Text("assistant", `{"accepted":false,"reason":"The observed fixture does not establish the assigned claim."}`), nil
	})
	result, err := runTestWorker(context.Background(), job, Options{RunDir: dir, Provider: provider})
	if err != nil || result.Status != "success" || calls != 2 || !strings.Contains(result.Text, `"accepted":false`) {
		t.Fatalf("parent did not retain the Step decision: %+v %v calls=%d", result, err, calls)
	}
}
