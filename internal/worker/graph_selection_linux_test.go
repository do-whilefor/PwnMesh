//go:build linux

package worker

import (
	"context"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
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
}
