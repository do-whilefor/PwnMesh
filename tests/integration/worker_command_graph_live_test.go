//go:build linux

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
	"pwnmesh/internal/workergraph"
)

// This workload exercises the production Loop, command DAG, graph bridge,
// evidence retention and completion using only synthetic local data in a small image.
func TestLiveWorkerCommandGraphProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_WORKER_GRAPH_TEST") != "1" {
		t.Skip("opt in to real model and Docker command-graph acceptance")
	}
	origin := `Exercise a small Worker command graph using ONLY synthetic local files under /workspace. Do not read credentials, install packages or contact other services.
The main Agent must authorize exactly one ordinary Step and preserve the following execution contract in its task. The Worker must use the run_graph tool with key sample-sum and parallelism 2, with exactly three required nodes: left, right, sum. left and right have no dependencies. left executes sleep 1 followed by printing the JSON array [2,3] to stdout; right executes sleep 1 followed by printing [5]. These deliberate one-second waits make actual branch overlap observable. sum depends_on both left and right, reads the two retained stdout files through the PWNMESH_DEPENDENCIES JSON file, concatenates their arrays in left/right order, and computes their sum with Python. It prints exactly one JSON object with values (the actual concatenated array) and sum (computed, never hardcoded). Use the tool's private node directories and retained stdout.log files; no shared output file is necessary. Do not simulate the graph with a bash background job, extra Steps, or sequential tool calls.
After the graph succeeds, publish one fact with scope sample-v1 describing the independently executed branches and calculated sum, using all three absolute stdout paths as evidence. Return that fact as the Step result. There is no ambiguous interpretation, report or dispute to create. The main Agent may complete after the Step succeeds, using its accepted result and retained raw evidence. No additional Step, curation or candidate judgment is needed.`
	runObservedProject(t, "Worker command DAG with parallel inputs and deterministic join", origin,
		"Execute the required three-node Worker graph, retain both input arrays and the computed values=[2,3,5], sum=10 receipt, and complete from the accepted evidence without curation.",
		"worker_command_graph_with_retained_evidence", validateWorkerCommandGraphDelivery, 1)
}

// Three paired runs isolate command scheduling from provider latency. They
// measure the real runtime tool in the same Docker image as the live workload;
// this is not an estimate of whole-project speedup.
func TestWorkerCommandGraphTiming(t *testing.T) {
	out := os.Getenv("PWNMESH_WORKER_GRAPH_TIMING_OUTPUT")
	if out == "" {
		t.Skip("opt in with an output path for controlled scheduling measurements")
	}
	var measurements []map[string]any
	join := `python3 - <<'PY'
import json, os
deps = {d['id']: d for d in json.load(open(os.environ['PWNMESH_DEPENDENCIES']))}
values = []
for name in ('left', 'right'):
    values.extend(json.load(open(deps[name]['output']['value']['output_path'])))
print(json.dumps({'values': values, 'sum': sum(values)}))
PY`
	for repeat := 1; repeat <= 3; repeat++ {
		// Alternate order to avoid always giving one setting the warm run.
		order := []int{1, 2}
		if repeat%2 == 0 {
			order = []int{2, 1}
		}
		for _, parallelism := range order {
			workspace := t.TempDir()
			run := fmt.Sprintf("trial-%d-p%d", repeat, parallelism)
			dir := filepath.Join(workspace, ".pwnmesh", "runs", run)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			j := worker.Job{RunID: run, Kind: "explore", Workspace: workspace, Graph: board.Graph{Project: board.Project{ID: "timing", OrchestrationVersion: 1}}}
			o := worker.Options{RunDir: dir}
			if err := worker.ConfigureRuntimeTools(j, &o); err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(o.Tools, func(tool agent.Tool) bool { return tool.Name == "run_graph" })
			if i < 0 {
				t.Fatal("command graph is not exposed by the production Worker")
			}
			raw, _ := json.Marshal(map[string]any{"key": "sample-sum", "parallelism": parallelism, "nodes": []map[string]any{
				{"id": "left", "command": "sleep 1; printf '[2,3]\\n'", "resources": []string{}},
				{"id": "right", "command": "sleep 1; printf '[5]\\n'", "resources": []string{}},
				{"id": "sum", "depends_on": []workergraph.Dependency{{ID: "left"}, {ID: "right"}}, "command": join, "resources": []string{}},
			}})
			started := time.Now()
			result, err := o.Tools[i].Execute(context.Background(), raw)
			wall := time.Since(started).Seconds()
			if err != nil {
				t.Fatalf("graph tool failed: %s: %v", result, err)
			}
			base := filepath.Join(dir, "graph-tools", "sample-sum") + "/"
			cpBytes, err := os.ReadFile(base + "graph.json")
			if err != nil {
				t.Fatal(err)
			}
			var cp workergraph.Checkpoint
			if err = json.Unmarshal(cpBytes, &cp); err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{}
			for _, id := range []string{"left", "right", "sum"} {
				for _, name := range []string{"stdout.log", "dependencies.json"} {
					path := base + "nodes/" + id + "/" + name
					files[path], err = os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			failures := validateCommandGraphCheckpoint(cp, run, base, files)
			if parallelism == 1 {
				if !slices.Contains(failures, "independent input commands did not overlap") {
					t.Fatal("serial configuration unexpectedly overlapped")
				}
				failures = slices.DeleteFunc(failures, func(s string) bool { return s == "independent input commands did not overlap" })
			}
			if len(failures) != 0 {
				t.Fatal(failures)
			}
			measurements = append(measurements, map[string]any{"repeat": repeat, "parallelism": parallelism, "wall_seconds": wall, "checkpoint": cp, "passed": true})
			t.Logf("repeat=%d parallelism=%d tool_wall_seconds=%.3f", repeat, parallelism, wall)
		}
	}
	if err := saveLiveJSON(out, map[string]any{"scope": "controlled command graph only; three matched pairs, not project or model latency", "measurements": measurements}); err != nil {
		t.Fatal(err)
	}
}

func validateWorkerCommandGraphDelivery(state board.State, files map[string][]byte) []string {
	failures := []string{}
	var steps []board.Step
	markers := 0
	for _, step := range state.Steps {
		if staleRepairCompletionMarker(state, step) {
			markers++
		} else {
			steps = append(steps, step)
		}
	}
	if state.Graph.Project.Status != "completed" || state.Graph.Project.OrchestrationVersion != 1 || len(steps) != 1 || markers > 1 || len(state.Disputes) != 0 || len(state.Candidates) != 0 {
		return []string{"expected one completed new-mode Step without a dispute"}
	}
	step := steps[0]
	if step.Worker == nil || step.Result == nil {
		return []string{"missing successful Worker result"}
	}
	facts := map[string]board.FactRecord{}
	for _, fact := range state.FactRecords {
		facts[fact.ID] = fact
	}
	if _, ok := orchestrationSuccessfulRun(state.Graph.Project, step, *step.Worker, facts, files); !ok {
		failures = append(failures, "Step lacks a matching successful Loop session and accepted fact")
	}
	run := orchestrationRunID(*step.Worker)
	base := "/workspace/.pwnmesh/runs/" + run + "/graph-tools/sample-sum/"
	var checkpoint workergraph.Checkpoint
	if err := json.Unmarshal(files[base+"graph.json"], &checkpoint); err != nil {
		return append(failures, "missing command graph checkpoint")
	}
	failures = append(failures, validateCommandGraphCheckpoint(checkpoint, run, base, files)...)
	fact := facts[*step.Result]
	if fact.Scope != "sample-v1" {
		failures = append(failures, "result fact has the wrong observation scope")
	}
	if !commandGraphJournalValid(files["/workspace/.pwnmesh/runs/"+run+"/events.jsonl"], checkpoint) {
		failures = append(failures, "missing matching successful run_graph call and receipt in the Worker journal")
	}
	curated := state.Curation.RunID != "" || state.Curation.ThroughRevision != 0 || state.Curation.Request != nil || state.Graph.Project.Curator != nil
	for path, raw := range files {
		if !strings.HasPrefix(path, "/workspace/.pwnmesh/runs/") || !strings.HasSuffix(path, "/job.json") {
			continue
		}
		var job worker.Job
		if json.Unmarshal(raw, &job) == nil && job.Kind == "curate" {
			curated = true
		}
	}
	if curated {
		failures = append(failures, "unambiguous command graph unnecessarily requested or ran curation")
	}
	if !slices.ContainsFunc(state.Goals, func(goal board.Goal) bool {
		return goal.ID == "goal" && goal.Status == "achieved" && goal.SupportValid && slices.Contains(goal.Sources, fact.ID) && state.ValidateFactSources(goal.Sources, true) == nil
	}) {
		failures = append(failures, "root goal does not have valid support from the accepted graph result")
	}
	for _, id := range []string{"left", "right", "sum"} {
		body, exists := files[base+"nodes/"+id+"/stdout.log"]
		retained := exists && slices.ContainsFunc(fact.Evidence, func(ref board.EvidenceRef) bool {
			return orchestrationEvidenceValid(ref, fact.RunID, files) && bytes.Equal(files[ref.Path], body)
		})
		if !retained {
			failures = append(failures, "accepted fact omitted retained node output: "+id)
		}
	}
	return failures
}

// Match the durable tool journal, not a model narrative or a standalone file
// named graph.json. Failed earlier calls do not become receipts or hide a later
// successful same-run reuse. Compare JSON values semantically across indentation.
func commandGraphJournalValid(raw []byte, checkpoint workergraph.Checkpoint) bool {
	calls := map[string]bool{}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event agent.Event
		if json.Unmarshal(line, &event) != nil {
			return false
		}
		if event.Message == nil {
			continue
		}
		for _, block := range event.Message.Content {
			if block.Type == "tool_use" {
				calls[block.ID] = event.Message.Role == "assistant" && block.Name == "run_graph" && commandGraphCallValid(block.Input)
			}
			if block.Type != "tool_result" || !calls[block.ToolUseID] {
				continue
			}
			delete(calls, block.ToolUseID)
			if block.IsError || event.Message.Role != "user" {
				continue
			}
			var text string
			var receipt struct {
				Key, Status, Error string
				Nodes              []workergraph.NodeState
			}
			if json.Unmarshal(block.Content, &text) != nil || json.Unmarshal([]byte(text), &receipt) != nil || receipt.Key != "sample-sum" || receipt.Status != "succeeded" || receipt.Error != "" || len(receipt.Nodes) != len(checkpoint.Nodes) {
				continue
			}
			matched := true
			seen := map[string]bool{}
			for _, node := range receipt.Nodes {
				matched = matched && !seen[node.ID] && slices.ContainsFunc(checkpoint.Nodes, func(saved workergraph.NodeState) bool {
					var actual, expected any
					return node.ID == saved.ID && node.Kind == saved.Kind && node.Status == "succeeded" && node.Status == saved.Status && node.Attempt == saved.Attempt && node.InputSHA256 == saved.InputSHA256 && node.Error == "" && node.StartedAt.Equal(saved.StartedAt) && node.FinishedAt.Equal(saved.FinishedAt) && json.Unmarshal(node.Output.Value, &actual) == nil && json.Unmarshal(saved.Output.Value, &expected) == nil && reflect.DeepEqual(actual, expected) && slices.Equal(node.Output.Artifacts, saved.Output.Artifacts)
				})
				seen[node.ID] = true
			}
			if matched {
				return true
			}
		}
	}
	return false
}

func commandGraphCallValid(raw json.RawMessage) bool {
	var call struct {
		Key         string
		Parallelism int
		Nodes       []struct {
			ID        string
			Command   string
			Optional  bool
			When      json.RawMessage
			Resources []string
			DependsOn []workergraph.Dependency `json:"depends_on"`
		}
	}
	if json.Unmarshal(raw, &call) != nil || call.Key != "sample-sum" || call.Parallelism != 2 || len(call.Nodes) != 3 {
		return false
	}
	seen := map[string]bool{}
	for _, node := range call.Nodes {
		if seen[node.ID] || !slices.Contains([]string{"left", "right", "sum"}, node.ID) || node.Optional || (len(node.When) != 0 && string(node.When) != "null") || node.Resources == nil || strings.TrimSpace(node.Command) == "" {
			return false
		}
		seen[node.ID] = true
		if node.ID != "sum" {
			if len(node.DependsOn) != 0 {
				return false
			}
		} else if len(node.DependsOn) != 2 || !slices.Contains(node.DependsOn, workergraph.Dependency{ID: "left"}) || !slices.Contains(node.DependsOn, workergraph.Dependency{ID: "right"}) {
			return false
		}
	}
	return true
}

// Check physical data and ordered execution, independently of the model's
// narrative and of the project's completed flag.
func validateCommandGraphCheckpoint(cp workergraph.Checkpoint, run, base string, files map[string][]byte) []string {
	failures := []string{}
	check := func(ok bool, text string) {
		if !ok {
			failures = append(failures, text)
		}
	}
	check(cp.SchemaVersion == 1 && cp.RunID == run && cp.Status == "succeeded" && len(cp.Nodes) == 3, "command graph identity or success is invalid")
	nodes := map[string]workergraph.NodeState{}
	for _, node := range cp.Nodes {
		_, duplicate := nodes[node.ID]
		check(!duplicate && slices.Contains([]string{"left", "right", "sum"}, node.ID), "unexpected or duplicate command node")
		nodes[node.ID] = node
		check(node.Kind == "function" && node.Status == "succeeded" && node.Attempt == 1 && !node.StartedAt.IsZero() && node.FinishedAt.After(node.StartedAt), "node has no successful execution interval: "+node.ID)
		var value struct {
			OutputPath string `json:"output_path"`
			ExitCode   int    `json:"exit_code"`
		}
		check(json.Unmarshal(node.Output.Value, &value) == nil && value.ExitCode == 0 && value.OutputPath == base+"nodes/"+node.ID+"/stdout.log", "node result path or exit code is invalid: "+node.ID)
		body, exists := files[value.OutputPath]
		digest := fmt.Sprintf("%x", sha256.Sum256(body))
		check(exists && slices.ContainsFunc(node.Output.Artifacts, func(a workergraph.Artifact) bool {
			return a.Path == value.OutputPath && a.SHA256 == digest
		}), "node stdout is missing or differs from its retained SHA-256: "+node.ID)
	}
	left, right, sum := nodes["left"], nodes["right"], nodes["sum"]
	check(left.StartedAt.Before(right.FinishedAt) && right.StartedAt.Before(left.FinishedAt), "independent input commands did not overlap")
	check(!sum.StartedAt.Before(left.FinishedAt) && !sum.StartedAt.Before(right.FinishedAt), "join ran before both inputs were accepted")
	var a, b []int
	check(json.Unmarshal(files[base+"nodes/left/stdout.log"], &a) == nil && slices.Equal(a, []int{2, 3}), "left input bytes differ")
	check(json.Unmarshal(files[base+"nodes/right/stdout.log"], &b) == nil && slices.Equal(b, []int{5}), "right input bytes differ")
	var receipt struct {
		Values []int `json:"values"`
		Sum    int   `json:"sum"`
	}
	check(json.Unmarshal(files[base+"nodes/sum/stdout.log"], &receipt) == nil && slices.Equal(receipt.Values, []int{2, 3, 5}) && receipt.Sum == 10, "join has no correct calculation receipt")
	var dependencies []workergraph.NodeState
	check(json.Unmarshal(files[base+"nodes/sum/dependencies.json"], &dependencies) == nil && len(dependencies) == 2, "join omitted persisted dependency inputs")
	for _, id := range []string{"left", "right"} {
		check(slices.ContainsFunc(dependencies, func(dep workergraph.NodeState) bool {
			var actual, expected any
			return dep.ID == id && dep.Status == "succeeded" && json.Unmarshal(dep.Output.Value, &actual) == nil && json.Unmarshal(nodes[id].Output.Value, &expected) == nil && reflect.DeepEqual(actual, expected) && slices.Equal(dep.Output.Artifacts, nodes[id].Output.Artifacts)
		}), "join dependency differs from accepted upstream output: "+id)
	}
	return failures
}

func TestCommandGraphAcceptanceRejectsBrokenProvenance(t *testing.T) {
	fixture := func() (workergraph.Checkpoint, map[string][]byte) {
		base := "/workspace/.pwnmesh/runs/r/graph-tools/sample-sum/"
		cp := workergraph.Checkpoint{SchemaVersion: 1, RunID: "r", Status: "succeeded"}
		files := map[string][]byte{}
		start := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
		for i, id := range []string{"left", "right", "sum"} {
			path := base + "nodes/" + id + "/stdout.log"
			body := []byte([]string{"[2,3]\n", "[5]\n", "{\"values\":[2,3,5],\"sum\":10}\n"}[i])
			files[path] = body
			value, _ := json.Marshal(map[string]any{"output_path": path, "exit_code": 0})
			at := start
			if id == "sum" {
				at = at.Add(2 * time.Second)
			}
			cp.Nodes = append(cp.Nodes, workergraph.NodeState{ID: id, Kind: "function", Status: "succeeded", Attempt: 1, StartedAt: at, FinishedAt: at.Add(time.Second), Output: workergraph.Output{Value: value, Artifacts: []workergraph.Artifact{{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(body))}}}})
		}
		files[base+"nodes/sum/dependencies.json"], _ = json.Marshal(cp.Nodes[:2])
		return cp, files
	}
	const base = "/workspace/.pwnmesh/runs/r/graph-tools/sample-sum/"
	cp, files := fixture()
	if failures := validateCommandGraphCheckpoint(cp, "r", base, files); len(failures) != 0 {
		t.Fatal(failures)
	}
	for _, tc := range []struct {
		name   string
		change func(*workergraph.Checkpoint, map[string][]byte)
	}{
		{"other_run", func(cp *workergraph.Checkpoint, _ map[string][]byte) { cp.RunID = "other" }},
		{"missing_node", func(cp *workergraph.Checkpoint, _ map[string][]byte) { cp.Nodes = cp.Nodes[:2] }},
		{"early_join", func(cp *workergraph.Checkpoint, _ map[string][]byte) { cp.Nodes[2].StartedAt = cp.Nodes[0].StartedAt }},
		{"serial_inputs", func(cp *workergraph.Checkpoint, _ map[string][]byte) { cp.Nodes[1].StartedAt = cp.Nodes[0].FinishedAt }},
		{"failed_input", func(cp *workergraph.Checkpoint, _ map[string][]byte) { cp.Nodes[0].Status = "failed" }},
		{"tampered_output", func(_ *workergraph.Checkpoint, f map[string][]byte) {
			f[base+"nodes/left/stdout.log"] = []byte("[100]")
		}},
		{"missing_dependencies", func(_ *workergraph.Checkpoint, f map[string][]byte) { delete(f, base+"nodes/sum/dependencies.json") }},
		{"forged_dependencies", func(_ *workergraph.Checkpoint, f map[string][]byte) {
			f[base+"nodes/sum/dependencies.json"] = []byte("[]")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp, files := fixture()
			tc.change(&cp, files)
			if len(validateCommandGraphCheckpoint(cp, "r", base, files)) == 0 {
				t.Fatal("accepted broken command graph")
			}
		})
	}
}

func workerCommandGraphDeliveryFixture(t *testing.T) (board.State, map[string][]byte) {
	t.Helper()
	state, files := curationRelationsDeliveryFixture(t)
	state.FactRecords = state.FactRecords[1:]
	state.FactRelations = nil
	state.Curation = board.CurationProgress{}
	delete(files, "/workspace/.pwnmesh/runs/curator-run/job.json")
	fact := &state.FactRecords[0]
	fact.Evidence = nil
	base := "/workspace/.pwnmesh/runs/producer-run/graph-tools/sample-sum/"
	cp := workergraph.Checkpoint{SchemaVersion: 1, RunID: "producer-run", Version: "commands-v1", Status: "succeeded"}
	start := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"left", "right", "sum"} {
		body := []byte([]string{"[2,3]\n", "[5]\n", "{\"values\":[2,3,5],\"sum\":10}\n"}[i])
		path := base + "nodes/" + id + "/stdout.log"
		files[path] = body
		fact.Evidence = append(fact.Evidence, retainOrchestrationEvidence("producer-run", body, files))
		value, _ := json.Marshal(map[string]any{"stdout": string(body), "output_path": path, "exit_code": 0})
		at := start
		if id == "sum" {
			at = at.Add(2 * time.Second)
		}
		cp.Nodes = append(cp.Nodes, workergraph.NodeState{ID: id, Kind: "function", Status: "succeeded", Attempt: 1, InputSHA256: strings.Repeat("a", 64), StartedAt: at, FinishedAt: at.Add(time.Second), Output: workergraph.Output{Value: value, Artifacts: []workergraph.Artifact{{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(body))}}}})
	}
	files[base+"graph.json"], _ = json.Marshal(cp)
	files[base+"nodes/sum/dependencies.json"], _ = json.MarshalIndent(cp.Nodes[:2], "", "  ")
	input := json.RawMessage(`{"key":"sample-sum","parallelism":2,"nodes":[{"id":"left","command":"sleep 1; printf '[2,3]\\n'","resources":[]},{"id":"right","command":"sleep 1; printf '[5]\\n'","resources":[]},{"id":"sum","command":"python3 join.py","resources":[],"depends_on":[{"id":"left"},{"id":"right"}]}]}`)
	receipt, _ := json.Marshal(map[string]any{"key": "sample-sum", "status": "succeeded", "nodes": cp.Nodes})
	content, _ := json.Marshal(string(receipt))
	journal := "/workspace/.pwnmesh/runs/producer-run/events.jsonl"
	files[journal] = nil
	for _, message := range []agent.Message{
		{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "graph-call", Name: "run_graph", Input: input}}},
		{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "graph-call", Content: content}}},
	} {
		raw, _ := json.Marshal(agent.Event{Type: "message", Message: &message})
		files[journal] = append(files[journal], append(raw, '\n')...)
	}
	return state, files
}

func changeCommandGraphJournal(t *testing.T, files map[string][]byte, change func(*agent.Block)) {
	t.Helper()
	const path = "/workspace/.pwnmesh/runs/producer-run/events.jsonl"
	var rewritten []byte
	for _, line := range bytes.Split(bytes.TrimSpace(files[path]), []byte{'\n'}) {
		var event agent.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		for i := range event.Message.Content {
			change(&event.Message.Content[i])
		}
		raw, _ := json.Marshal(event)
		rewritten = append(rewritten, append(raw, '\n')...)
	}
	files[path] = rewritten
}

func TestWorkerCommandGraphDeliveryWithoutCurationRequiresJournalAndGoal(t *testing.T) {
	state, files := workerCommandGraphDeliveryFixture(t)
	if failures := validateWorkerCommandGraphDelivery(state, files); len(failures) != 0 {
		t.Fatal(failures)
	}
	changeInput := func(from, to string) func(*board.State, map[string][]byte) {
		return func(_ *board.State, files map[string][]byte) {
			changeCommandGraphJournal(t, files, func(block *agent.Block) {
				if block.Type == "tool_use" {
					block.Input = bytes.ReplaceAll(block.Input, []byte(from), []byte(to))
				}
			})
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*board.State, map[string][]byte)
	}{
		{"missing_journal", func(_ *board.State, f map[string][]byte) {
			delete(f, "/workspace/.pwnmesh/runs/producer-run/events.jsonl")
		}},
		{"different_key", changeInput(`"key":"sample-sum"`, `"key":"other"`)},
		{"serial_call", changeInput(`"parallelism":2`, `"parallelism":1`)},
		{"optional_node", changeInput(`"id":"sum","command"`, `"id":"sum","optional":true,"command"`)},
		{"optional_dependency", changeInput(`"depends_on":[{"id":"left"}`, `"depends_on":[{"id":"left","optional":true}`)},
		{"missing_dependency", changeInput(`"depends_on":[{"id":"left"},{"id":"right"}]`, `"depends_on":[{"id":"left"}]`)},
		{"wrong_tool", func(_ *board.State, f map[string][]byte) {
			changeCommandGraphJournal(t, f, func(b *agent.Block) {
				if b.Type == "tool_use" {
					b.Name = "bash"
				}
			})
		}},
		{"error_receipt", func(_ *board.State, f map[string][]byte) {
			changeCommandGraphJournal(t, f, func(b *agent.Block) {
				if b.Type == "tool_result" {
					b.IsError = true
				}
			})
		}},
		{"unpaired_receipt", func(_ *board.State, f map[string][]byte) {
			changeCommandGraphJournal(t, f, func(b *agent.Block) {
				if b.Type == "tool_result" {
					b.ToolUseID = "other"
				}
			})
		}},
		{"different_receipt", func(_ *board.State, f map[string][]byte) {
			changeCommandGraphJournal(t, f, func(b *agent.Block) {
				if b.Type == "tool_result" {
					var text string
					_ = json.Unmarshal(b.Content, &text)
					b.Content, _ = json.Marshal(strings.ReplaceAll(text, "producer-run", "other-run"))
				}
			})
		}},
		{"unexpected_curation", func(s *board.State, _ map[string][]byte) { s.Curation.RunID = "curator-run" }},
		{"unexpected_curation_cursor", func(s *board.State, _ map[string][]byte) { s.Curation.ThroughRevision = 1 }},
		{"unexpected_curation_request", func(s *board.State, _ map[string][]byte) { s.Curation.Request = &board.CurationRequest{} }},
		{"unexpected_active_curator", func(s *board.State, _ map[string][]byte) { s.Graph.Project.Curator = &board.Reason{} }},
		{"unexpected_curator_job", func(_ *board.State, f map[string][]byte) {
			f["/workspace/.pwnmesh/runs/curator-run/job.json"], _ = json.Marshal(worker.Job{Kind: "curate"})
		}},
		{"unrelated_goal", func(s *board.State, _ map[string][]byte) { s.Goals[0].Sources = []string{"origin"} }},
		{"unsupported_goal", func(s *board.State, _ map[string][]byte) { s.Goals[0].SupportValid = false }},
		{"wrong_scope", func(s *board.State, _ map[string][]byte) { s.FactRecords[0].Scope = "other" }},
		{"extra_candidate", func(s *board.State, _ map[string][]byte) {
			s.Candidates = append(s.Candidates, board.Candidate{ID: "unrequested"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, files := workerCommandGraphDeliveryFixture(t)
			tc.change(&state, files)
			if failures := validateWorkerCommandGraphDelivery(state, files); len(failures) == 0 {
				t.Fatal("accepted missing or mismatched execution provenance")
			}
		})
	}
}

func TestWorkerCommandGraphDeliveryAllowsJSONFormattingAndEarlierFailure(t *testing.T) {
	state, files := workerCommandGraphDeliveryFixture(t)
	const checkpointPath = "/workspace/.pwnmesh/runs/producer-run/graph-tools/sample-sum/graph.json"
	var checkpoint workergraph.Checkpoint
	_ = json.Unmarshal(files[checkpointPath], &checkpoint)
	files[checkpointPath], _ = json.MarshalIndent(checkpoint, "", "    ")
	const journal = "/workspace/.pwnmesh/runs/producer-run/events.jsonl"
	success := append([]byte(nil), files[journal]...)
	changeCommandGraphJournal(t, files, func(block *agent.Block) {
		if block.Type == "tool_use" {
			block.ID = "earlier-failed-call"
		}
		if block.Type == "tool_result" {
			block.ToolUseID, block.IsError = "earlier-failed-call", true
		}
	})
	files[journal] = append(files[journal], success...)
	if failures := validateWorkerCommandGraphDelivery(state, files); len(failures) != 0 {
		t.Fatal("semantic JSON or earlier failed call rejected final successful provenance", failures)
	}
}
