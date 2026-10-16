//go:build linux

package dag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/workergraph"
)

func interruptedReuseSource(t *testing.T, dependent bool) (Scope, workergraph.Checkpoint) {
	t.Helper()
	scope := scopeFor(graphScope(t), t.TempDir())
	spec := Spec{Key: "interrupted", Parallelism: 1, Nodes: []Node{
		{ID: "a-good", Command: `printf once >> "$PWNMESH_WORKSPACE/effects"; printf original`, Resources: []string{}},
		{ID: "z-unknown", Kind: "agent", Task: "Operation interrupted after its effect", Resources: []string{}},
	}}
	if dependent {
		spec.Nodes[1].DependsOn = []workergraph.Dependency{{ID: "a-good"}}
	}
	raw, err := marshalCommandGraphCall(spec)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), scope, raw, &AgentExecutor{Run: func(context.Context, AgentNode) (workergraph.Output, error) {
		return workergraph.Output{}, workergraph.ErrInterrupted
	}})
	if !errors.Is(err, workergraph.ErrInterrupted) {
		t.Fatalf("source did not interrupt: %v", err)
	}
	raw, err = os.ReadFile(filepath.Join(scope.RunDir, "graph-tools", spec.Key, "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint workergraph.Checkpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal(err)
	}
	good, _ := commandNodeValue(t, checkpoint, "a-good")
	unknown, _ := commandNodeValue(t, checkpoint, "z-unknown")
	if checkpoint.Status != "interrupted" || good.Status != "succeeded" || unknown.Status != "running" {
		t.Fatalf("unexpected source: %+v", checkpoint)
	}
	return scope, checkpoint
}

func TestInterruptedGraphReusesIndependentSuccessWithoutReplayingUnknown(t *testing.T) {
	scope, original := interruptedReuseSource(t, false)
	sourcePath := filepath.Join(scope.RunDir, "graph-tools", "interrupted", "graph.json")
	before, _ := os.ReadFile(sourcePath)
	spec := Spec{Key: "recover", Nodes: []Node{{ID: "saved", Kind: "reuse", Resources: []string{}, ReuseFrom: &Reuse{Key: "interrupted", Node: "a-good"}}}}
	raw, _ := marshalCommandGraphCall(spec)
	for range 2 {
		reply, err := Run(context.Background(), scope, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			ObservedAt string                  `json:"observed_at"`
			VerifiedAt string                  `json:"verified_at"`
			Evidence   []Evidence              `json:"verified_evidence"`
			Nodes      []workergraph.NodeState `json:"nodes"`
		}
		if err := json.Unmarshal([]byte(reply), &response); err != nil {
			t.Fatal(err)
		}
		good, _ := commandNodeValue(t, original, "a-good")
		if len(response.Evidence) != 1 || response.VerifiedAt != response.ObservedAt {
			t.Fatalf("missing verification provenance: %s", reply)
		}
		ref := response.Evidence[0]
		if ref.ExecutionStartedAt != good.StartedAt.Format(time.RFC3339Nano) || ref.ExecutionFinishedAt != good.FinishedAt.Format(time.RFC3339Nano) {
			t.Fatalf("reuse acquired a new execution window: %+v", ref)
		}
		verified, err := time.Parse(time.RFC3339Nano, ref.VerifiedAt)
		if err != nil || verified.Before(good.FinishedAt) {
			t.Fatalf("invalid verification time: %+v %v", ref, err)
		}
		var output Output
		if json.Unmarshal(response.Nodes[0].Output.Value, &output) != nil || output.ReusedFrom == nil || output.ReusedFrom.ExecutionFinishedAt != ref.ExecutionFinishedAt {
			t.Fatalf("missing original reuse time: %s", reply)
		}
	}
	after, _ := os.ReadFile(sourcePath)
	effects, _ := os.ReadFile(filepath.Join(scope.Workspace, "effects"))
	if !bytes.Equal(before, after) || string(effects) != "once" {
		t.Fatal("recovery changed its source or replayed effects")
	}
}

func TestInterruptedGraphRejectsUnresolvedDependencyInfluence(t *testing.T) {
	scope, _ := interruptedReuseSource(t, true)
	spec := Spec{Key: "unsafe", Nodes: []Node{{ID: "saved", Kind: "reuse", Resources: []string{}, ReuseFrom: &Reuse{Key: "interrupted", Node: "a-good"}}}}
	if _, err := commandGraphCall(t, context.Background(), scope, scope.RunDir, spec); err == nil || !strings.Contains(err.Error(), "z-unknown") || !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("dependent unresolved operation accepted without recovery guidance: %v", err)
	}
}

func TestInterruptedReuseChecksWholeSuccessfulClosureAndResources(t *testing.T) {
	specs := map[string]Node{
		"source":  {ID: "source", Resources: []string{"/workspace/reports"}},
		"result":  {ID: "result", DependsOn: []workergraph.Dependency{{ID: "source"}}, Resources: []string{}},
		"unknown": {ID: "unknown", Resources: []string{"./reports/../reports/result.json"}},
	}
	checkpoint := workergraph.Checkpoint{Status: "interrupted", Nodes: []workergraph.NodeState{{ID: "source", Status: "succeeded"}, {ID: "result", Status: "succeeded"}, {ID: "unknown", Status: "running"}}}
	if err := validateInterruptedReuse(Scope{Workspace: "/workspace"}, checkpoint, specs, "result"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unresolved writer influencing an ancestor was ignored: %v", err)
	}
	specs["unknown"] = Node{ID: "unknown", Resources: []string{"/workspace/independent"}}
	if err := validateInterruptedReuse(Scope{Workspace: "/workspace"}, checkpoint, specs, "result"); err != nil {
		t.Fatalf("independent successful subgraph rejected: %v", err)
	}
}

func TestInterruptedImportReconcilesWithoutReplayingSource(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained-source", true: "changed-source"}[changed], func(t *testing.T) {
			scope, dir, _ := reuseSource(t, false)
			scope.RunDir = dir
			sourcePath := filepath.Join(dir, "graph-tools", "original", "graph.json")
			sourceBefore, err := os.ReadFile(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			spec := Spec{Key: "import", Nodes: []Node{{ID: "saved", Kind: "reuse", Resources: []string{}, ReuseFrom: &Reuse{Key: "original", Node: "download"}}}}
			if _, err := commandGraphCall(t, context.Background(), scope, dir, spec); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "graph-tools", spec.Key, "graph.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved workergraph.Checkpoint
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			// Recreate the durable boundary after intent but before receipt commit.
			saved.Status, saved.Nodes[0].Status = "interrupted", "running"
			saved.Nodes[0].Output = workergraph.Output{}
			saved.Nodes[0].FinishedAt = time.Time{}
			raw, err = json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if changed {
				if err := os.WriteFile(filepath.Join(dir, "graph-tools", "original", "nodes", "download", "stdout.log"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			resumed, err := commandGraphCall(t, context.Background(), scope, dir, spec)
			if changed {
				after, readErr := os.ReadFile(path)
				if err == nil || readErr != nil || !bytes.Equal(raw, after) {
					t.Fatalf("changed source reached recovery: err=%v read=%v changed=%t", err, readErr, !bytes.Equal(raw, after))
				}
			} else if err != nil || resumed.Status != "succeeded" || resumed.Nodes[0].Status != "succeeded" || resumed.Nodes[0].Attempt != 1 || resumed.Nodes[0].InputSHA256 != saved.Nodes[0].InputSHA256 {
				t.Fatalf("receipt import could not reconcile: %+v %v", resumed, err)
			}
			sourceAfter, _ := os.ReadFile(sourcePath)
			effects, _ := os.ReadFile(filepath.Join(scope.Workspace, "effects"))
			if !bytes.Equal(sourceBefore, sourceAfter) || string(effects) != "once\n" {
				t.Fatal("import recovery rewrote or replayed its source")
			}
		})
	}
}

func TestInterruptedCommandStillRequiresInspection(t *testing.T) {
	scope, dir, saved := reuseSource(t, false)
	saved.Status, saved.Nodes[0].Status = "interrupted", "running"
	saved.Nodes[0].Output = workergraph.Output{}
	saved.Nodes[0].FinishedAt = time.Time{}
	raw, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "graph-tools", "original", "graph.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Key: "original", Parallelism: 1, Nodes: []Node{{ID: "download", Command: `printf 'once\n' >> "$PWNMESH_WORKSPACE/effects"; printf '{"version":1}\n' > result.json; printf 'downloaded\n'`, Resources: []string{}, Artifacts: []string{"result.json"}}}}
	if _, err := commandGraphCall(t, context.Background(), scope, dir, spec); err == nil || !strings.Contains(err.Error(), "reconciliation required") {
		t.Fatalf("uncertain command was accepted: %v", err)
	}
	after, _ := os.ReadFile(path)
	effects, _ := os.ReadFile(filepath.Join(scope.Workspace, "effects"))
	if !bytes.Equal(raw, after) || string(effects) != "once\n" {
		t.Fatal("uncertain command checkpoint changed or effects replayed")
	}
}

func TestSnapshotAcceptsLargeRetainedDiagnostic(t *testing.T) {
	scope := scopeFor(graphScope(t), t.TempDir())
	diagnostic := strings.Repeat("x", (8<<20)+1)
	spec := Spec{Key: "large", Nodes: []Node{{ID: "agent", Kind: "agent", Task: "Observe", Resources: []string{}, Optional: true}}}
	raw, err := marshalCommandGraphCall(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), scope, raw, &AgentExecutor{Run: func(context.Context, AgentNode) (workergraph.Output, error) {
		return workergraph.Output{}, errors.New(diagnostic)
	}}); err != nil {
		t.Fatal(err)
	}
	saved, _, err := readCommandSnapshot(context.Background(), scope, spec.Key, filepath.Join(scope.RunDir, "graph-tools", spec.Key))
	if err != nil || len(saved.Nodes) != 1 || saved.Nodes[0].Error != diagnostic {
		t.Fatalf("inspection rejected an accepted checkpoint: %v", err)
	}
}
