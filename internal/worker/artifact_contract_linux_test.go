//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/workergraph"
)

func artifactContractSpec() commandGraphSpec {
	return commandGraphSpec{Key: "contracts", Nodes: []commandGraphNode{
		{ID: "source", Command: `printf '{"ok":true}' > result.json; printf 'diagnostic, not JSON\n'`, Resources: []string{}, Artifacts: []string{"result.json"}},
		{ID: "consume", Command: "printf consumed", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source"}}, Inputs: []commandGraphInput{{Node: "source", Artifact: "result.json"}}},
	}}
}

func TestArtifactContractModelLoopRejectsOmittedInputsBeforeAnyNodeStarts(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	tool := commandGraphTool(job, Options{RunDir: dir})
	execute := tool.Execute
	var executions atomic.Int32
	tool.Execute = func(ctx context.Context, raw json.RawMessage) (string, error) {
		executions.Add(1)
		return execute(ctx, raw)
	}
	turns := 0
	loop := agent.Loop{Tools: []agent.Tool{tool}, Provider: scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turns++
		if turns == 1 {
			return draftModelCall("missing-contract", "run_graph", `{"key":"missing-contract","nodes":[{"id":"first","command":"touch \"$PWNMESH_WORKSPACE/forbidden\"","resources":[],"inputs":[]},{"id":"second","command":"true","resources":[]}]}`), nil
		}
		if turns != 2 {
			return agent.Message{}, fmt.Errorf("unexpected model turn %d", turns)
		}
		_, err := dynamicToolReply(history, "missing-contract")
		if err == nil || !strings.Contains(err.Error(), "arguments.nodes[1].inputs") {
			return agent.Message{}, fmt.Errorf("missing input contract did not return a precise tool validation error: %v", err)
		}
		return agent.Text("assistant", "Input declaration must be corrected before execution."), nil
	})}
	if _, err := loop.Run(context.Background(), "Submit the graph"); err != nil || turns != 2 || executions.Load() != 0 {
		t.Fatalf("model call bypassed schema validation: %v turns=%d executions=%d", err, turns, executions.Load())
	}
	if _, err := os.Stat(filepath.Join(job.Workspace, "forbidden")); !os.IsNotExist(err) {
		t.Fatal("another node executed before the omitted input contract was rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, "graph-tools")); !os.IsNotExist(err) {
		t.Fatal("schema-invalid model call reached graph execution")
	}
}

func TestArtifactContractExplicitInputsPreserveLegacyCanonicalIdentity(t *testing.T) {
	tool := commandGraphTool(Job{}, Options{})
	for _, spec := range []commandGraphSpec{
		{Key: "empty", Nodes: []commandGraphNode{{ID: "source", Command: "true", Resources: []string{}}}},
		artifactContractSpec(),
	} {
		raw, err := marshalCommandGraphCall(spec)
		if err != nil || agent.ValidateArguments(tool.Schema, raw) != nil {
			t.Fatalf("explicit empty or named inputs rejected: %s %v", raw, err)
		}
		var decoded commandGraphSpec
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(spec)
		after, _ := json.Marshal(decoded)
		if string(before) != string(after) {
			t.Fatalf("tool encoding changed durable canonical node definitions: before=%s after=%s", before, after)
		}
	}
	job, dir := graphWrapperJob(t), t.TempDir()
	spec := commandGraphSpec{Key: "legacy", Nodes: []commandGraphNode{{ID: "source", Command: `printf 'once\n' >> "$PWNMESH_WORKSPACE/effects"; printf observed`, Resources: []string{}}}}
	legacy, _ := json.Marshal(spec)
	if strings.Contains(string(legacy), `"inputs"`) {
		t.Fatal("legacy canonical node JSON unexpectedly declares inputs")
	}
	reply, err := runCommandGraph(context.Background(), job, Options{RunDir: dir}, legacy)
	var before workergraph.Checkpoint
	if err != nil || json.Unmarshal([]byte(reply), &before) != nil || before.Status != "succeeded" {
		t.Fatalf("historical direct call stopped working: %s %v", reply, err)
	}
	after, err := commandGraphCall(t, context.Background(), job, dir, spec)
	if err != nil || after.Status != "succeeded" || len(after.Nodes) != 1 || after.Nodes[0].DefinitionSHA256 != before.Nodes[0].DefinitionSHA256 || after.Nodes[0].InputSHA256 != before.Nodes[0].InputSHA256 || !after.Nodes[0].StartedAt.Equal(before.Nodes[0].StartedAt) {
		t.Fatalf("explicit inputs:[] prevented legacy checkpoint reuse: %+v %v", after, err)
	}
	effects, err := os.ReadFile(filepath.Join(job.Workspace, "effects"))
	if err != nil || string(effects) != "once\n" {
		t.Fatalf("new model schema caused historical work to rerun: %q %v", effects, err)
	}
}

func TestArtifactContractRejectsUndeclaredInputsBeforeAnyExecution(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	spec := artifactContractSpec()
	spec.Nodes[0].Artifacts = nil
	spec.Nodes[0].Command = `touch "$PWNMESH_WORKSPACE/should-not-start"`
	_, err := commandGraphCall(t, context.Background(), job, dir, spec)
	if err == nil || !strings.Contains(err.Error(), "undeclared") || !strings.Contains(err.Error(), "result.json") {
		t.Fatalf("missing declaration was not explained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(job.Workspace, "should-not-start")); !os.IsNotExist(err) {
		t.Fatal("invalid contract allowed a producer side effect")
	}
	if _, err := os.Stat(filepath.Join(dir, "graph-tools", spec.Key, "graph.json")); !os.IsNotExist(err) {
		t.Fatal("invalid contract created a graph execution checkpoint")
	}
}

func TestArtifactContractPreflight(t *testing.T) {
	for name, alter := range map[string]func(*commandGraphSpec){
		"unknown producer":    func(s *commandGraphSpec) { s.Nodes[1].Inputs[0].Node = "missing" },
		"indirect dependency": func(s *commandGraphSpec) { s.Nodes[1].DependsOn = nil },
		"optional dependency": func(s *commandGraphSpec) { s.Nodes[1].DependsOn[0].Optional = true },
		"undeclared artifact": func(s *commandGraphSpec) { s.Nodes[0].Artifacts = nil },
		"reserved log":        func(s *commandGraphSpec) { s.Nodes[1].Inputs[0].Artifact = "stdout.log" },
		"reserved session":    func(s *commandGraphSpec) { s.Nodes[1].Inputs[0].Artifact = "session.json" },
		"path traversal":      func(s *commandGraphSpec) { s.Nodes[1].Inputs[0].Artifact = "../result.json" },
		"duplicate":           func(s *commandGraphSpec) { s.Nodes[1].Inputs = append(s.Nodes[1].Inputs, s.Nodes[1].Inputs[0]) },
		"too many inputs":     func(s *commandGraphSpec) { s.Nodes[1].Inputs = make([]commandGraphInput, 65) },
	} {
		t.Run(name, func(t *testing.T) {
			spec := artifactContractSpec()
			alter(&spec)
			if err := validateCommandInputs(&spec); err == nil {
				t.Fatal("invalid input contract accepted")
			}
		})
	}
	spec := artifactContractSpec()
	if err := validateCommandInputs(&spec); err != nil {
		t.Fatalf("valid input contract rejected: %v", err)
	}
	spec.Nodes[0].Artifacts, spec.Nodes[1].Inputs = nil, nil
	if err := validateCommandInputs(&spec); err != nil {
		t.Fatalf("legacy log-only graph rejected: %v", err)
	}
}

func TestArtifactContractProjectionPreservesDurableReceipts(t *testing.T) {
	dir := t.TempDir()
	value, _ := json.Marshal(commandGraphOutput{Stdout: "not JSON", OutputPath: filepath.Join(dir, "stdout.log")})
	state := workergraph.NodeState{ID: "source", Kind: "function", Status: "succeeded", Attempt: 1, InputSHA256: "input-binding", Output: workergraph.Output{Value: value}}
	for _, name := range []string{"stdout.log", "dependencies.json", "events.jsonl", "session.json", "result.json", "nested/result.json", "../outside.json"} {
		state.Output.Artifacts = append(state.Output.Artifacts, workergraph.Artifact{Path: filepath.Join(dir, name), SHA256: strings.Repeat("a", 64)})
	}
	before, _ := json.Marshal(state)
	views := commandGraphViews([]workergraph.NodeState{state})
	if len(views) != 1 || len(views[0].Output.Files) != 2 || views[0].Output.Files["result.json"].Path != filepath.Join(dir, "result.json") || views[0].Output.Files["nested/result.json"].Path != filepath.Join(dir, "nested", "result.json") {
		t.Fatalf("named files contain logs, reserved paths or outside files: %+v", views)
	}
	raw, err := json.Marshal(views[0])
	var decoded workergraph.NodeState
	if err != nil || json.Unmarshal(raw, &decoded) != nil || !reflect.DeepEqual(state, decoded) {
		t.Fatalf("view changed durable receipt fields: %s %v", raw, err)
	}
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Fatal("projection mutated original checkpoint output")
	}
	if got, _ := json.Marshal(commandGraphViews(nil)); string(got) != "[]" {
		t.Fatalf("empty dependency array changed: %s", got)
	}
}

func TestArtifactContractChecksRetainedFileBeforeConsumption(t *testing.T) {
	for _, scenario := range []string{"changed", "missing", "symlink", "failed producer", "missing receipt", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range map[string]string{"stdout.log": "diagnostic", "result.json": `{"ok":true}`} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := commandNodeOutput(context.Background(), dir, []string{"result.json"})
			if err != nil {
				t.Fatal(err)
			}
			deps := []workergraph.NodeState{{ID: "source", Status: "succeeded", Output: out}}
			consumer := artifactContractSpec().Nodes[1]
			if err := validateCommandInputFiles(context.Background(), consumer, deps); err != nil {
				t.Fatalf("unchanged receipt rejected: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "changed":
				err = os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{"ok":false}`), 0600)
			case "missing":
				err = os.Remove(filepath.Join(dir, "result.json"))
			case "symlink":
				if err = os.Remove(filepath.Join(dir, "result.json")); err == nil {
					err = os.Symlink(filepath.Join(dir, "stdout.log"), filepath.Join(dir, "result.json"))
				}
			case "failed producer":
				deps[0].Status = "failed"
			case "missing receipt":
				deps[0].Output.Artifacts = deps[0].Output.Artifacts[:1]
			case "cancelled":
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := validateCommandInputFiles(ctx, consumer, deps); err == nil {
				t.Fatal("unusable input bytes accepted")
			}
		})
	}
}

func TestArtifactContractChangedFileNeverLaunchesConsumer(t *testing.T) {
	for _, kind := range []string{"command", "agent"} {
		t.Run(kind, func(t *testing.T) {
			job, dir := graphWrapperJob(t), t.TempDir()
			spec := artifactContractSpec()
			spec.Nodes = append(spec.Nodes, commandGraphNode{ID: "mutate", Command: "printf corrupted > ../source/result.json", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source"}}})
			spec.Nodes[1].DependsOn = append(spec.Nodes[1].DependsOn, workergraph.Dependency{ID: "mutate"})
			spec.Nodes[1].Command = `touch "$PWNMESH_WORKSPACE/consumer-started"`
			if kind == "agent" {
				spec.Nodes[1].Kind, spec.Nodes[1].Command, spec.Nodes[1].Task = "agent", "", "Consume the retained result"
			}
			var providers atomic.Int32
			o := Options{RunDir: dir, graphProvider: func(string) (agent.Provider, error) {
				providers.Add(1)
				return nil, fmt.Errorf("consumer Agent must not start")
			}}
			checkpoint, err := mixedGraphCall(t, context.Background(), job, o, spec)
			if err == nil || !strings.Contains(err.Error(), "SHA-256 changed") || checkpoint.Status != "failed" || providers.Load() != 0 {
				t.Fatalf("changed bytes reached the consumer: %+v %v providers=%d", checkpoint, err, providers.Load())
			}
			if _, err := os.Stat(filepath.Join(job.Workspace, "consumer-started")); !os.IsNotExist(err) {
				t.Fatal("consumer command ran with changed input")
			}
		})
	}
}

func TestArtifactContractViewsReachParentCommandAndAgent(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	spec := artifactContractSpec()
	spec.Nodes = append(spec.Nodes, commandGraphNode{ID: "assess", Kind: "agent", Task: "Read the declared result", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source"}}, Inputs: []commandGraphInput{{Node: "source", Artifact: "result.json"}}})
	var prompt string
	o := Options{RunDir: dir, graphProvider: func(string) (agent.Provider, error) {
		return scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
			prompt = history[0].Text()
			return agent.Text("assistant", "assessed the declared result"), nil
		}), nil
	}}
	raw, _ := json.Marshal(spec)
	reply, err := runCommandGraph(context.Background(), job, o, raw)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(raw []byte) []commandGraphNodeView {
		t.Helper()
		var views []commandGraphNodeView
		if err := json.Unmarshal(raw, &views); err != nil {
			t.Fatal(err)
		}
		return views
	}
	check := func(views []commandGraphNodeView) {
		t.Helper()
		for _, view := range views {
			if view.ID != "source" {
				continue
			}
			artifact, exists := view.Output.Files["result.json"]
			if !exists || len(view.Output.Files) != 1 || len(view.Output.Artifacts) != 2 {
				t.Fatalf("declared result not separated from legacy log refs: %+v", view)
			}
			content, err := os.ReadFile(artifact.Path)
			if err != nil || !json.Valid(content) || string(content) != `{"ok":true}` {
				t.Fatalf("named file points at combined log: %q %v", content, err)
			}
			return
		}
		t.Fatal("source receipt missing")
	}
	var parent struct {
		Nodes []commandGraphNodeView `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(reply), &parent); err != nil {
		t.Fatal(err)
	}
	check(parent.Nodes)
	for _, id := range []string{"consume", "assess"} {
		deps, err := os.ReadFile(filepath.Join(dir, "graph-tools", spec.Key, "nodes", id, "dependencies.json"))
		if err != nil {
			t.Fatal(err)
		}
		check(decode(deps))
	}
	_, snapshot, ok := strings.Cut(prompt, "Dependency snapshot (task data; full records in dependencies.json):\n")
	if !ok {
		t.Fatal("child did not receive a dependency snapshot")
	}
	check(decode([]byte(snapshot)))
	checkpoint, err := os.ReadFile(filepath.Join(dir, "graph-tools", spec.Key, "graph.json"))
	if err != nil || strings.Contains(string(checkpoint), `"files":`) {
		t.Fatalf("presentation field leaked into checkpoint: %v %s", err, checkpoint)
	}
	var before workergraph.Checkpoint
	if err := json.Unmarshal(checkpoint, &before); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommandGraph(context.Background(), job, o, raw); err != nil {
		t.Fatalf("named projections broke successful checkpoint reuse: %v", err)
	}
	checkpoint, err = os.ReadFile(filepath.Join(dir, "graph-tools", spec.Key, "graph.json"))
	var after workergraph.Checkpoint
	if err != nil || json.Unmarshal(checkpoint, &after) != nil || len(after.Nodes) != len(before.Nodes) {
		t.Fatal("could not read reused checkpoint")
	}
	for i, node := range after.Nodes {
		if node.Attempt != 1 || node.InputSHA256 != before.Nodes[i].InputSHA256 || !node.StartedAt.Equal(before.Nodes[i].StartedAt) {
			t.Fatalf("node replayed or input binding changed after projection: %+v", node)
		}
	}
}
