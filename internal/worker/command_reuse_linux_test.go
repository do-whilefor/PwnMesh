//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/process"
	"pwnmesh/internal/workergraph"
)

func reuseSource(t *testing.T, failed bool) (Job, string, workergraph.Checkpoint) {
	t.Helper()
	j, dir := graphWrapperJob(t), t.TempDir()
	spec := commandGraphSpec{Key: "original", Parallelism: 1, Nodes: []commandGraphNode{
		{ID: "download", Command: `printf 'once\n' >> "$PWNMESH_WORKSPACE/effects"; printf '{"version":1}\n' > result.json; printf 'downloaded\n'`, Resources: []string{}, Artifacts: []string{"result.json"}},
	}}
	if failed {
		spec.Nodes = append(spec.Nodes, commandGraphNode{ID: "analysis", Command: "exit 3", DependsOn: []workergraph.Dependency{{ID: "download"}}, Resources: []string{}, Artifacts: []string{}})
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

func reuseGraph() commandGraphSpec {
	return commandGraphSpec{Key: "repair", Nodes: []commandGraphNode{
		{ID: "downloaded", Kind: "reuse", ReuseFrom: &commandGraphReuse{Key: "original", Node: "download"}, Resources: []string{}},
		{ID: "fixed", Command: `python3 -c 'import json, os; deps=json.load(open(os.environ["PWNMESH_DEPENDENCIES"])); item=next(d for d in deps if d["id"]=="downloaded"); data=json.load(open(item["output"]["files"]["result.json"]["path"])); assert data["version"]==1; open("report.txt","w").write("repaired\n")' && printf 'fixed\n' >> "$PWNMESH_WORKSPACE/repair-effects" && printf 'fixed\n'`, DependsOn: []workergraph.Dependency{{ID: "downloaded"}}, Resources: []string{}, Artifacts: []string{"report.txt"}, Inputs: []commandGraphInput{{Node: "downloaded", Artifact: "result.json"}}, When: &commandGraphWhen{Node: "downloaded", Contains: "downloaded"}},
	}}
}

func TestCommandGraphReuseSuccessAfterRequiredFailureWithoutReplaying(t *testing.T) {
	j, dir, source := reuseSource(t, true)
	sourcePath := filepath.Join(dir, "graph-tools", "original", "graph.json")
	before, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	spec := reuseGraph()
	for attempt := 0; attempt < 2; attempt++ {
		checkpoint, err := commandGraphCall(t, context.Background(), j, dir, spec)
		if err != nil || checkpoint.Status != "succeeded" {
			t.Fatalf("reuse attempt %d failed: %+v %v", attempt, checkpoint, err)
		}
		imported, value := commandNodeValue(t, checkpoint, "downloaded")
		original, _ := commandNodeValue(t, source, "download")
		if value.ReusedFrom == nil || value.ReusedFrom.Key != "original" || value.ReusedFrom.Node != "download" || value.ReusedFrom.DefinitionSHA256 != original.DefinitionSHA256 || imported.Kind != original.Kind || value.OutputPath != filepath.Join(dir, "graph-tools", "original", "nodes", "download", "stdout.log") {
			t.Fatalf("reuse lost source identity or output path: %+v", value)
		}
	}
	after, _ := os.ReadFile(sourcePath)
	effects, _ := os.ReadFile(filepath.Join(j.Workspace, "effects"))
	repairEffects, _ := os.ReadFile(filepath.Join(j.Workspace, "repair-effects"))
	if !bytes.Equal(before, after) || string(effects) != "once\n" || string(repairEffects) != "fixed\n" {
		t.Fatalf("reuse rewrote failed graph or repeated work: effects=%q repair=%q", effects, repairEffects)
	}
}

func TestCommandGraphReuseSourceWithExplicitEmptyDependencies(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	// Use actual tool JSON: marshaling commandGraphSpec omits this empty slice
	// and would miss the distinct representation a model can submit.
	raw := json.RawMessage(`{"key":"original","nodes":[{"id":"download","command":"printf 'once\\n' >> \"$PWNMESH_WORKSPACE/effects\"; printf '{\"version\":1}\\n' > result.json; printf downloaded","depends_on":[],"resources":[],"artifacts":["result.json"]}]}`)
	if _, err := runCommandGraph(context.Background(), j, Options{RunDir: dir}, raw); err != nil {
		t.Fatalf("source with explicit empty dependencies failed: %v", err)
	}
	spec := commandGraphSpec{Key: "repair", Nodes: []commandGraphNode{{ID: "downloaded", Kind: "reuse", ReuseFrom: &commandGraphReuse{Key: "original", Node: "download"}, Resources: []string{}}}}
	checkpoint, err := commandGraphCall(t, context.Background(), j, dir, spec)
	if err != nil || checkpoint.Status != "succeeded" {
		t.Fatalf("explicit empty dependencies broke source definition lookup: %+v %v", checkpoint, err)
	}
	effects, err := os.ReadFile(filepath.Join(j.Workspace, "effects"))
	if err != nil || string(effects) != "once\n" {
		t.Fatalf("reuse reexecuted its source: %q %v", effects, err)
	}
}

func TestCommandGraphReusePreflightRejectsChangesBeforeAnyExecution(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, string, *workergraph.Checkpoint)
	}{
		{"artifact", func(t *testing.T, dir string, _ *workergraph.Checkpoint) {
			if err := os.WriteFile(filepath.Join(dir, "nodes", "download", "result.json"), []byte(`{"version":2}`), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing-artifact", func(t *testing.T, dir string, _ *workergraph.Checkpoint) {
			if err := os.Remove(filepath.Join(dir, "nodes", "download", "result.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"stdout", func(t *testing.T, dir string, _ *workergraph.Checkpoint) {
			if err := os.WriteFile(filepath.Join(dir, "nodes", "download", "stdout.log"), []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"definition", func(t *testing.T, dir string, c *workergraph.Checkpoint) {
			path := filepath.Join(dir, "definitions", c.Nodes[0].DefinitionSHA256+".json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, bytes.Replace(raw, []byte("result.json"), []byte("other.json"), 1), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing-definition", func(t *testing.T, dir string, c *workergraph.Checkpoint) {
			if err := os.Remove(filepath.Join(dir, "definitions", c.Nodes[0].DefinitionSHA256+".json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"cross-run", func(_ *testing.T, _ string, c *workergraph.Checkpoint) { c.RunID = "different-run" }},
		{"input-binding", func(_ *testing.T, _ string, c *workergraph.Checkpoint) {
			c.Nodes[0].InputSHA256 = strings.Repeat("0", 64)
		}},
		{"unresolved", func(_ *testing.T, _ string, c *workergraph.Checkpoint) { c.Nodes[0].Status = "running" }},
		{"pending", func(_ *testing.T, _ string, c *workergraph.Checkpoint) { c.Nodes[0].Status = "pending" }},
		{"interrupted", func(_ *testing.T, _ string, c *workergraph.Checkpoint) { c.Status = "interrupted" }},
		{"artifact-list", func(_ *testing.T, _ string, c *workergraph.Checkpoint) {
			c.Nodes[0].Output.Artifacts = c.Nodes[0].Output.Artifacts[:1]
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j, dir, source := reuseSource(t, false)
			sourceDir := filepath.Join(dir, "graph-tools", "original")
			tc.mutate(t, sourceDir, &source)
			raw, err := json.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sourceDir, "graph.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			spec := reuseGraph()
			spec.Nodes = append(spec.Nodes, commandGraphNode{ID: "aaa-effect", Command: `printf unexpected > "$PWNMESH_WORKSPACE/new-effect"`, Resources: []string{}, Artifacts: []string{}})
			if _, err := commandGraphCall(t, context.Background(), j, dir, spec); err == nil {
				t.Fatal("unsafe source accepted")
			}
			if _, err := os.Stat(filepath.Join(j.Workspace, "new-effect")); !os.IsNotExist(err) {
				t.Fatalf("new work started before failed preflight: %v", err)
			}
		})
	}
}

func TestCommandGraphReuseRejectsSourceLockAliasesAndFailedNode(t *testing.T) {
	for _, mode := range []string{"lock", "lock-symlink", "missing-lock", "directory-symlink", "checkpoint-symlink", "artifact-symlink", "failed-node", "same-key", "traversal", "missing-node"} {
		t.Run(mode, func(t *testing.T) {
			j, dir, _ := reuseSource(t, mode == "failed-node")
			sourceDir := filepath.Join(dir, "graph-tools", "original")
			spec := reuseGraph()
			switch mode {
			case "lock":
				unlock, err := process.Lock(sourceDir)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			case "lock-symlink":
				path := filepath.Join(sourceDir, "worker.lock")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(j.Workspace, "new-effect"), path); err != nil {
					t.Fatal(err)
				}
			case "missing-lock":
				if err := os.Remove(filepath.Join(sourceDir, "worker.lock")); err != nil {
					t.Fatal(err)
				}
			case "directory-symlink":
				realDir := sourceDir + "-moved"
				if err := os.Rename(sourceDir, realDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(realDir, sourceDir); err != nil {
					t.Fatal(err)
				}
			case "checkpoint-symlink", "artifact-symlink":
				path := filepath.Join(sourceDir, "graph.json")
				if mode == "artifact-symlink" {
					path = filepath.Join(sourceDir, "nodes", "download", "result.json")
				}
				if err := os.Rename(path, path+"-moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-moved", path); err != nil {
					t.Fatal(err)
				}
			case "failed-node":
				spec.Nodes[0].ReuseFrom.Node = "analysis"
			case "same-key":
				spec.Key = "original"
			case "traversal":
				spec.Nodes[0].ReuseFrom.Key = "../original"
			case "missing-node":
				spec.Nodes[0].ReuseFrom.Node = "absent"
			}
			raw, _ := json.Marshal(spec)
			if _, err := runCommandGraph(context.Background(), j, Options{RunDir: dir}, raw); err == nil {
				t.Fatal("invalid reuse accepted")
			}
			if _, err := os.Stat(filepath.Join(j.Workspace, "new-effect")); !os.IsNotExist(err) {
				t.Fatalf("invalid source lock created an external file: %v", err)
			}
		})
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
	source := commandGraphSpec{Key: "original", Nodes: []commandGraphNode{{ID: "interpret", Kind: "agent", Task: "Interpret", Resources: []string{}, Artifacts: []string{}}}}
	if _, err := mixedGraphCall(t, context.Background(), j, opts, source); err != nil {
		t.Fatal(err)
	}
	spec := commandGraphSpec{Key: "repair", Nodes: []commandGraphNode{{ID: "reused", Kind: "reuse", ReuseFrom: &commandGraphReuse{Key: "original", Node: "interpret"}, Resources: []string{}}}}
	raw, _ := json.Marshal(spec)
	response, err := runCommandGraph(context.Background(), j, opts, raw)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Nodes    []workergraph.NodeState `json:"nodes"`
		Evidence []commandGraphEvidence  `json:"verified_evidence"`
	}
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(result.Nodes) != 1 || result.Nodes[0].Kind != "agent" || len(result.Evidence) != 0 {
		t.Fatalf("reuse reran model or promoted interpretation to evidence: calls=%d result=%+v", calls, result)
	}
}

func TestCommandGraphReuseRejectsRecursiveImportsAndChangedProvenance(t *testing.T) {
	j, dir, _ := reuseSource(t, false)
	if _, err := commandGraphCall(t, context.Background(), j, dir, reuseGraph()); err != nil {
		t.Fatal(err)
	}
	spec := commandGraphSpec{Key: "third", Nodes: []commandGraphNode{{ID: "import", Kind: "reuse", ReuseFrom: &commandGraphReuse{Key: "repair", Node: "downloaded"}, Resources: []string{}}}}
	if _, err := commandGraphCall(t, context.Background(), j, dir, spec); err == nil {
		t.Fatal("recursive reuse accepted")
	}
	imports, cleanup, err := prepareCommandReuses(context.Background(), j, Options{RunDir: dir}, reuseGraph())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	imported := imports["downloaded"]
	output := imported.Output()
	var value commandGraphOutput
	if err := json.Unmarshal(output.Value, &value); err != nil {
		t.Fatal(err)
	}
	value.ReusedFrom.Node = "other"
	output.Value, _ = json.Marshal(value)
	if err := imported.Verify(context.Background(), output); err == nil {
		t.Fatal("changed provenance accepted")
	}
}
