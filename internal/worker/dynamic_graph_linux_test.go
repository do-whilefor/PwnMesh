//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/workergraph"
)

func dynamicToolReply(history []agent.Message, id string) (string, error) {
	for i := len(history) - 1; i >= 0; i-- {
		for _, block := range history[i].Content {
			if block.Type != "tool_result" || block.ToolUseID != id {
				continue
			}
			var text string
			if err := json.Unmarshal(block.Content, &text); err != nil {
				return "", err
			}
			if block.IsError {
				return "", fmt.Errorf("tool %s failed: %s", id, text)
			}
			return text, nil
		}
	}
	return "", fmt.Errorf("missing tool result %s", id)
}

func TestDynamicGraphParentLoopDelegatesFromObservedResults(t *testing.T) {
	type assignment struct {
		Role  string `json:"role"`
		Input string `json:"input"`
	}
	for _, input := range [][]assignment{
		{{"parser", "nested-expression"}, {"storage", "retry-write"}, {"routing", "cross-region"}},
		{{"latency", "cold-start"}, {"memory", "large-stream"}, {"cancellation", "abandoned-request"}, {"recovery", "interrupted-run"}, {"isolation", "shared-file"}},
	} {
		t.Run(fmt.Sprintf("%d discovered roles", len(input)), func(t *testing.T) {
			job, dir := graphWrapperJob(t), t.TempDir()
			rawInput, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(job.Workspace, "work-items.json"), rawInput, 0600); err != nil {
				t.Fatal(err)
			}
			// Only discovery is known initially. The parent adds both specialists
			// and their eventual integration task after observing prior results.
			spec := commandGraphSpec{Key: "adaptive", Nodes: []commandGraphNode{{
				ID: "z-discover", Kind: "agent", Task: "Discover work items from work-items.json", Resources: []string{},
			}}}
			assignments := map[string]string{"z-discover": spec.Nodes[0].Task}
			reports := map[string]string{}
			var callsMu sync.Mutex
			calls, sessions := map[string]int{}, map[string]int{}
			var output bytes.Buffer
			opts := Options{RunDir: dir, Output: &output, graphProvider: func(id string) (agent.Provider, error) {
				callsMu.Lock()
				sessions[id]++
				callsMu.Unlock()
				task, known := assignments[id]
				if !known {
					return nil, fmt.Errorf("started undelegated child %s", id)
				}
				turn := 0
				return scenarioProvider(func(_ context.Context, history []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
					turn++
					callsMu.Lock()
					calls[id]++
					callsMu.Unlock()
					for _, def := range defs {
						if slices.Contains([]string{"run_graph", "graph_action", "finish_step", "read_graph"}, def.Name) {
							return agent.Message{}, fmt.Errorf("child %s received parent capability %s", id, def.Name)
						}
					}
					if len(history) == 0 || !strings.Contains(history[0].Text(), "<task>\n"+task+"\n</task>") {
						return agent.Message{}, fmt.Errorf("child %s lost its data-dependent task", id)
					}
					if turn == 1 {
						if len(history) != 1 {
							return agent.Message{}, fmt.Errorf("child %s inherited another Loop history", id)
						}
						command := `cat "$PWNMESH_WORKSPACE/work-items.json"`
						if id != "z-discover" {
							command = "printf '%s' '" + strings.ReplaceAll(reports[id], "'", "'\\''") + "' > evidence.txt; cat evidence.txt"
						}
						args, _ := json.Marshal(map[string]string{"command": command})
						return draftModelCall(id+"-observe", "bash", string(args)), nil
					}
					if turn != 2 {
						return agent.Message{}, fmt.Errorf("child %s unexpectedly replayed turn %d", id, turn)
					}
					observed, err := dynamicToolReply(history, id+"-observe")
					if err != nil {
						return agent.Message{}, err
					}
					// Every child finishes from its own actual tool result, including
					// discovery, whose result determines the number of new Agents.
					return agent.Text("assistant", observed), nil
				}), nil
			}}
			turns := 0
			var finalCheckpoint workergraph.Checkpoint
			opts.Provider = scenarioProvider(func(_ context.Context, history []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
				turns++
				if !slices.ContainsFunc(defs, func(def agent.Definition) bool { return def.Name == "run_graph" }) {
					return agent.Message{}, fmt.Errorf("parent Loop lost delegation tool")
				}
				var checkpoint workergraph.Checkpoint
				if turns > 1 {
					reply, err := dynamicToolReply(history, fmt.Sprintf("delegate-%d", turns-1))
					if err != nil {
						return agent.Message{}, err
					}
					if err := json.Unmarshal([]byte(reply), &checkpoint); err != nil {
						return agent.Message{}, err
					}
					if checkpoint.Status != "succeeded" {
						return agent.Message{}, fmt.Errorf("delegation did not complete: %+v", checkpoint)
					}
				}
				switch turns {
				case 1:
				case 2:
					_, result := commandNodeValue(t, checkpoint, "z-discover")
					var discovered []assignment
					if err := json.Unmarshal([]byte(result.Stdout), &discovered); err != nil {
						return agent.Message{}, fmt.Errorf("invalid discovered work: %w", err)
					}
					for _, item := range discovered {
						id := "specialist-" + item.Role
						task := "Assess " + item.Role + " using discovered input: " + item.Input
						assignments[id] = task
						report, _ := json.Marshal(assignment{Role: item.Role, Input: "observed " + item.Input})
						reports[id] = string(report)
						spec.Nodes = append(spec.Nodes, commandGraphNode{ID: id, Kind: "agent", Task: task, Resources: []string{}, Artifacts: []string{"evidence.txt"}, DependsOn: []workergraph.Dependency{{ID: "z-discover"}}})
					}
				case 3:
					var findings []assignment
					var dependencies []workergraph.Dependency
					for _, node := range checkpoint.Nodes {
						if node.ID == "z-discover" {
							continue
						}
						_, result := commandNodeValue(t, checkpoint, node.ID)
						var finding assignment
						if err := json.Unmarshal([]byte(result.Stdout), &finding); err != nil {
							return agent.Message{}, err
						}
						findings = append(findings, finding)
						dependencies = append(dependencies, workergraph.Dependency{ID: node.ID})
					}
					merged, _ := json.Marshal(findings)
					// This newly appended ID sorts before every completed node.
					assignments["a-integrate"] = "Integrate observed findings: " + string(merged)
					reports["a-integrate"] = string(merged)
					spec.Nodes = append(spec.Nodes, commandGraphNode{ID: "a-integrate", Kind: "agent", Task: assignments["a-integrate"], Resources: []string{}, Artifacts: []string{"evidence.txt"}, DependsOn: dependencies})
				case 4:
					// Retry the full cumulative definition: no child may restart.
				case 5:
					finalCheckpoint = checkpoint
					return agent.Text("assistant", completedOutput("explore")), nil
				default:
					return agent.Message{}, fmt.Errorf("parent unexpectedly needed turn %d", turns)
				}
				raw, err := json.Marshal(spec)
				if err != nil {
					return agent.Message{}, err
				}
				return draftModelCall(fmt.Sprintf("delegate-%d", turns), "run_graph", string(raw)), nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			result, err := runTestWorker(ctx, job, opts)
			if err != nil || result.Status != "success" || turns != 5 {
				t.Fatalf("dynamic parent Loop failed: %+v %v turns=%d", result, err, turns)
			}
			if got := graphOutputResults(t, output.Bytes()); len(got) != 1 {
				t.Fatalf("children published top-level results: %+v", got)
			}
			if len(finalCheckpoint.Nodes) != len(input)+2 || len(sessions) != len(input)+2 {
				t.Fatalf("delegation ignored discovered work: nodes=%d sessions=%v", len(finalCheckpoint.Nodes), sessions)
			}
			_, merged := commandNodeValue(t, finalCheckpoint, "a-integrate")
			for _, item := range input {
				if !strings.Contains(merged.Stdout, "observed "+item.Input) || !strings.Contains(merged.Stdout, item.Role) {
					t.Fatalf("integration lost observed role/input %+v: %s", item, merged.Stdout)
				}
			}
			rawDependencies, err := os.ReadFile(filepath.Join(dir, "graph-tools", spec.Key, "nodes", "a-integrate", "dependencies.json"))
			var dependencies []workergraph.NodeState
			if err != nil || json.Unmarshal(rawDependencies, &dependencies) != nil || len(dependencies) != len(input) {
				t.Fatalf("integration did not receive all completed specialists: %s %v", rawDependencies, err)
			}
			for _, dependency := range dependencies {
				completed, _ := commandNodeValue(t, finalCheckpoint, dependency.ID)
				if dependency.Status != "succeeded" || !bytes.Equal(dependency.Output.Value, completed.Output.Value) {
					t.Errorf("newly sorted integration node received wrong dependency state: %+v", dependency)
				}
			}
			for id, task := range assignments {
				if sessions[id] != 1 || calls[id] != 2 {
					t.Errorf("child %s did not execute one independent Loop: sessions=%d calls=%d", id, sessions[id], calls[id])
				}
				raw, err := os.ReadFile(filepath.Join(dir, "graph-tools", spec.Key, "nodes", id, "session.json"))
				var state graphAgentSession
				if err != nil || json.Unmarshal(raw, &state) != nil || state.NodeID != id || state.RunID != job.RunID || len(state.History) < 4 || !strings.Contains(state.History[0].Text(), task) || state.Result == "" {
					t.Errorf("child %s lacks its own durable Loop history: %s %v", id, raw, err)
				}
			}
		})
	}
}

func TestDynamicGraphRejectsUnsafeExtensionBeforeStartingNewAgent(t *testing.T) {
	for _, variant := range []string{"remove old node", "change old command", "change old artifacts", "unordered shared resource", "changed old artifact"} {
		t.Run(variant, func(t *testing.T) {
			job, dir := graphWrapperJob(t), t.TempDir()
			var starts atomic.Int32
			opts := Options{RunDir: dir, graphProvider: func(string) (agent.Provider, error) {
				starts.Add(1)
				return scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
					return agent.Text("assistant", "reviewed old observation"), nil
				}), nil
			}}
			original := commandGraphNode{ID: "z-observed", Command: "printf original > evidence.txt; printf observed", Resources: []string{"shared-report"}, Artifacts: []string{"evidence.txt"}}
			spec := commandGraphSpec{Key: "extend", Nodes: []commandGraphNode{original}}
			checkpoint, err := mixedGraphCall(t, context.Background(), job, opts, spec)
			if err != nil || checkpoint.Status != "succeeded" {
				t.Fatalf("initial observation failed: %+v %v", checkpoint, err)
			}
			next := commandGraphNode{ID: "a-review", Kind: "agent", Task: "review observation", Resources: []string{"shared-report"}, DependsOn: []workergraph.Dependency{{ID: original.ID}}}
			spec.Nodes = append(spec.Nodes, next)
			switch variant {
			case "remove old node":
				spec.Nodes = []commandGraphNode{next}
				spec.Nodes[0].DependsOn = nil
			case "change old command":
				spec.Nodes[0].Command = "printf replacement"
			case "change old artifacts":
				spec.Nodes[0].Artifacts = nil
			case "unordered shared resource":
				spec.Nodes[1].DependsOn = nil
			case "changed old artifact":
				if err := os.WriteFile(filepath.Join(dir, "graph-tools", spec.Key, "nodes", original.ID, "evidence.txt"), []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := mixedGraphCall(t, context.Background(), job, opts, spec); err == nil || starts.Load() != 0 {
				t.Fatalf("unsafe extension started new work: err=%v starts=%d", err, starts.Load())
			}
			if variant == "changed old artifact" {
				return
			}
			// Rejected definitions must not consume the key or poison its history.
			spec.Nodes = []commandGraphNode{original, next}
			checkpoint, err = mixedGraphCall(t, context.Background(), job, opts, spec)
			if err != nil || checkpoint.Status != "succeeded" || starts.Load() != 1 {
				t.Fatalf("corrected extension failed: %+v %v starts=%d", checkpoint, err, starts.Load())
			}
		})
	}
}
