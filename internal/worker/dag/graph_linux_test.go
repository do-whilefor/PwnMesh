//go:build linux

package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/workergraph"
)

func commandGraphCall(t *testing.T, ctx context.Context, job Scope, dir string, spec Spec) (workergraph.Checkpoint, error) {
	t.Helper()
	raw, err := marshalCommandGraphCall(spec)
	if err != nil {
		t.Fatal(err)
	}
	reply, callErr := runGraph(ctx, job, dir, raw)
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
func marshalCommandGraphCall(spec Spec) ([]byte, error) {
	type toolNode struct {
		Node
		Inputs []Input `json:"inputs"`
	}
	nodes := make([]toolNode, 0, len(spec.Nodes))
	for _, node := range spec.Nodes {
		inputs := node.Inputs
		if inputs == nil {
			inputs = []Input{}
		}
		nodes = append(nodes, toolNode{node, inputs})
	}
	return json.Marshal(struct {
		Spec
		Nodes []toolNode `json:"nodes"`
	}{spec, nodes})
}

func commandNodeValue(t *testing.T, checkpoint workergraph.Checkpoint, id string) (workergraph.NodeState, Output) {
	t.Helper()
	for _, node := range checkpoint.Nodes {
		if node.ID == id {
			var value Output
			if len(node.Output.Value) > 0 {
				if err := json.Unmarshal(node.Output.Value, &value); err != nil {
					t.Fatal(err)
				}
			}
			return node, value
		}
	}
	t.Fatalf("missing node %s", id)
	return workergraph.NodeState{}, Output{}
}

func TestGraphRejectsTrailingJSONBeforeEffects(t *testing.T) {
	for _, suffix := range []string{` {}`, ` garbage`} {
		t.Run(suffix, func(t *testing.T) {
			scope := scopeFor(graphScope(t), t.TempDir())
			raw := json.RawMessage(`{"key":"framing","nodes":[{"id":"child","kind":"agent","task":"Observe","resources":[],"inputs":[]}]}` + suffix)
			calls := 0
			_, err := Run(context.Background(), scope, raw, &AgentExecutor{Run: func(context.Context, AgentNode) (workergraph.Output, error) {
				calls++
				return workergraph.Output{}, errors.New("unexpected execution")
			}})
			if err == nil || calls != 0 {
				t.Fatalf("trailing input reached execution: err=%v calls=%d", err, calls)
			}
			if _, err := os.Stat(filepath.Join(scope.RunDir, "graph-tools")); !os.IsNotExist(err) {
				t.Fatal("invalid JSON created graph state")
			}
		})
	}
}

func TestCommandGraphJSONArtifactAndStdoutExcludeStderr(t *testing.T) {
	// Exercise the dependency file, array shape, artifact and log contract
	// through actual producer/consumer execution rather than tutorial wording.
	spec := Spec{Key: "json-artifact", Nodes: []Node{
		{ID: "left", Command: `printf '{"ok":true}\n' > result.json; cat result.json; printf 'warning\n' >&2`, Resources: []string{}, Artifacts: []string{"result.json"}},
		{ID: "consume-logs", Command: `python3 -c 'import json, os; deps=json.load(open(os.environ["PWNMESH_DEPENDENCIES"])); assert len(deps)==1 and deps[0]["id"]=="left"; output=deps[0]["output"]; assert "stdout.log" not in output.get("files", {}) and "stderr.log" not in output.get("files", {}); assert json.load(open(output["value"]["output_path"]))=={"ok":True}; assert open(output["value"]["stderr_path"]).read()=="warning\n"; print("consumed separate logs")'`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "left"}}},
	}}
	checkpoint, err := commandGraphCall(t, context.Background(), graphScope(t), t.TempDir(), spec)
	if err != nil || checkpoint.Status != "succeeded" {
		t.Fatalf("JSON producer failed: %+v %v", checkpoint, err)
	}
	consumer, consumed := commandNodeValue(t, checkpoint, "consume-logs")
	if consumer.Status != "succeeded" || consumed.Stdout != "consumed separate logs\n" {
		t.Fatalf("dependency full-log paths were not consumed: %+v %+v", consumer, consumed)
	}
	node, value := commandNodeValue(t, checkpoint, "left")
	log, err := os.ReadFile(value.OutputPath)
	if err != nil || string(log) != value.Stdout || string(log) != "{\"ok\":true}\n" {
		t.Fatalf("stdout was contaminated: %q %v", log, err)
	}
	diagnostic, err := os.ReadFile(value.StderrPath)
	if err != nil || string(diagnostic) != value.Stderr || value.Stderr != "warning\n" {
		t.Fatalf("stderr was lost: %q %v", diagnostic, err)
	}
	for _, artifact := range node.Output.Artifacts {
		if filepath.Base(artifact.Path) != "result.json" {
			continue
		}
		raw, err := os.ReadFile(artifact.Path)
		var result struct {
			OK bool `json:"ok"`
		}
		if err != nil || json.Unmarshal(raw, &result) != nil || !result.OK {
			t.Fatalf("dedicated JSON artifact is invalid: %q %v", raw, err)
		}
		return
	}
	t.Fatal("declared JSON artifact is missing from dependency output")
}

func TestCommandGraphParallelDependenciesAndIsolatedArtifacts(t *testing.T) {
	job, dir := graphScope(t), t.TempDir()
	spec := Spec{Key: "parallel", Parallelism: 2, Nodes: []Node{
		{ID: "left", Command: `touch ready; while [ ! -f ../right/ready ]; do sleep .01; done; printf 'left\n'`, Resources: []string{}},
		{ID: "right", Command: `touch ready; while [ ! -f ../left/ready ]; do sleep .01; done; printf 'right\n'`, Resources: []string{}},
		{ID: "join", Command: `test "$PWD" = "$PWNMESH_NODE_DIR" && test -d "$PWNMESH_WORKSPACE" && grep -q '"id":"left"' "$PWNMESH_DEPENDENCIES" && grep -q '"id":"right"' "$PWNMESH_DEPENDENCIES" && cat ../left/stdout.log ../right/stdout.log > joined.txt && cat joined.txt`, Resources: []string{}, Artifacts: []string{"joined.txt"}, DependsOn: []workergraph.Dependency{{ID: "left"}, {ID: "right"}}},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	checkpoint, err := commandGraphCall(t, ctx, job, dir, spec)
	if err != nil || checkpoint.Status != "succeeded" {
		t.Fatalf("parallel branches failed: %+v %v", checkpoint, err)
	}
	left, leftValue := commandNodeValue(t, checkpoint, "left")
	right, rightValue := commandNodeValue(t, checkpoint, "right")
	joined, value := commandNodeValue(t, checkpoint, "join")
	if !left.StartedAt.Before(right.FinishedAt) || !right.StartedAt.Before(left.FinishedAt) || joined.StartedAt.Before(left.FinishedAt) || joined.StartedAt.Before(right.FinishedAt) {
		t.Fatalf("parallel execution/dependency ordering wrong: %+v", checkpoint)
	}
	if leftValue.OutputPath == rightValue.OutputPath || value.Stdout != "left\nright\n" || len(joined.Output.Artifacts) != 3 {
		t.Fatalf("isolated outputs/merge wrong: %+v", checkpoint)
	}
	if _, err := os.Stat(filepath.Join(job.Workspace, "ready")); !os.IsNotExist(err) {
		t.Fatal("node command wrote in shared project directory")
	}
	for _, node := range checkpoint.Nodes {
		if node.Attempt != 1 || !filepath.IsAbs(node.Output.Artifacts[0].Path) {
			t.Fatalf("missing stable node attempt/output: %+v", node)
		}
	}
	// Reuse validates all artifacts without re-running the commands.
	again, err := commandGraphCall(t, context.Background(), job, dir, spec)
	if err != nil || again.Status != "succeeded" {
		t.Fatalf("completed graph not reused: %+v %v", again, err)
	}
	for i, node := range again.Nodes {
		if !node.StartedAt.Equal(checkpoint.Nodes[i].StartedAt) {
			t.Fatal("completed command was replayed")
		}
	}
	if err := os.WriteFile(joined.Output.Artifacts[1].Path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := commandGraphCall(t, context.Background(), job, dir, spec); err == nil || !strings.Contains(err.Error(), "SHA-256 changed") {
		t.Fatalf("changed artifact reused: %v", err)
	}
}

func TestCommandGraphReceiptRechecksUpstreamFilesAfterDownstreamChanges(t *testing.T) {
	for name, command := range map[string]string{
		"rewrite":            `printf altered > ../source/stdout.log`,
		"delete":             `rm ../source/stdout.log`,
		"large-rewrite":      `head -c 9000 /dev/zero | tr '\0' x > ../source/stdout.log`,
		"large-tail-rewrite": `printf y | dd of=../source/stdout.log bs=1 seek=8999 conv=notrunc status=none`,
		"symlink":            `rm ../source/stdout.log; ln -s ../source/dependencies.json ../source/stdout.log`,
	} {
		t.Run(name, func(t *testing.T) {
			j, dir := graphScope(t), t.TempDir()
			sourceCommand := `printf original`
			if name == "large-tail-rewrite" {
				sourceCommand = `head -c 9000 /dev/zero | tr '\0' x`
			}
			spec := Spec{Key: "changed-upstream", Nodes: []Node{
				{ID: "source", Command: sourceCommand, Resources: []string{"source-stdout"}},
				{ID: "rewrite", Command: command + `; printf downstream`, Resources: []string{"source-stdout"}, DependsOn: []workergraph.Dependency{{ID: "source"}}},
			}}
			raw, _ := json.Marshal(spec)
			result, err := runGraph(context.Background(), j, dir, raw)
			if err != nil {
				t.Fatal(err)
			}
			var receipt struct {
				Evidence []Evidence        `json:"verified_evidence"`
				Errors   map[string]string `json:"evidence_errors"`
			}
			if err := json.Unmarshal([]byte(result), &receipt); err != nil {
				t.Fatal(err)
			}
			if len(receipt.Evidence) != 1 || receipt.Evidence[0].NodeID != "rewrite" || receipt.Errors["source"] == "" {
				t.Fatalf("changed upstream was still certified: %+v", receipt)
			}
		})
	}
}

func TestCommandGraphConditionsAndOptionalFailureJoin(t *testing.T) {
	job, dir := graphScope(t), t.TempDir()
	spec := Spec{Key: "conditional", Nodes: []Node{
		{ID: "source", Command: `printf skip`, Resources: []string{}},
		{ID: "skip", Command: `touch forbidden`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source"}}, When: &When{Node: "source", Contains: "run"}},
		{ID: "failed", Command: `printf 'failure evidence'; exit 7`, Resources: []string{}, Optional: true},
		{ID: "join", Command: `grep -q '"status":"skipped"' "$PWNMESH_DEPENDENCIES" && grep -q '"status":"failed"' "$PWNMESH_DEPENDENCIES" && printf joined`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "skip", Optional: true}, {ID: "failed", Optional: true}}},
	}}
	checkpoint, err := commandGraphCall(t, context.Background(), job, dir, spec)
	if err != nil || checkpoint.Status != "succeeded" {
		t.Fatalf("optional join failed: %+v %v", checkpoint, err)
	}
	skipped, _ := commandNodeValue(t, checkpoint, "skip")
	failed, _ := commandNodeValue(t, checkpoint, "failed")
	_, joined := commandNodeValue(t, checkpoint, "join")
	if skipped.Status != "skipped" || skipped.Reason == "" || failed.Status != "failed" || !strings.Contains(failed.Error, "stdout.log") || joined.Stdout != "joined" {
		t.Fatalf("condition/failure results lost: %+v", checkpoint)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph-tools", "conditional", "nodes", "skip")); !os.IsNotExist(err) {
		t.Fatal("skipped node performed side effects")
	}
}

func TestCommandGraphRejectsUnreachableFailureRoutesBeforeExecution(t *testing.T) {
	for _, test := range []struct {
		name, status               string
		optionalNode, optionalEdge bool
	}{
		{name: "failed-required-edge", status: "failed", optionalNode: true},
		{name: "skipped-required-edge", status: "skipped", optionalNode: true},
		{name: "failed-required-node", status: "failed", optionalEdge: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			job, dir := graphScope(t), t.TempDir()
			spec := Spec{Key: "unreachable", Nodes: []Node{
				{ID: "source", Command: `touch started; exit 7`, Resources: []string{}, Optional: test.optionalNode},
				{ID: "route", Command: `touch forbidden`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source", Optional: test.optionalEdge}}, When: &When{Node: "source", Status: test.status}},
			}}
			if test.status == "skipped" {
				spec.Nodes[0].DependsOn = []workergraph.Dependency{{ID: "seed"}}
				spec.Nodes[0].When = &When{Node: "seed", Contains: "absent"}
				spec.Nodes = append(spec.Nodes, Node{ID: "seed", Command: "printf skip", Resources: []string{}})
			}
			if _, err := commandGraphCall(t, context.Background(), job, dir, spec); err == nil || !strings.Contains(err.Error(), "when") {
				t.Fatalf("unreachable route was not rejected during preflight: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "graph-tools", spec.Key)); !os.IsNotExist(err) {
				t.Fatalf("invalid route created a graph or executed its source: %v", err)
			}
		})
	}
}

func TestCommandGraphSkippedRouteDoesNotRequireOptionalSource(t *testing.T) {
	job, dir := graphScope(t), t.TempDir()
	spec := Spec{Key: "skip-route", Nodes: []Node{
		{ID: "seed", Command: "printf skip", Resources: []string{}},
		{ID: "source", Command: "exit 99", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "seed"}}, When: &When{Node: "seed", Contains: "absent"}},
		{ID: "route", Command: "printf routed", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source", Optional: true}}, When: &When{Node: "source", Status: "skipped"}},
	}}
	for range 2 {
		checkpoint, err := commandGraphCall(t, context.Background(), job, dir, spec)
		if err != nil || checkpoint.Status != "succeeded" {
			t.Fatalf("reachable skip route was rejected: %+v %v", checkpoint, err)
		}
		source, _ := commandNodeValue(t, checkpoint, "source")
		route, output := commandNodeValue(t, checkpoint, "route")
		if source.Status != "skipped" || source.Attempt != 0 || route.Status != "succeeded" || route.Attempt != 1 || output.Stdout != "routed" {
			t.Fatalf("skip route did not retain its successful single execution: %+v", checkpoint)
		}
	}
}

func TestCommandGraphContainsUsesCompleteRetainedOutput(t *testing.T) {
	for _, test := range []struct {
		name, command, contains, status string
	}{
		{"after-preview", `head -c 9000 /dev/zero | tr '\0' x; printf marker`, "marker", "succeeded"},
		{"across-chunks", `head -c 65534 /dev/zero | tr '\0' x; printf marker`, "marker", "succeeded"},
		{"preview-notice", `head -c 9000 /dev/zero | tr '\0' x`, "Preview truncated", "skipped"},
		{"invalid-utf8", `printf '\377'`, "�", "skipped"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job, dir := graphScope(t), t.TempDir()
			spec := Spec{Key: "full-output", Nodes: []Node{
				{ID: "source", Command: test.command, Resources: []string{}},
				{ID: "route", Command: `printf routed`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source"}}, When: &When{Node: "source", Contains: test.contains}},
			}}
			checkpoint, err := commandGraphCall(t, context.Background(), job, dir, spec)
			if err != nil || checkpoint.Status != "succeeded" {
				t.Fatalf("condition failed: %+v %v", checkpoint, err)
			}
			route, value := commandNodeValue(t, checkpoint, "route")
			if route.Status != test.status || test.status == "succeeded" && value.Stdout != "routed" {
				t.Fatalf("condition did not use exact full output: %+v", route)
			}
			if test.status == "skipped" {
				if _, err := os.Stat(filepath.Join(dir, "graph-tools", spec.Key, "nodes", "route")); !os.IsNotExist(err) {
					t.Fatal("unmatched condition performed side effects")
				}
			}
			// Reuse retains the decision and never re-executes the branch.
			again, err := commandGraphCall(t, context.Background(), job, dir, spec)
			if err != nil || again.Status != "succeeded" {
				t.Fatalf("condition could not be reused: %+v %v", again, err)
			}
			resumed, _ := commandNodeValue(t, again, "route")
			if resumed.Status != route.Status || !resumed.StartedAt.Equal(route.StartedAt) {
				t.Fatalf("condition was replayed: before=%+v after=%+v", route, resumed)
			}
		})
	}
}

func TestCommandGraphContainsRejectsChangedDependencyBeforeDispatch(t *testing.T) {
	for name, command := range map[string]string{
		"matching-replacement": `printf marker > ../source/stdout.log`,
		"tail-after-match":     `printf y | dd of=../source/stdout.log bs=1 seek=8999 conv=notrunc status=none`,
	} {
		t.Run(name, func(t *testing.T) {
			job, dir := graphScope(t), t.TempDir()
			spec := Spec{Key: "changed-condition", Nodes: []Node{
				{ID: "source", Command: `printf marker; head -c 9000 /dev/zero | tr '\0' x`, Resources: []string{"source-stdout"}},
				{ID: "rewrite", Command: command, Resources: []string{"source-stdout"}, DependsOn: []workergraph.Dependency{{ID: "source"}}},
				{ID: "route", Command: `touch forbidden`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source"}, {ID: "rewrite"}}, When: &When{Node: "source", Contains: "marker"}},
			}}
			checkpoint, err := commandGraphCall(t, context.Background(), job, dir, spec)
			if err == nil || !strings.Contains(err.Error(), "condition dependency stdout") || checkpoint.Status != "failed" {
				t.Fatalf("changed dependency authorized condition: %+v %v", checkpoint, err)
			}
			route, _ := commandNodeValue(t, checkpoint, "route")
			if route.Status != "failed" || route.Attempt != 0 {
				t.Fatalf("condition started an unsafe operation: %+v", route)
			}
			if _, err := os.Stat(filepath.Join(dir, "graph-tools", spec.Key, "nodes", "route")); !os.IsNotExist(err) {
				t.Fatal("changed dependency allowed downstream side effects")
			}
		})
	}
}

func TestCommandGraphCancellationAndUncertainRecoveryNeverReplay(t *testing.T) {
	job, dir := graphScope(t), t.TempDir()
	spec := Spec{Key: "interrupted", Nodes: []Node{{ID: "slow", Command: `printf start >> started; sleep 30; touch forbidden`, Resources: []string{}}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := commandGraphCall(t, ctx, job, dir, spec)
		done <- err
	}()
	nodeDir := filepath.Join(dir, "graph-tools", "interrupted", "nodes", "slow")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(nodeDir, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation not propagated: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled command did not exit")
	}
	if _, err := commandGraphCall(t, context.Background(), job, dir, spec); err == nil || !strings.Contains(err.Error(), "reconcil") {
		t.Fatalf("uncertain command replayed: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(nodeDir, "started"))
	if err != nil || string(raw) != "start" {
		t.Fatalf("uncertain side effect repeated: %s %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(nodeDir, "forbidden")); !os.IsNotExist(err) {
		t.Fatal("cancelled subprocess remained running")
	}
}

func TestCommandGraphDrainAndTimeoutStopSubprocesses(t *testing.T) {
	for _, kind := range []string{"required-failure", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			job, dir := graphScope(t), t.TempDir()
			spec := Spec{Key: kind, Parallelism: 2, Nodes: []Node{{ID: "slow", Command: "touch ready; sleep 30; touch forbidden", Resources: []string{}}}}
			if kind == "timeout" {
				spec.Nodes[0].Timeout = 1
			} else {
				spec.Nodes = append(spec.Nodes, Node{ID: "fail", Command: "while [ ! -f ../slow/ready ]; do sleep .01; done; exit 7", Resources: []string{}})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			checkpoint, err := commandGraphCall(t, ctx, job, dir, spec)
			if err == nil || checkpoint.Status != "failed" {
				t.Fatalf("failed graph lost its terminal failure: %+v %v", checkpoint, err)
			}
			if kind == "required-failure" {
				// The started sibling is drained until the parent deadline,
				// then joined without replaying its uncertain side effects.
				slow, _ := commandNodeValue(t, checkpoint, "slow")
				if !errors.Is(ctx.Err(), context.DeadlineExceeded) || slow.Status != "running" {
					t.Fatalf("required failure did not drain until cancellation: %+v parent=%v", checkpoint, ctx.Err())
				}
			} else if ctx.Err() != nil {
				t.Fatalf("node timeout did not promptly stop: %+v %v", checkpoint, err)
			}
			if !strings.Contains(err.Error(), "stdout.log") {
				t.Fatalf("failed command lost its diagnostic output path: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "graph-tools", kind, "nodes", "slow", "forbidden")); !os.IsNotExist(err) {
				t.Fatal("stopped child command completed its late side effect")
			}
			groups, err := filepath.Glob(filepath.Join(dir, "group-*"))
			if err != nil || len(groups) != 0 {
				t.Fatalf("tool processes were not collected: %v %v", groups, err)
			}
		})
	}
}

func TestCommandGraphValidationBeforeExecution(t *testing.T) {
	for name, mutate := range map[string]func(*Spec){
		"traversal key":      func(s *Spec) { s.Key = "../escape" },
		"traversal node":     func(s *Spec) { s.Nodes[0].ID = ".." },
		"artifact traversal": func(s *Spec) { s.Nodes[0].Artifacts = []string{"../escape"} },
		"reserved artifact":  func(s *Spec) { s.Nodes[0].Artifacts = []string{"stdout.log"} },
		"parallelism":        func(s *Spec) { s.Parallelism = 17 },
		"resource conflict": func(s *Spec) {
			s.Nodes[0].Resources, s.Nodes[1].Resources = []string{"report"}, []string{"report"}
		},
		"cycle": func(s *Spec) {
			s.Nodes[0].DependsOn, s.Nodes[1].DependsOn = []workergraph.Dependency{{ID: "b"}}, []workergraph.Dependency{{ID: "a"}}
		},
		"unknown dependency": func(s *Spec) { s.Nodes[0].DependsOn = []workergraph.Dependency{{ID: "unknown"}} },
		"unknown condition":  func(s *Spec) { s.Nodes[0].When = &When{Node: "b", Contains: "run"} },
	} {
		t.Run(name, func(t *testing.T) {
			spec := Spec{Key: "invalid", Nodes: []Node{{ID: "a", Command: "touch forbidden", Resources: []string{}}, {ID: "b", Command: "true", Resources: []string{}}}}
			mutate(&spec)
			if err := validateCommandGraph(&spec); err == nil {
				t.Fatal("invalid graph accepted")
			}
		})
	}
	spec := Spec{Key: "ordered", Nodes: []Node{{ID: "a", Command: "true", Resources: []string{"report"}}, {ID: "b", Command: "true", Resources: []string{"report"}, DependsOn: []workergraph.Dependency{{ID: "a"}}}}}
	if err := validateCommandGraph(&spec); err != nil {
		t.Fatalf("explicit resource order rejected: %v", err)
	}
}

func TestCommandGraphRejectsRedirectedDirectoriesAndArtifacts(t *testing.T) {
	for _, kind := range []string{"directory", "artifact"} {
		t.Run(kind, func(t *testing.T) {
			job, dir := graphScope(t), t.TempDir()
			spec := Spec{Key: "symlink", Nodes: []Node{{ID: "a", Command: `ln -s "$PWNMESH_WORKSPACE" linked; printf evidence > "$PWNMESH_WORKSPACE/evidence.txt"`, Resources: []string{}, Artifacts: []string{"linked/evidence.txt"}}}}
			if kind == "directory" {
				if err := os.Symlink(job.Workspace, filepath.Join(dir, "graph-tools")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := commandGraphCall(t, context.Background(), job, dir, spec); err == nil {
				t.Fatal("symlink redirection accepted")
			}
		})
	}
}

func TestCommandGraphKeyBindsDefinitionAndPreviewIsBounded(t *testing.T) {
	job, dir := graphScope(t), t.TempDir()
	spec := Spec{Key: "bound", Nodes: []Node{{ID: "a", Command: `head -c 100000 /dev/zero | tr '\0' x`, Resources: []string{}}}}
	checkpoint, err := commandGraphCall(t, context.Background(), job, dir, spec)
	if err != nil {
		t.Fatal(err)
	}
	_, value := commandNodeValue(t, checkpoint, "a")
	info, err := os.Stat(value.OutputPath)
	if err != nil || info.Size() != 100000 || len(value.Stdout) > 8100 || !strings.Contains(value.Stdout, "truncated") {
		t.Fatalf("output was not bounded/preserved: %d %v", len(value.Stdout), err)
	}
	spec.Nodes[0].Command = "touch forbidden"
	if _, err := commandGraphCall(t, context.Background(), job, dir, spec); err == nil {
		t.Fatalf("changed definition reused a key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph-tools", spec.Key, "nodes", "a", "forbidden")); !os.IsNotExist(err) {
		t.Fatal("changed node command executed")
	}
}

func TestCommandGraphRejectsCheckpointPreviewTampering(t *testing.T) {
	job, dir := graphScope(t), t.TempDir()
	spec := Spec{Key: "preview", Nodes: []Node{{ID: "a", Command: "printf original", Resources: []string{}}}}
	if _, err := commandGraphCall(t, context.Background(), job, dir, spec); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "graph-tools", "preview", "graph.json")
	raw, err := os.ReadFile(path)
	var checkpoint workergraph.Checkpoint
	if err != nil || json.Unmarshal(raw, &checkpoint) != nil {
		t.Fatal("missing command graph checkpoint")
	}
	var value Output
	if err := json.Unmarshal(checkpoint.Nodes[0].Output.Value, &value); err != nil {
		t.Fatal(err)
	}
	value.Stdout = "forged condition input"
	checkpoint.Nodes[0].Output.Value, _ = json.Marshal(value)
	raw, _ = json.Marshal(checkpoint)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := commandGraphCall(t, context.Background(), job, dir, spec); err == nil || !strings.Contains(err.Error(), "stdout preview") {
		t.Fatalf("unbound checkpoint preview reused: %v", err)
	}
}

func TestCommandGraphBoundsWrittenFileSizes(t *testing.T) {
	job, dir := graphScope(t), t.TempDir()
	spec := Spec{Key: "file-limit", Nodes: []Node{{ID: "a", Command: "head -c 67108865 /dev/zero", Resources: []string{}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checkpoint, err := commandGraphCall(t, ctx, job, dir, spec)
	if err == nil || checkpoint.Status != "failed" {
		t.Fatalf("oversized stdout was not rejected: %+v %v", checkpoint, err)
	}
	info, err := os.Stat(filepath.Join(dir, "graph-tools", "file-limit", "nodes", "a", "stdout.log"))
	if err != nil || info.Size() > 64<<20 {
		t.Fatalf("command exceeded output file limit: %v %v", info, err)
	}
}

func TestCommandGraphCapacityAndAutomaticParallelism(t *testing.T) {
	for _, count := range []int{1, 5, 17, 64} {
		spec := Spec{Key: "capacity"}
		for i := 0; i < count; i++ {
			spec.Nodes = append(spec.Nodes, Node{ID: fmt.Sprintf("node-%02d", i), Command: "true", Resources: []string{}})
		}
		for i := 0; i < count-1; i++ {
			spec.Nodes[count-1].DependsOn = append(spec.Nodes[count-1].DependsOn, workergraph.Dependency{ID: spec.Nodes[i].ID})
		}
		if err := validateCommandGraph(&spec); err != nil || spec.Parallelism != min(count, 16) {
			t.Fatalf("automatic concurrency for %d nodes: %d, %v", count, spec.Parallelism, err)
		}
		if count == 64 {
			spec.Nodes = append(spec.Nodes, Node{ID: "overflow", Command: "true", Resources: []string{}})
			if validateCommandGraph(&spec) == nil {
				t.Fatal("runtime accepted an oversized cumulative graph")
			}
		}
	}
}
func TestMixedGraphRejectsAmbiguousCapabilitiesBeforeExecution(t *testing.T) {
	for _, node := range []Node{
		{ID: "bad", Kind: "agent", Task: "task", Command: "touch outside", Resources: []string{}},
		{ID: "bad", Kind: "command", Task: "task", Resources: []string{}},
		{ID: "bad", Kind: "agent", Resources: []string{}},
		{ID: "bad", Kind: "agent", Task: "task", Resources: []string{}, Artifacts: []string{"session.json"}},
	} {
		spec := Spec{Key: "invalid", Nodes: []Node{node}}
		if err := validateCommandGraph(&spec); err == nil {
			t.Fatalf("accepted ambiguous node: %+v", node)
		}
	}
}
