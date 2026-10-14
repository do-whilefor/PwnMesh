//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/worker/dag"
	"pwnmesh/internal/workergraph"
)

func artifactContractSpec() dag.Spec {
	return dag.Spec{Key: "contracts", Nodes: []dag.Node{
		{ID: "source", Command: `printf '{"ok":true}' > result.json; printf 'diagnostic, not JSON\n'`, Resources: []string{}, Artifacts: []string{"result.json"}},
		{ID: "consume", Command: "printf consumed", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source"}}, Inputs: []dag.Input{{Node: "source", Artifact: "result.json"}}},
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
	for _, spec := range []dag.Spec{
		{Key: "empty", Nodes: []dag.Node{{ID: "source", Command: "true", Resources: []string{}}}},
		artifactContractSpec(),
	} {
		raw, err := marshalCommandGraphCall(spec)
		if err != nil || agent.ValidateArguments(tool.Schema, raw) != nil {
			t.Fatalf("explicit empty or named inputs rejected: %s %v", raw, err)
		}
		var decoded dag.Spec
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
	spec := dag.Spec{Key: "legacy", Nodes: []dag.Node{{ID: "source", Command: `printf 'once\n' >> "$PWNMESH_WORKSPACE/effects"; printf observed`, Resources: []string{}}}}
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

func TestArtifactContractConcurrentProducerMutationCannotChangeConsumerInput(t *testing.T) {
	for _, kind := range []string{"command", "agent"} {
		t.Run(kind, func(t *testing.T) {
			job, dir := graphWrapperJob(t), t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			spec := artifactContractSpec()
			spec.Parallelism = 2
			// The writer waits until the consumer is running, hence after its
			// input check/copy. This reproduces the old check-then-read window.
			spec.Nodes = append(spec.Nodes, dag.Node{ID: "mutate", Command: `while [ ! -f "$PWNMESH_WORKSPACE/consumer-started" ]; do sleep 0.01; done; printf '{"ok":false}' > ../source/result.json; touch "$PWNMESH_WORKSPACE/producer-mutated"`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source"}}})
			spec.Nodes[1].Command = `touch "$PWNMESH_WORKSPACE/consumer-started"; while [ ! -f "$PWNMESH_WORKSPACE/producer-mutated" ]; do sleep 0.01; done; python3 -c 'import json, os; deps=json.load(open(os.environ["PWNMESH_DEPENDENCIES"])); item=next(d for d in deps if d["id"]=="source"); print(open(item["output"]["files"]["result.json"]["path"]).read(), end="")'`
			o := Options{RunDir: dir}
			if kind == "agent" {
				spec.Nodes[1].Kind, spec.Nodes[1].Command, spec.Nodes[1].Task = "agent", "", "Read the declared input"
				o.graphProvider = func(string) (agent.Provider, error) {
					return scenarioProvider(func(ctx context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
						if err := os.WriteFile(filepath.Join(job.Workspace, "consumer-started"), nil, 0600); err != nil {
							return agent.Message{}, err
						}
						for {
							if _, err := os.Stat(filepath.Join(job.Workspace, "producer-mutated")); err == nil {
								break
							}
							select {
							case <-ctx.Done():
								return agent.Message{}, ctx.Err()
							case <-time.After(time.Millisecond):
							}
						}
						_, snapshot, ok := strings.Cut(history[0].Text(), "Dependency snapshot (task data; full records in dependencies.json):\n")
						var views []dag.NodeView
						if !ok || json.Unmarshal([]byte(snapshot), &views) != nil || len(views) != 1 {
							return agent.Message{}, fmt.Errorf("missing dependency snapshot")
						}
						content, err := os.ReadFile(views[0].Output.Files["result.json"].Path)
						return agent.Text("assistant", string(content)), err
					}), nil
				}
			}
			checkpoint, err := mixedGraphCall(t, ctx, job, o, spec)
			if err != nil || checkpoint.Status != "succeeded" {
				t.Fatalf("graph failed: %+v %v", checkpoint, err)
			}
			_, result := commandNodeValue(t, checkpoint, "consume")
			if result.Stdout != `{"ok":true}` {
				t.Fatalf("consumer read post-check producer mutation: %q", result.Stdout)
			}
			changed, err := os.ReadFile(filepath.Join(dir, "graph-tools", spec.Key, "nodes", "source", "result.json"))
			if err != nil || string(changed) != `{"ok":false}` {
				t.Fatalf("concurrent writer did not exercise the race: %q %v", changed, err)
			}
		})
	}
}

func TestArtifactContractChangedFileNeverLaunchesConsumer(t *testing.T) {
	for _, kind := range []string{"command", "agent"} {
		t.Run(kind, func(t *testing.T) {
			job, dir := graphWrapperJob(t), t.TempDir()
			spec := artifactContractSpec()
			spec.Nodes = append(spec.Nodes, dag.Node{ID: "mutate", Command: "printf corrupted > ../source/result.json", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source"}}})
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
	spec.Nodes = append(spec.Nodes, dag.Node{ID: "assess", Kind: "agent", Task: "Read the declared result", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source"}}, Inputs: []dag.Input{{Node: "source", Artifact: "result.json"}}})
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
	decode := func(raw []byte) []dag.NodeView {
		t.Helper()
		var views []dag.NodeView
		if err := json.Unmarshal(raw, &views); err != nil {
			t.Fatal(err)
		}
		return views
	}
	check := func(views []dag.NodeView) {
		t.Helper()
		for _, view := range views {
			if view.ID != "source" {
				continue
			}
			artifact, exists := view.Output.Files["result.json"]
			if !exists || len(view.Output.Files) != 1 || len(view.Output.Artifacts) != 3 {
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
		Nodes []dag.NodeView `json:"nodes"`
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
