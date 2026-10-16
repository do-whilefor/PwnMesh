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
