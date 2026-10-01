//go:build linux

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/workergraph"
)

func TestLiveWorkerMixedGraphProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_WORKER_MIXED_GRAPH_TEST") != "1" {
		t.Skip("opt in to real model and Docker dynamic multi-Agent graph acceptance")
	}
	origin := `Use only synthetic local files under /workspace. Do not read credentials, install packages or contact other services.
Authorize exactly one ordinary Step preserving this execution contract. The parent Worker is the scheduling Agent: use its existing Loop to observe results and choose subsequent Agent assignments, node IDs and dependencies. Do not predeclare the whole graph or simulate Agents with shell background jobs.
The first successful run_graph call, key sample-agents, contains only one required discovery node of kind agent, with resources:[] and no dependencies. Its task is to use local Python tools and random.SystemRandom to generate 3 to 5 categories with distinct random names and 3 to 6 integer values per category, write manifest.json as {"categories":[{"name":"generated-name","values":[integers]}]}, and declare that file as an artifact. The discovery Agent returns the actual manifest JSON as its final answer so the parent sees the discovered categories in the tool receipt. Neither category names nor their count may be prescribed before discovery runs.
Only AFTER receiving that discovery result, choose one new Agent task for each actual category and append these nodes under the SAME graph key. Each category Agent depends on discovery, reads the original declared manifest artifact through PWNMESH_DEPENDENCIES (an absolute JSON FILE PATH), computes its assigned category from those values using local tools, and writes a declared result.json containing {"category":name,"count":number-of-values,"sum":computed-sum}. Node IDs are your choice; use resources:[] and private node output files. Child Agents return local accounts and do not publish blackboard records.
Append one required command join, in the same or a later call, depending on every category Agent. It reads their declared result.json artifacts through PWNMESH_DEPENDENCIES, and prints exactly one JSON object {"categories":[the actual per-category result objects],"count":computed-total-count,"sum":computed-total-sum}. Do not hardcode results. Every run_graph call with this key supplies the FULL cumulative node list: retain each old node definition unchanged and add new nodes; never delete or edit old nodes. The tool accepts 1..64 total nodes and parallelism 1..16; omitted parallelism defaults to min(node-count,16). Leave parallelism omitted for this exercise. Retained nodes must not execute again.
After the graph succeeds, the parent finishes the Step with one evidence-backed fact. Its fact.scope field must be exactly the string "mixed-agent-v2", with no explanation or extra text; put explanations in fact.description. Preserve this exact field requirement in the delegated Step. Evidence must include the original manifest.json, every result.json, and the join stdout.log. Child final prose is not raw evidence. The main Agent completes from the accepted Step result; no additional review, report or curation is needed. This tests data-driven Agent scheduling, not an arithmetic performance claim.`
	runObservedProject(t, "Worker Agent discovers work and dynamically delegates by category", origin,
		"Discover unpredictable local categories, then extend one Worker graph with an Agent for each observed category and a computed join; preserve original evidence and complete the project.",
		"worker_dynamic_mixed_graph_with_isolated_loops", validateWorkerMixedGraphDelivery, 1)
}

// Recheck the original live archive without contacting a model or modifying
// retained evidence. Use exactly the same delivery validator as the live run.
func TestRetainedWorkerMixedGraphProject(t *testing.T) {
	dir := os.Getenv("PWNMESH_WORKER_MIXED_GRAPH_REPLAY_DIR")
	if dir == "" {
		t.Skip("opt in with the directory containing retained state.json and workspace.tar")
	}
	rawState, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state board.State
	if err = json.Unmarshal(rawState, &state); err != nil {
		t.Fatal(err)
	}
	archive, err := os.Open(filepath.Join(dir, "workspace.tar"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	digest := sha256.New()
	files, err := retainLiveWorkspace(io.TeeReader(archive, digest), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("validation_scope=worker_dynamic_mixed_graph_with_isolated_loops workspace_sha256=%x state_sha256=%x", digest.Sum(nil), sha256.Sum256(rawState))
	if failures := validateWorkerMixedGraphDelivery(state, files); len(failures) != 0 {
		t.Fatal(failures)
	}
}

type mixedGraphNodeSpec struct {
	ID        string
	Kind      string
	Command   string
	Task      string
	Timeout   int
	DependsOn []workergraph.Dependency `json:"depends_on"`
	Resources []string
	Artifacts []string
	Optional  bool
	When      any
}

type mixedGraphCall struct {
	Key         string
	Parallelism int
	Nodes       []mixedGraphNodeSpec
}

type mixedCategoryResult struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
	Sum      int    `json:"sum"`
}

// Demand a durable discovery receipt BEFORE the model submits new assignments.
// A fixed DAG, two simultaneous tool calls, or a final checkpoint alone cannot
// establish that the parent actually scheduled from observed child results.
func mixedGraphJournal(raw []byte, checkpoint workergraph.Checkpoint) (string, mixedGraphCall, error) {
	type pendingCall struct {
		call     mixedGraphCall
		observed bool
	}
	pending := map[string]pendingCall{}
	committed := map[string]mixedGraphNodeSpec{}
	discovery := ""
	var last mixedGraphCall
	rounds := 0
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event agent.Event
		if json.Unmarshal(line, &event) != nil {
			return "", last, fmt.Errorf("invalid parent journal")
		}
		if event.Message == nil {
			continue
		}
		for _, block := range event.Message.Content {
			if block.Type == "tool_use" && block.Name == "run_graph" && event.Message.Role == "assistant" {
				var call mixedGraphCall
				if json.Unmarshal(block.Input, &call) == nil && call.Key == "sample-agents" {
					pending[block.ID] = pendingCall{call, discovery != ""}
				}
			}
			p, exists := pending[block.ToolUseID]
			if block.Type != "tool_result" || !exists {
				continue
			}
			delete(pending, block.ToolUseID)
			if block.IsError || event.Message.Role != "user" {
				continue
			}
			var text string
			var receipt struct {
				Key, Status, Error string
				Nodes              []workergraph.NodeState
			}
			if json.Unmarshal(block.Content, &text) != nil || json.Unmarshal([]byte(text), &receipt) != nil || receipt.Key != "sample-agents" || receipt.Status != "succeeded" || receipt.Error != "" {
				continue
			}
			if p.call.Parallelism != 0 || len(receipt.Nodes) != len(p.call.Nodes) {
				return "", last, fmt.Errorf("dynamic graph call or receipt shape is invalid")
			}
			next := map[string]mixedGraphNodeSpec{}
			for _, spec := range p.call.Nodes {
				if _, duplicate := next[spec.ID]; duplicate || spec.ID == "" || spec.Optional || spec.When != nil || spec.Resources == nil || len(spec.Resources) != 0 {
					return "", last, fmt.Errorf("invalid dynamic node definition")
				}
				if spec.Kind == "" {
					spec.Kind = "command"
				}
				if spec.Timeout == 0 {
					spec.Timeout = 120
					if spec.Kind == "agent" {
						spec.Timeout = 600
					}
				}
				slices.SortFunc(spec.DependsOn, func(a, b workergraph.Dependency) int { return strings.Compare(a.ID, b.ID) })
				slices.Sort(spec.Artifacts)
				if len(spec.DependsOn) == 0 {
					spec.DependsOn = nil
				}
				if len(spec.Artifacts) == 0 {
					spec.Artifacts = nil
				}
				next[spec.ID] = spec
			}
			for id, old := range committed {
				if !reflect.DeepEqual(old, next[id]) {
					return "", last, fmt.Errorf("old node deleted or changed: %s", id)
				}
			}
			seen := map[string]bool{}
			for _, node := range receipt.Nodes {
				spec, declared := next[node.ID]
				kind := "function"
				if spec.Kind == "agent" {
					kind = "agent"
				}
				if !declared || seen[node.ID] || node.Kind != kind || !slices.ContainsFunc(checkpoint.Nodes, func(saved workergraph.NodeState) bool { return mixedNodeReceiptEqual(node, saved) }) {
					return "", last, fmt.Errorf("graph receipt does not match retained node: %s", node.ID)
				}
				seen[node.ID] = true
			}
			if discovery == "" {
				if len(p.call.Nodes) != 1 || p.call.Nodes[0].Kind != "agent" || len(p.call.Nodes[0].DependsOn) != 0 {
					return "", last, fmt.Errorf("first graph did not contain only a discovery Agent")
				}
				discovery = p.call.Nodes[0].ID
			} else if !p.observed {
				return "", last, fmt.Errorf("assignments were submitted before the discovery receipt")
			}
			if len(next) > len(committed) {
				rounds++
			}
			committed, last = next, p.call
		}
	}
	if rounds < 2 || len(committed) != len(checkpoint.Nodes) {
		return "", last, fmt.Errorf("missing discovery followed by successful cumulative graph extension")
	}
	return discovery, last, nil
}

func mixedNodeReceiptEqual(a, b workergraph.NodeState) bool {
	var av, bv any
	return a.ID == b.ID && a.Kind == b.Kind && a.Status == "succeeded" && b.Status == "succeeded" && a.Attempt == 1 && b.Attempt == 1 && a.InputSHA256 == b.InputSHA256 && a.Error == "" && b.Error == "" && a.StartedAt.Equal(b.StartedAt) && a.FinishedAt.Equal(b.FinishedAt) && json.Unmarshal(a.Output.Value, &av) == nil && json.Unmarshal(b.Output.Value, &bv) == nil && reflect.DeepEqual(av, bv) && slices.Equal(a.Output.Artifacts, b.Output.Artifacts)
}

func validateDynamicMixedGraph(run string, files map[string][]byte) ([]string, []string) {
	failures, evidence := []string{}, []string{}
	check := func(ok bool, detail string) {
		if !ok {
			failures = append(failures, detail)
		}
	}
	base := "/workspace/.pwnmesh/runs/" + run + "/graph-tools/sample-agents/"
	var checkpoint workergraph.Checkpoint
	if json.Unmarshal(files[base+"graph.json"], &checkpoint) != nil {
		return evidence, []string{"missing dynamic mixed graph checkpoint"}
	}
	check(checkpoint.RunID == run && checkpoint.Status == "succeeded", "graph identity or terminal status mismatch")
	discovery, call, err := mixedGraphJournal(files["/workspace/.pwnmesh/runs/"+run+"/events.jsonl"], checkpoint)
	if err != nil {
		return evidence, append(failures, err.Error())
	}
	nodes := map[string]workergraph.NodeState{}
	for _, node := range checkpoint.Nodes {
		_, duplicate := nodes[node.ID]
		check(!duplicate && node.Status == "succeeded" && node.Attempt == 1 && !node.StartedAt.IsZero() && node.FinishedAt.After(node.StartedAt), "invalid or replayed node: "+node.ID)
		nodes[node.ID] = node
		for _, artifact := range node.Output.Artifacts {
			body, exists := files[artifact.Path]
			check(exists && fmt.Sprintf("%x", sha256.Sum256(body)) == artifact.SHA256, "missing or changed artifact: "+artifact.Path)
		}
	}
	artifactPath := func(id, name string) string {
		found := ""
		for _, artifact := range nodes[id].Output.Artifacts {
			if strings.HasPrefix(artifact.Path, base+"nodes/"+id+"/") && path.Base(artifact.Path) == name {
				check(found == "", "ambiguous artifact: "+id+"/"+name)
				found = artifact.Path
			}
		}
		check(found != "", "missing declared artifact: "+id+"/"+name)
		return found
	}
	manifestPath := artifactPath(discovery, "manifest.json")
	evidence = append(evidence, manifestPath)
	var manifest struct {
		Categories []struct {
			Name   string `json:"name"`
			Values []int  `json:"values"`
		} `json:"categories"`
	}
	if json.Unmarshal(files[manifestPath], &manifest) != nil || len(manifest.Categories) < 3 || len(manifest.Categories) > 5 {
		return evidence, append(failures, "discovery did not produce 3..5 input categories")
	}
	manifestJSON, _ := json.Marshal(manifest)
	var discoveryOutput struct{ Stdout string }
	// Check the preview actually delivered to the parent and matched against its
	// journal above, not a later section of a possibly truncated stdout file.
	check(json.Unmarshal(nodes[discovery].Output.Value, &discoveryOutput) == nil && mixedManifestInText([]byte(discoveryOutput.Stdout), manifestJSON), "discovery receipt did not expose the original manifest to the parent")
	expected := map[string]mixedCategoryResult{}
	for _, category := range manifest.Categories {
		_, duplicate := expected[category.Name]
		check(!duplicate && strings.TrimSpace(category.Name) != "" && len(category.Values) >= 3 && len(category.Values) <= 6, "invalid discovered category")
		result := mixedCategoryResult{Category: category.Name, Count: len(category.Values)}
		for _, value := range category.Values {
			result.Sum += value
		}
		expected[category.Name] = result
	}
	check(len(nodes) == len(expected)+2, "Agent count was not chosen from discovered categories")
	categoryNodes, covered := map[string]bool{}, map[string]bool{}
	join := ""
	for _, spec := range call.Nodes {
		node := nodes[spec.ID]
		dir := base + "nodes/" + spec.ID + "/"
		var dependencies []workergraph.NodeState
		check(json.Unmarshal(files[dir+"dependencies.json"], &dependencies) == nil && len(dependencies) == len(spec.DependsOn), "missing frozen inputs: "+spec.ID)
		seen := map[string]bool{}
		for _, dep := range dependencies {
			check(!seen[dep.ID] && slices.Contains(spec.DependsOn, workergraph.Dependency{ID: dep.ID}) && mixedNodeReceiptEqual(dep, nodes[dep.ID]) && !node.StartedAt.Before(dep.FinishedAt), "dependency does not match its producer: "+spec.ID+"/"+dep.ID)
			seen[dep.ID] = true
		}
		if node.Kind == "function" {
			check(join == "", "unexpected extra command node")
			join = spec.ID
			continue
		}
		check(node.Kind == "agent" && spec.Kind == "agent", "unexpected graph node kind")
		var session struct {
			RunID             string `json:"run_id"`
			GraphKey          string `json:"graph_key"`
			NodeID            string `json:"node_id"`
			InputHash         string `json:"input_sha256"`
			History           []agent.Message
			ContextCheckpoint *agent.ContextCheckpoint `json:"context_checkpoint"`
			Result            string
		}
		check(json.Unmarshal(files[dir+"session.json"], &session) == nil && session.RunID == run && session.GraphKey == "sample-agents" && session.NodeID == spec.ID && len(session.InputHash) == 64 && session.ContextCheckpoint != nil && len(session.History) >= 4 && session.Result != "", "missing independent child Loop: "+spec.ID)
		tools := map[string]bool{}
		usedTool := false
		for _, message := range session.History {
			for _, block := range message.Content {
				if message.Role == "assistant" && block.Type == "tool_use" {
					check(block.Name != "run_graph" && block.Name != "finish_step" && block.Name != "graph_action", "child exceeded local capabilities: "+spec.ID)
					tools[block.ID] = true
				}
				usedTool = usedTool || message.Role == "user" && block.Type == "tool_result" && tools[block.ToolUseID] && !block.IsError
			}
		}
		check(usedTool, "child did not use local tools: "+spec.ID)
		if spec.ID == discovery {
			continue
		}
		check(len(spec.DependsOn) == 1 && spec.DependsOn[0] == (workergraph.Dependency{ID: discovery}), "category Agent is not bound to discovery: "+spec.ID)
		resultPath := artifactPath(spec.ID, "result.json")
		evidence = append(evidence, resultPath)
		var result mixedCategoryResult
		decodeErr := json.Unmarshal(files[resultPath], &result)
		want, exists := expected[result.Category]
		check(decodeErr == nil && exists && !covered[result.Category] && result == want, "category Agent result does not match original manifest: "+spec.ID)
		covered[result.Category], categoryNodes[spec.ID] = true, true
	}
	check(len(covered) == len(expected) && len(categoryNodes) == len(expected), "missing category Agent results")
	joinPath := base + "nodes/" + join + "/stdout.log"
	evidence = append(evidence, joinPath)
	var joined struct {
		Categories []mixedCategoryResult `json:"categories"`
		Count      int                   `json:"count"`
		Sum        int                   `json:"sum"`
	}
	check(join != "" && json.Unmarshal(files[joinPath], &joined) == nil && len(joined.Categories) == len(expected), "missing computed command join")
	seen, count, sum := map[string]bool{}, 0, 0
	for _, result := range joined.Categories {
		want, exists := expected[result.Category]
		check(exists && !seen[result.Category] && result == want, "join category differs from original data")
		seen[result.Category] = true
		count, sum = count+result.Count, sum+result.Sum
	}
	check(joined.Count == count && joined.Sum == sum, "join totals do not match categories")
	for _, spec := range call.Nodes {
		if spec.ID == join {
			check(len(spec.DependsOn) == len(categoryNodes), "join does not depend on every category Agent")
			for _, dep := range spec.DependsOn {
				check(categoryNodes[dep.ID] && !dep.Optional, "join dependency is not a required category Agent")
			}
		}
	}
	return evidence, failures
}

// A child may surround its complete JSON with a report, a fenced code block,
// or a label. Require an entire equivalent object, including every value;
// category names or a summary alone are not evidence of parent observation.
func mixedManifestInText(raw, manifest []byte) bool {
	decode := func(raw []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		err := decoder.Decode(&value)
		return value, err
	}
	want, err := decode(manifest)
	if err != nil {
		return false
	}
	for offset, b := range raw {
		if b == '{' {
			if value, err := decode(raw[offset:]); err == nil && reflect.DeepEqual(value, want) {
				return true
			}
		}
	}
	return false
}

func TestMixedManifestInTextRequiresCompleteEquivalentData(t *testing.T) {
	manifest := `{"categories":[{"name":"violet","values":[1,2,3]},{"name":"amber","values":[4,5,6]},{"name":"teal","values":[7,8,9007199254740993]}]}`
	wrapped := "Discovery complete.\n```json\n" + manifest + "\n```\nMANIFEST_JSON: " + manifest
	for _, test := range []struct {
		name, text string
		want       bool
	}{
		{"plain JSON", manifest, true},
		{"wrapped identical", wrapped, true},
		{"changed value", strings.ReplaceAll(wrapped, "9007199254740993", "9007199254740992"), false},
		{"summary only", "3 categories: violet, amber, teal; all checks passed", false},
		{"truncated object", "MANIFEST_JSON: " + manifest[:len(manifest)-4], false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := mixedManifestInText([]byte(test.text), []byte(manifest)); got != test.want {
				t.Fatalf("manifest observation=%v, want %v", got, test.want)
			}
		})
	}
}

func validateWorkerMixedGraphDelivery(state board.State, files map[string][]byte) []string {
	failures := []string{}
	check := func(ok bool, detail string) {
		if !ok {
			failures = append(failures, detail)
		}
	}
	var steps []board.Step
	for _, step := range state.Steps {
		if !staleRepairCompletionMarker(state, step) {
			steps = append(steps, step)
		}
	}
	if state.Graph.Project.Status != "completed" || state.Graph.Project.OrchestrationVersion != 1 || len(steps) != 1 || steps[0].Worker == nil || steps[0].Result == nil {
		return []string{"expected one completed authorized Step with an accepted result"}
	}
	step := steps[0]
	facts := map[string]board.FactRecord{}
	for _, fact := range state.FactRecords {
		facts[fact.ID] = fact
	}
	_, accepted := orchestrationSuccessfulRun(state.Graph.Project, step, *step.Worker, facts, files)
	check(accepted, "missing successful parent Loop and accepted fact")
	run := orchestrationRunID(*step.Worker)
	evidence, graphFailures := validateDynamicMixedGraph(run, files)
	failures = append(failures, graphFailures...)
	fact := facts[*step.Result]
	check(fact.Scope == "mixed-agent-v2", "accepted fact has wrong scope")
	for _, path := range evidence {
		body, exists := files[path]
		check(exists && slices.ContainsFunc(fact.Evidence, func(ref board.EvidenceRef) bool {
			return orchestrationEvidenceValid(ref, fact.RunID, files) && bytes.Equal(files[ref.Path], body)
		}), "accepted parent result omitted original node evidence: "+path)
	}
	check(slices.ContainsFunc(state.Goals, func(goal board.Goal) bool {
		return goal.ID == "goal" && goal.Status == "achieved" && goal.SupportValid && slices.Contains(goal.Sources, fact.ID) && state.ValidateFactSources(goal.Sources, true) == nil
	}), "root goal lacks valid support from the accepted Step")
	return failures
}

func dynamicMixedGraphFixture(t *testing.T) (workergraph.Checkpoint, []agent.Message) {
	t.Helper()
	checkpoint := workergraph.Checkpoint{RunID: "producer-run", Status: "succeeded"}
	start := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	specs := []mixedGraphNodeSpec{{ID: "discover", Kind: "agent", Task: "discover local inputs", Resources: []string{}}}
	for _, id := range []string{"discover", "violet", "amber", "teal"} {
		value, _ := json.Marshal(map[string]string{"stdout": id})
		checkpoint.Nodes = append(checkpoint.Nodes, workergraph.NodeState{ID: id, Kind: "agent", Status: "succeeded", Attempt: 1, InputSHA256: strings.Repeat("a", 64), StartedAt: start, FinishedAt: start.Add(time.Second), Output: workergraph.Output{Value: value}})
		if id != "discover" {
			specs = append(specs, mixedGraphNodeSpec{ID: id, Kind: "agent", Task: "process observed category " + id, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "discover"}}})
		}
	}
	var messages []agent.Message
	for round, count := range []int{1, len(specs)} {
		input, _ := json.Marshal(mixedGraphCall{Key: "sample-agents", Nodes: specs[:count]})
		receipt, _ := json.Marshal(map[string]any{"key": "sample-agents", "status": "succeeded", "nodes": checkpoint.Nodes[:count]})
		content, _ := json.Marshal(string(receipt))
		id := fmt.Sprintf("round-%d", round)
		messages = append(messages, agent.Message{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: id, Name: "run_graph", Input: input}}}, agent.Message{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: id, Content: content}}})
	}
	return checkpoint, messages
}

func TestMixedGraphJournalRequiresObservedDynamicExtension(t *testing.T) {
	for name, mutate := range map[string]func(*workergraph.Checkpoint, *[]agent.Message){
		"observed extension":    func(*workergraph.Checkpoint, *[]agent.Message) {},
		"preplanned full graph": func(_ *workergraph.Checkpoint, messages *[]agent.Message) { *messages = (*messages)[2:] },
		"extension before receipt": func(_ *workergraph.Checkpoint, messages *[]agent.Message) {
			(*messages)[1], (*messages)[2] = (*messages)[2], (*messages)[1]
		},
		"missing second receipt": func(_ *workergraph.Checkpoint, messages *[]agent.Message) { *messages = (*messages)[:3] },
		"replayed discovery":     func(checkpoint *workergraph.Checkpoint, _ *[]agent.Message) { checkpoint.Nodes[0].Attempt = 2 },
		"changed prior assignment": func(_ *workergraph.Checkpoint, messages *[]agent.Message) {
			var call mixedGraphCall
			_ = json.Unmarshal((*messages)[2].Content[0].Input, &call)
			call.Nodes[0].Task = "a different assignment"
			(*messages)[2].Content[0].Input, _ = json.Marshal(call)
		},
		"missing receipt node": func(_ *workergraph.Checkpoint, messages *[]agent.Message) {
			receipt, _ := json.Marshal(map[string]any{"key": "sample-agents", "status": "succeeded", "nodes": []workergraph.NodeState{}})
			(*messages)[3].Content[0].Content, _ = json.Marshal(string(receipt))
		},
	} {
		t.Run(name, func(t *testing.T) {
			checkpoint, messages := dynamicMixedGraphFixture(t)
			mutate(&checkpoint, &messages)
			var journal []byte
			for _, message := range messages {
				raw, _ := json.Marshal(agent.Event{Type: "message", Message: &message})
				journal = append(journal, append(raw, '\n')...)
			}
			discovery, call, err := mixedGraphJournal(journal, checkpoint)
			if name == "observed extension" {
				if err != nil || discovery != "discover" || len(call.Nodes) != 4 {
					t.Fatalf("valid dynamic graph rejected: %s %+v %v", discovery, call, err)
				}
			} else if err == nil {
				t.Fatal("invalid dynamic scheduling evidence accepted")
			}
		})
	}
}
