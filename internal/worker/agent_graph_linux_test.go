//go:build linux

package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/workergraph"
)

func mixedGraphCall(t *testing.T, ctx context.Context, j Job, o Options, spec commandGraphSpec) (workergraph.Checkpoint, error) {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := runCommandGraph(ctx, j, o, raw)
	var checkpoint workergraph.Checkpoint
	if result != "" {
		if err := json.Unmarshal([]byte(result), &checkpoint); err != nil {
			t.Fatal(err)
		}
	}
	return checkpoint, runErr
}

func TestMixedGraphOwnsIndependentLoopsAndParentRemainsOnlyPublisher(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	// Child shell processes must use the parent's launch/cancellation scope,
	// while their output files and Loop histories stay in node directories.
	token := strings.Repeat("a", 32)
	t.Setenv("PWNMESH_LAUNCH_TOKEN", token)
	if err := os.WriteFile(filepath.Join(dir, "launch-token"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	leftReady, rightReady := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	o := Options{RunDir: dir, Output: &output, graphProvider: func(id string) (agent.Provider, error) {
		turn := 0
		return scenarioProvider(func(ctx context.Context, history []agent.Message, defs []agent.Definition, emit agent.Emit) (agent.Message, error) {
			calls.Add(1)
			turn++
			for _, def := range defs {
				if def.Name == "run_graph" || def.Name == "graph_action" || def.Name == "finish_step" || def.Name == "read_graph" {
					t.Errorf("child capability escaped: %s", def.Name)
				}
			}
			if !strings.Contains(history[0].Text(), "task-"+id) || strings.Contains(history[0].Text(), "task-"+map[string]string{"left": "right", "right": "left"}[id]) {
				t.Errorf("child history was shared: %s", history[0].Text())
			}
			if turn == 1 {
				ready, peer := leftReady, rightReady
				if id == "right" {
					ready, peer = rightReady, leftReady
				}
				close(ready)
				select {
				case <-peer:
				case <-ctx.Done():
					return agent.Message{}, ctx.Err()
				}
				args, _ := json.Marshal(map[string]string{"command": `test "$PWD" = "$PWNMESH_NODE_DIR" && test -d "$PWNMESH_WORKSPACE" && test "$(cat "$PWNMESH_DEPENDENCIES")" = '[]' || exit 7; ` + "printf '" + id + " raw evidence\\n' > evidence.txt; cat evidence.txt"})
				return agent.Message{Role: "assistant", StopReason: "tool_use", Content: []agent.Block{{Type: "tool_use", ID: id + "-bash", Name: "bash", Input: args}}}, nil
			}
			return agent.Text("assistant", id+" observed; raw evidence: evidence.txt"), nil
		}), nil
	}}
	spec := commandGraphSpec{Key: "mixed", Parallelism: 2, Nodes: []commandGraphNode{
		{ID: "left", Kind: "agent", Task: "task-left", Resources: []string{}, Artifacts: []string{"evidence.txt"}},
		{ID: "right", Kind: "agent", Task: "task-right", Resources: []string{}, Artifacts: []string{"evidence.txt"}},
		{ID: "join", Command: `cat ../left/evidence.txt ../right/evidence.txt; cat "$PWNMESH_DEPENDENCIES"`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "left"}, {ID: "right"}}},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checkpoint, err := mixedGraphCall(t, ctx, j, o, spec)
	if err != nil || checkpoint.Status != "succeeded" || calls.Load() != 4 {
		t.Fatalf("mixed graph failed: %+v %v, calls=%d", checkpoint, err, calls.Load())
	}
	if output.Len() != 0 {
		t.Fatalf("child leaked a top-level event or result: %s", output.String())
	}
	_, joined := commandNodeValue(t, checkpoint, "join")
	if !strings.Contains(joined.Stdout, "left raw evidence") || !strings.Contains(joined.Stdout, "right raw evidence") {
		t.Fatalf("fan-in lost dependency outputs: %s", joined.Stdout)
	}
	for _, id := range []string{"left", "right"} {
		raw, err := os.ReadFile(filepath.Join(dir, "graph-tools", "mixed", "nodes", id, "session.json"))
		var state graphAgentSession
		if err != nil || json.Unmarshal(raw, &state) != nil || state.NodeID != id || state.RunID != j.RunID || len(state.History) < 4 || state.Checkpoint == nil || state.Result == "" {
			t.Fatalf("missing isolated Loop checkpoint for %s: %s %v", id, raw, err)
		}
	}
	again, err := mixedGraphCall(t, ctx, j, o, spec)
	if err != nil || again.Status != "succeeded" || calls.Load() != 4 {
		t.Fatalf("completed Agent was replayed: %+v %v calls=%d", again, err, calls.Load())
	}
	if err := os.WriteFile(filepath.Join(dir, "graph-tools", "mixed", "nodes", "left", "evidence.txt"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := mixedGraphCall(t, ctx, j, o, spec); err == nil || !strings.Contains(err.Error(), "SHA-256 changed") {
		t.Fatalf("changed child evidence was reused: %v", err)
	}
}

func TestAgentGraphRespectsParentDeadlineAndFailureRoute(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	parentDeadline := time.Now().Add(time.Second)
	var sawDeadline time.Time
	o := Options{RunDir: dir, graphDeadline: parentDeadline, graphProvider: func(string) (agent.Provider, error) {
		return scenarioProvider(func(ctx context.Context, _ []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
			sawDeadline, _ = ctx.Deadline()
			return agent.Message{}, errors.New("specific child failure")
		}), nil
	}}
	spec := commandGraphSpec{Key: "route", Nodes: []commandGraphNode{
		{ID: "check", Kind: "agent", Task: "observe", Timeout: 600, Resources: []string{}, Optional: true},
		{ID: "on-failure", Command: "printf 'parent must review failure'", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "check", Optional: true}}, When: &commandGraphWhen{Node: "check", Status: "failed"}},
		{ID: "on-success", Command: "exit 99", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "check", Optional: true}}, When: &commandGraphWhen{Node: "check", Status: "succeeded"}},
	}}
	checkpoint, err := mixedGraphCall(t, context.Background(), j, o, spec)
	if err != nil || checkpoint.Status != "succeeded" || !sawDeadline.Equal(parentDeadline) {
		t.Fatalf("failed route/deadline: %+v %v deadline=%v", checkpoint, err, sawDeadline)
	}
	for _, node := range checkpoint.Nodes {
		want := map[string]string{"check": "failed", "on-failure": "succeeded", "on-success": "skipped"}[node.ID]
		if node.Status != want {
			t.Fatalf("wrong route: %+v", node)
		}
	}
	o.graphDeadline = time.Now().Add(-time.Second)
	o.graphProvider = func(string) (agent.Provider, error) {
		t.Fatal("expired parent started a new model session")
		return nil, nil
	}
	spec.Key = "expired"
	checkpoint, err = mixedGraphCall(t, context.Background(), j, o, spec)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	for _, node := range checkpoint.Nodes {
		if node.Status == "succeeded" {
			t.Fatalf("expired parent executed child: %+v", node)
		}
	}
}

func TestMixedGraphRejectsAmbiguousCapabilitiesBeforeExecution(t *testing.T) {
	for _, node := range []commandGraphNode{
		{ID: "bad", Kind: "agent", Task: "task", Command: "touch outside", Resources: []string{}},
		{ID: "bad", Kind: "command", Task: "task", Resources: []string{}},
		{ID: "bad", Kind: "agent", Resources: []string{}},
		{ID: "bad", Kind: "agent", Task: "task", Resources: []string{}, Artifacts: []string{"session.json"}},
	} {
		spec := commandGraphSpec{Key: "invalid", Nodes: []commandGraphNode{node}}
		if err := validateCommandGraph(&spec); err == nil {
			t.Fatalf("accepted ambiguous node: %+v", node)
		}
	}
}

func TestAgentReportsAreNotDirectEvidenceReceipts(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	o := Options{RunDir: dir, graphProvider: func(string) (agent.Provider, error) {
		return scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
			return agent.Text("assistant", "An interpretation that still needs evidence."), nil
		}), nil
	}}
	raw := json.RawMessage(`{"key":"evidence-kinds","nodes":[{"id":"interpretation","kind":"agent","task":"assess","resources":[]},{"id":"observation","command":"printf 'observed byte'","resources":[]}]}`)
	result, err := runCommandGraph(context.Background(), j, o, raw)
	var receipt struct {
		Evidence []commandGraphEvidence `json:"verified_evidence"`
	}
	if err != nil || json.Unmarshal([]byte(result), &receipt) != nil || len(receipt.Evidence) != 1 || receipt.Evidence[0].NodeID != "observation" {
		t.Fatalf("Agent prose was offered as direct raw evidence: %s %v", result, err)
	}
}

func TestAgentGraphInheritsOnlyBoundParentConstraints(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		t.Run(fmt.Sprintf("snapshot=%v", snapshot), func(t *testing.T) {
			j, state := snapshotRuntimeFixture(t, "explore")
			state.Graph.Facts[0].Description = "At most one HTTP request across all workers; never retry /private."
			state.Graph.Hints = []board.Hint{{ID: "h-limit", Content: "Use only the supplied account; do not submit guesses.", Creator: "user"}}
			state.Steps[0].Description = "Inspect the retained response only; acquisition has already consumed the request allowance."
			state.Steps[0].WritePaths = []string{"/workspace/report.json"}
			state.Steps[0].DependsOn = []string{"upstream"}
			state.Steps = append(state.Steps, board.Step{ID: "unrelated", GoalID: "goal", Description: "Unrelated planning noise", Status: "open"})
			state.FactRecords[0].Description = "Unrelated observation noise"
			var err error
			if snapshot {
				j.InputView, err = board.ContextView(state, j.Intent.ID, board.DefaultContextViewBytes)
			} else {
				j.InputSnapshot, j.InputView = nil, nil
				j.Graph, j.State = state.Graph, &state
			}
			if err != nil {
				t.Fatal(err)
			}
			// The bound view, rather than a later copy of the intent, supplies
			// its full task constraints in both supported input formats.
			j.Intent.Description = "Later intent copy must not replace bound constraints"
			requests := 0
			o := Options{RunDir: t.TempDir(), graphProvider: func(string) (agent.Provider, error) {
				return scenarioProvider(func(_ context.Context, history []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
					requests++
					prompt := history[0].Text()
					_, body, ok := strings.Cut(prompt, "<parent_constraints>\n")
					body, _, closed := strings.Cut(body, "\n</parent_constraints>")
					var inherited struct {
						UserInputs []board.Fact `json:"user_inputs"`
						Hints      []board.Hint `json:"hints"`
						Step       board.Step   `json:"step"`
					}
					if !ok || !closed || json.Unmarshal([]byte(body), &inherited) != nil {
						return agent.Message{}, errors.New("child has no structured parent constraints")
					}
					if !reflect.DeepEqual(inherited.UserInputs, state.Graph.Facts) || !reflect.DeepEqual(inherited.Hints, state.Graph.Hints) || inherited.Step.Description != state.Steps[0].Description || !reflect.DeepEqual(inherited.Step.WritePaths, state.Steps[0].WritePaths) || !reflect.DeepEqual(inherited.Step.DependsOn, state.Steps[0].DependsOn) {
						return agent.Message{}, fmt.Errorf("child lost original limits or Step contract: %+v", inherited)
					}
					for _, noise := range []string{"Unrelated planning noise", "Unrelated observation noise", "Later intent copy", `"fact_records"`, `"status"`} {
						if strings.Contains(prompt, noise) {
							return agent.Message{}, fmt.Errorf("child received unrelated context: %s", noise)
						}
					}
					for _, def := range defs {
						if def.Name == "graph_action" || def.Name == "run_graph" || def.Name == "finish_step" || def.Name == "read_graph" {
							return agent.Message{}, fmt.Errorf("child gained parent capability %s", def.Name)
						}
					}
					return agent.Text("assistant", "Assessed retained evidence within the inherited limits."), nil
				}), nil
			}}
			spec := commandGraphNode{ID: "assess", Kind: "agent", Task: "Explain the retained result", Timeout: 10, Resources: []string{}}
			if _, err := executeAgentNode(context.Background(), j, o, "bounded", t.TempDir(), spec, nil); err != nil || requests != 1 {
				t.Fatalf("inherited constraints failed: %v requests=%d", err, requests)
			}
		})
	}
}

func TestAgentGraphRejectsMissingBoundStepBeforeModelRequest(t *testing.T) {
	j, _ := snapshotRuntimeFixture(t, "explore")
	var view map[string]json.RawMessage
	if err := json.Unmarshal(j.InputView, &view); err != nil {
		t.Fatal(err)
	}
	view["steps"] = json.RawMessage(`[]`)
	j.InputView, _ = json.Marshal(view)
	o := Options{RunDir: t.TempDir(), graphProvider: func(string) (agent.Provider, error) {
		t.Fatal("incomplete immutable input started a model request")
		return nil, nil
	}}
	spec := commandGraphNode{ID: "assess", Kind: "agent", Task: "Inspect", Timeout: 10, Resources: []string{}}
	if _, err := executeAgentNode(context.Background(), j, o, "missing", t.TempDir(), spec, nil); err == nil || !strings.Contains(err.Error(), "immutable input") {
		t.Fatalf("missing bound Step silently used mutable intent text: %v", err)
	}
}

func TestAgentGraphScopesScenarioPolicyAndTools(t *testing.T) {
	for _, tc := range []struct {
		scenario   string
		submission bool
	}{{"", false}, {"pentest", true}, {"ctf", false}, {"ctf", true}} {
		t.Run(fmt.Sprintf("%s/submission=%v", tc.scenario, tc.submission), func(t *testing.T) {
			t.Setenv("TSEC_SERVER_HOST", "")
			t.Setenv("TSEC_AGENT_TOKEN", "")
			if tc.submission {
				t.Setenv("TSEC_SERVER_HOST", "contest.invalid")
				t.Setenv("TSEC_AGENT_TOKEN", "fixture-token")
			}
			j := scenarioJob(t, tc.scenario, "explore")
			requests := 0
			o := Options{RunDir: t.TempDir(), graphProvider: func(string) (agent.Provider, error) {
				return scenarioProvider(func(_ context.Context, history []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
					requests++
					prompt := history[0].Text()
					ctf := tc.scenario == "ctf" && tc.submission
					if strings.Contains(prompt, pentestPolicy) != (tc.scenario == "pentest") || strings.Contains(prompt, ctfPolicy) != ctf || strings.Count(prompt, executionDiscipline) != 1 {
						return agent.Message{}, errors.New("child scenario policy or execution limits were lost or duplicated")
					}
					tools := map[string]bool{}
					for _, def := range defs {
						tools[def.Name] = true
						if strings.Contains(def.Description, ctfExecution) != (ctf && def.Name == "bash") {
							return agent.Message{}, errors.New("CTF submission instructions escaped their configured bash capability")
						}
					}
					if tools["cvss31"] != (tc.scenario == "pentest") || tools["graph_action"] || tools["run_graph"] || tools["finish_step"] || tools["read_graph"] {
						return agent.Message{}, fmt.Errorf("wrong child tool boundary: %v", tools)
					}
					return agent.Text("assistant", "Scenario constraints reviewed."), nil
				}), nil
			}}
			spec := commandGraphNode{ID: "assess", Kind: "agent", Task: "Assess fixture", Timeout: 10, Resources: []string{}}
			if _, err := executeAgentNode(context.Background(), j, o, "scenario", t.TempDir(), spec, nil); err != nil || requests != 1 {
				t.Fatalf("child scenario delivery failed: %v requests=%d", err, requests)
			}
		})
	}
}

func TestAgentGraphCancellationKillsOwnedProcessesAndNeverReplays(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	var calls atomic.Int32
	o := Options{RunDir: dir, graphProvider: func(string) (agent.Provider, error) {
		return scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
			calls.Add(1)
			return draftModelCall("side-effect", "bash", `{"command":"printf start >> started; sleep 30; touch forbidden"}`), nil
		}), nil
	}}
	spec := commandGraphSpec{Key: "interrupted-agent", Nodes: []commandGraphNode{{ID: "slow", Kind: "agent", Task: "inspect", Resources: []string{}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := mixedGraphCall(t, ctx, j, o, spec)
		done <- err
	}()
	nodeDir := filepath.Join(dir, "graph-tools", spec.Key, "nodes", "slow")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(nodeDir, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child process did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation did not reach child Loop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child Loop did not stop after cancellation")
	}
	if _, err := mixedGraphCall(t, context.Background(), j, o, spec); err == nil || !strings.Contains(err.Error(), "reconcil") || calls.Load() != 1 {
		t.Fatalf("uncertain Agent work replayed: calls=%d error=%v", calls.Load(), err)
	}
	started, err := os.ReadFile(filepath.Join(nodeDir, "started"))
	if err != nil || string(started) != "start" {
		t.Fatalf("side effect repeated: %s %v", started, err)
	}
	groups, err := filepath.Glob(filepath.Join(dir, "group-*"))
	if err != nil || len(groups) != 0 {
		t.Fatalf("owned child processes were not reaped: %v %v", groups, err)
	}
	if _, err := os.Stat(filepath.Join(nodeDir, "forbidden")); !os.IsNotExist(err) {
		t.Fatal("cancelled child process continued")
	}
}

func TestInitialWorkerRequestScopesExecutionDiscipline(t *testing.T) {
	for _, kind := range []string{"explore", "reason", "curate"} {
		t.Run(kind, func(t *testing.T) {
			job, dir := outcomeJob(t, kind), t.TempDir()
			if kind == "curate" {
				job = curationJob(t)
			}
			options := Options{RunDir: dir}
			if controlJob(job) {
				options.Output = &draftTestBridge{dir: dir, handle: func(request GraphRequest) (any, error) {
					switch request.Op {
					case "decision_receipt":
						return board.DecisionReceipt{}, nil
					case "curate_receipt":
						return board.StateActionResult{}, nil
					default:
						t.Fatalf("prompt-only run unexpectedly used %s", request.Op)
						return nil, nil
					}
				}}
			}
			calls := 0
			options.Provider = scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				want := 1
				if controlJob(job) {
					want = 0
				}
				if len(history) == 0 || strings.Count(history[0].Text(), executionDiscipline) != want {
					t.Fatalf("first %s provider request lost or duplicated role-scoped execution guidance", kind)
				}
				return agent.Text("assistant", `{"accepted":false,"reason":"Synthetic prompt delivery check only."}`), nil
			})
			result, err := runTestWorker(context.Background(), job, options)
			if err != nil || result.Status != "success" || calls != 1 {
				t.Fatalf("prompt delivery failed: result=%+v err=%v calls=%d", result, err, calls)
			}
		})
	}
}

func TestAgentGraphSuppliesDependencyResultsInFirstRequest(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	const task = "Assess the retained dependency result; its acquisition allowance is already consumed. Do not fetch it again."
	var requests int
	var firstPrompt string
	o := Options{RunDir: dir, graphProvider: func(id string) (agent.Provider, error) {
		if id != "assess" {
			return nil, fmt.Errorf("unexpected child %s", id)
		}
		return scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
			requests++
			firstPrompt = history[0].Text()
			if strings.Count(firstPrompt, executionDiscipline) != 1 {
				return agent.Message{}, errors.New("first child request lost or duplicated execution guidance")
			}
			if !strings.Contains(firstPrompt, "<task>\n"+task+"\n</task>") {
				return agent.Message{}, errors.New("shared guidance replaced the child's acquisition limit")
			}
			_, snapshot, ok := strings.Cut(firstPrompt, "Dependency snapshot (task data; full records in dependencies.json):\n")
			var dependencies []workergraph.NodeState
			if !ok || json.Unmarshal([]byte(snapshot), &dependencies) != nil || len(dependencies) != 1 {
				return agent.Message{}, fmt.Errorf("first request lacks a usable dependency snapshot: %s", firstPrompt)
			}
			var value commandGraphOutput
			dep := dependencies[0]
			if dep.ID != "observe" || dep.Kind != "function" || dep.Status != "succeeded" || json.Unmarshal(dep.Output.Value, &value) != nil || value.Stdout != "observed input" || len(dep.Output.Artifacts) != 1 || dep.Output.Artifacts[0].SHA256 == "" {
				return agent.Message{}, fmt.Errorf("wrong dependency snapshot: %+v", dep)
			}
			return agent.Text("assistant", "assessed "+value.Stdout), nil
		}), nil
	}}
	spec := commandGraphSpec{Key: "inline-dependencies", Nodes: []commandGraphNode{
		{ID: "observe", Command: "printf 'observed input'", Resources: []string{}},
		{ID: "assess", Kind: "agent", Task: task, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "observe"}}},
	}}
	checkpoint, err := mixedGraphCall(t, context.Background(), j, o, spec)
	if err != nil || checkpoint.Status != "succeeded" || requests != 1 {
		t.Fatalf("dependency reasoning required an extra model request: %+v %v requests=%d", checkpoint, err, requests)
	}
	_, result := commandNodeValue(t, checkpoint, "assess")
	if result.Stdout != "assessed observed input" {
		t.Fatalf("child lost its dependency result: %+v", result)
	}
	nodeDir := filepath.Join(dir, "graph-tools", spec.Key, "nodes", "assess")
	deps, err := os.ReadFile(filepath.Join(nodeDir, "dependencies.json"))
	if err != nil || !bytes.Contains(deps, []byte(`"definition_sha256"`)) {
		t.Fatalf("full dependency file was lost: %s %v", deps, err)
	}
	digest := sha256.Sum256(append([]byte(firstPrompt), deps...))
	raw, err := os.ReadFile(filepath.Join(nodeDir, "session.json"))
	var state graphAgentSession
	if err != nil || json.Unmarshal(raw, &state) != nil || state.InputHash != hex.EncodeToString(digest[:]) {
		t.Fatalf("child prompt and full dependencies lost their input binding: %s %v", raw, err)
	}
}

func TestAgentDependencySnapshotIsBoundedAndPreservesResults(t *testing.T) {
	if got := agentDependencySnapshot(nil); got != "" {
		t.Fatalf("empty dependencies added context: %s", got)
	}
	dependencies := []workergraph.NodeState{
		{ID: "failed", Kind: "agent", Status: "failed", Error: "model unavailable", Attempt: 1, StartedAt: time.Now(), RunDurationMS: 100, InputSHA256: "internal-input"},
		{ID: "skipped", Kind: "function", Status: "skipped", Reason: "condition unmatched", DefinitionSHA256: "internal-definition"},
	}
	snapshot := agentDependencySnapshot(commandGraphViews(dependencies))
	var decoded []workergraph.NodeState
	if json.Unmarshal([]byte(snapshot), &decoded) != nil || len(decoded) != 2 || decoded[0].Error != dependencies[0].Error || decoded[1].Reason != dependencies[1].Reason {
		t.Fatalf("failure or skip context was lost: %s", snapshot)
	}
	for _, noise := range []string{"attempt", "started_at", "run_duration_ms", "input_sha256", "definition_sha256"} {
		if strings.Contains(snapshot, noise) {
			t.Fatalf("dependency snapshot includes runtime metadata %s: %s", noise, snapshot)
		}
	}
	// Enforce the bound on encoded bytes, including JSON escaping, and omit
	// the entire snapshot when it cannot fit rather than implying completeness.
	for _, content := range []string{strings.Repeat("x", 16<<10), strings.Repeat("<", 4000)} {
		dependencies[0].Output.Value, _ = json.Marshal(content)
		if got := agentDependencySnapshot(commandGraphViews(dependencies)); got != "" {
			t.Fatalf("oversized dependency snapshot reached the model: %d bytes", len(got))
		}
	}
}

func TestAgentGraphRejectsTruncatedFinalReportsWithoutReplay(t *testing.T) {
	for _, stopReason := range []string{"max_tokens", "length"} {
		t.Run(stopReason, func(t *testing.T) {
			j, dir := graphWrapperJob(t), t.TempDir()
			requests := 0
			o := Options{RunDir: dir, graphProvider: func(string) (agent.Provider, error) {
				return scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
					requests++
					message := agent.Text("assistant", "partial account")
					message.StopReason = stopReason
					return message, nil
				}), nil
			}}
			spec := commandGraphSpec{Key: "truncated-report", Nodes: []commandGraphNode{
				{ID: "assess", Kind: "agent", Task: "Assess", Resources: []string{}},
				{ID: "publish", Command: "printf 'must not run'", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "assess"}}},
			}}
			checkpoint, err := mixedGraphCall(t, context.Background(), j, o, spec)
			if err == nil || !strings.Contains(err.Error(), "truncated") || checkpoint.Status != "failed" || requests != 1 {
				t.Fatalf("truncated account was accepted: %+v %v requests=%d", checkpoint, err, requests)
			}
			for _, node := range checkpoint.Nodes {
				want := map[string]string{"assess": "failed", "publish": "blocked"}[node.ID]
				if node.Status != want {
					t.Fatalf("truncated result triggered downstream work: %+v", node)
				}
			}
			nodeDir := filepath.Join(dir, "graph-tools", spec.Key, "nodes", "assess")
			raw, err := os.ReadFile(filepath.Join(nodeDir, "session.json"))
			var state graphAgentSession
			if err != nil || json.Unmarshal(raw, &state) != nil || state.Result != "partial account" || !strings.Contains(state.Error, "truncated") {
				t.Fatalf("incomplete report was not retained: %s %v", raw, err)
			}
			final, ok := lastAssistant(state.History)
			if !ok || final.StopReason != stopReason {
				t.Fatalf("original truncation metadata was lost: %+v", final)
			}
			stdout, err := os.ReadFile(filepath.Join(nodeDir, "stdout.log"))
			if err != nil || string(stdout) != state.Result {
				t.Fatalf("partial account log was lost: %s %v", stdout, err)
			}
			if _, err := mixedGraphCall(t, context.Background(), j, o, spec); err == nil || requests != 1 {
				t.Fatalf("failed child was automatically replayed: %v requests=%d", err, requests)
			}
		})
	}
}

func TestAgentGraphRetainsTruncatedToolRecovery(t *testing.T) {
	for _, stopReason := range []string{"max_tokens", "length"} {
		t.Run(stopReason, func(t *testing.T) {
			j, dir := graphWrapperJob(t), t.TempDir()
			requests := 0
			o := Options{RunDir: dir, graphProvider: func(string) (agent.Provider, error) {
				return scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
					requests++
					switch requests {
					case 1:
						message := draftModelCall("incomplete", "bash", `{"command":"printf x >> effects"}`)
						message.StopReason = stopReason
						return message, nil
					case 2:
						if _, err := dynamicToolReply(history, "incomplete"); err == nil || !strings.Contains(err.Error(), "truncated") {
							return agent.Message{}, fmt.Errorf("truncated tool call was not safely rejected: %v", err)
						}
						return draftModelCall("complete", "bash", `{"command":"printf x >> effects"}`), nil
					default:
						return agent.Text("assistant", "complete account"), nil
					}
				}), nil
			}}
			spec := commandGraphSpec{Key: "truncated-tool", Nodes: []commandGraphNode{{ID: "work", Kind: "agent", Task: "Work", Resources: []string{}, Artifacts: []string{"effects"}}}}
			checkpoint, err := mixedGraphCall(t, context.Background(), j, o, spec)
			if err != nil || checkpoint.Status != "succeeded" || requests != 3 {
				t.Fatalf("existing truncated tool recovery was broken: %+v %v requests=%d", checkpoint, err, requests)
			}
			content, err := os.ReadFile(filepath.Join(dir, "graph-tools", spec.Key, "nodes", "work", "effects"))
			if err != nil || string(content) != "x" {
				t.Fatalf("truncated call executed a side effect: %s %v", content, err)
			}
		})
	}
}

func TestAgentGraphPassesDeclaredArtifactPathsToChild(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	// Artifact names remain JSON data, including quotes, line breaks and markup.
	artifactName := "nested/report \"v1\"\n<done>.json"
	requests := 0
	o := Options{RunDir: dir, graphProvider: func(string) (agent.Provider, error) {
		return scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
			requests++
			if requests == 1 {
				_, raw, ok := strings.Cut(history[0].Text(), "Required output artifacts (JSON array of paths relative to PWNMESH_NODE_DIR, your working directory; create before returning): ")
				var paths []string
				if !ok || json.Unmarshal([]byte(raw), &paths) != nil || len(paths) != 1 || paths[0] != artifactName {
					return agent.Message{}, errors.New("child did not receive the declared artifact contract as JSON")
				}
				args, _ := json.Marshal(map[string]string{"path": paths[0], "content": "stored output"})
				return draftModelCall("store-output", "write", string(args)), nil
			}
			if _, err := dynamicToolReply(history, "store-output"); err != nil {
				return agent.Message{}, err
			}
			return agent.Text("assistant", "created the required output"), nil
		}), nil
	}}
	spec := commandGraphSpec{Key: "declared-artifacts", Nodes: []commandGraphNode{{ID: "produce", Kind: "agent", Task: "Produce the requested result", Resources: []string{}, Artifacts: []string{artifactName}}}}
	checkpoint, err := mixedGraphCall(t, context.Background(), j, o, spec)
	if err != nil || checkpoint.Status != "succeeded" || requests != 2 {
		t.Fatalf("child could not honor the declared artifact contract: %+v %v requests=%d", checkpoint, err, requests)
	}
	node, _ := commandNodeValue(t, checkpoint, "produce")
	path := filepath.Join(dir, "graph-tools", spec.Key, "nodes", "produce", artifactName)
	if len(node.Output.Artifacts) != 2 || node.Output.Artifacts[1].Path != path {
		t.Fatalf("artifact escaped the node directory: %+v", node.Output.Artifacts)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "stored output" {
		t.Fatalf("declared artifact was not retained: %q %v", body, err)
	}
}

func TestAgentGraphMissingDeclaredArtifactReportsPathAndBlocksDependents(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	requests := 0
	o := Options{RunDir: dir, graphProvider: func(string) (agent.Provider, error) {
		return scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
			requests++
			return agent.Text("assistant", "claimed complete without producing the artifact"), nil
		}), nil
	}}
	spec := commandGraphSpec{Key: "missing-artifact", Nodes: []commandGraphNode{
		{ID: "produce", Kind: "agent", Task: "Produce a local result", Resources: []string{}, Artifacts: []string{"missing.json"}},
		{ID: "consume", Command: "printf 'must not run'", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "produce"}}},
	}}
	checkpoint, err := mixedGraphCall(t, context.Background(), j, o, spec)
	missing := filepath.Join(dir, "graph-tools", spec.Key, "nodes", "produce", "missing.json")
	if err == nil || !strings.Contains(err.Error(), missing) || checkpoint.Status != "failed" || requests != 1 {
		t.Fatalf("missing artifact was accepted or its path lost: %+v %v requests=%d", checkpoint, err, requests)
	}
	producer, _ := commandNodeValue(t, checkpoint, "produce")
	consumer, _ := commandNodeValue(t, checkpoint, "consume")
	if producer.Status != "failed" || !strings.Contains(producer.Error, missing) || consumer.Status != "blocked" {
		t.Fatalf("artifact validation did not block dependent work: producer=%+v consumer=%+v", producer, consumer)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(missing), "session.json"))
	var session graphAgentSession
	if err != nil || json.Unmarshal(raw, &session) != nil || !strings.Contains(session.Error, missing) || session.Result == "" {
		t.Fatalf("child session lost its artifact failure or final account: %s %v", raw, err)
	}
	if _, err := mixedGraphCall(t, context.Background(), j, o, spec); err == nil || requests != 1 {
		t.Fatalf("artifact failure automatically replayed the child: %v requests=%d", err, requests)
	}
}
