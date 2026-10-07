//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/workergraph"
)

func drainFailureSpec() commandGraphSpec {
	return commandGraphSpec{Key: "original", Parallelism: 2, Nodes: []commandGraphNode{
		{ID: "download", Command: `printf 'download\n' >> "$PWNMESH_WORKSPACE/effects"; printf '{"version":1}' > download.json; printf downloaded`, Resources: []string{}, Artifacts: []string{"download.json"}},
		{ID: "manifest", Kind: "agent", Task: "Inspect the download and retain manifest.json", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "download"}}, Inputs: []commandGraphInput{{Node: "download", Artifact: "download.json"}}, Artifacts: []string{"manifest.json"}},
		{ID: "integrity", Command: `while [ ! -f "$PWNMESH_WORKSPACE/manifest-started" ]; do sleep .01; done; printf 'deterministic integrity failure\n'; exit 3`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "download"}}, Inputs: []commandGraphInput{{Node: "download", Artifact: "download.json"}}},
		{ID: "join", Command: `touch "$PWNMESH_WORKSPACE/old-join-started"`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "manifest"}, {ID: "integrity"}}},
	}}
}

// Waiting for a durable failure, rather than sleeping for a guessed duration,
// ensures the Agent finishes only after its sibling has actually failed.
func waitForDrainFailure(ctx context.Context, runDir string) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := os.ReadFile(filepath.Join(runDir, "graph-tools", "original", "graph.json"))
		var checkpoint workergraph.Checkpoint
		if err == nil && json.Unmarshal(raw, &checkpoint) == nil {
			for _, node := range checkpoint.Nodes {
				if node.ID == "integrity" && node.Status == "failed" {
					return ctx.Err()
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func TestCommandGraphDrainsRunningAgentAfterFailureAndReusesCompletedBranches(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	var calls atomic.Int32
	opts := Options{RunDir: dir, graphProvider: func(id string) (agent.Provider, error) {
		if id != "manifest" {
			return nil, fmt.Errorf("unexpected Agent %s", id)
		}
		return scenarioProvider(func(ctx context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
			if calls.Add(1) == 1 {
				if err := os.WriteFile(filepath.Join(job.Workspace, "manifest-started"), []byte("ready"), 0600); err != nil {
					return agent.Message{}, err
				}
				if err := waitForDrainFailure(ctx, dir); err != nil {
					return agent.Message{}, err
				}
				return draftModelCall("manifest-output", "write", `{"path":"manifest.json","content":"{\"manifest\":true}"}`), nil
			}
			if _, err := dynamicToolReply(history, "manifest-output"); err != nil {
				return agent.Message{}, err
			}
			return agent.Text("assistant", "Retained manifest.json after inspecting the download."), nil
		}), nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source, err := mixedGraphCall(t, ctx, job, opts, drainFailureSpec())
	if err == nil || source.Status != "failed" || !strings.Contains(err.Error(), "command exited 3") {
		t.Fatalf("source did not retain the required failure: %+v %v", source, err)
	}
	download, _ := commandNodeValue(t, source, "download")
	manifest, _ := commandNodeValue(t, source, "manifest")
	integrity, _ := commandNodeValue(t, source, "integrity")
	join, _ := commandNodeValue(t, source, "join")
	if download.Status != "succeeded" || manifest.Status != "succeeded" || integrity.Status != "failed" || join.Status != "blocked" || calls.Load() != 2 || !manifest.FinishedAt.After(integrity.FinishedAt) {
		t.Fatalf("required failure cancelled an already-running Agent or dispatched blocked work: %+v calls=%d", source, calls.Load())
	}
	if _, err := os.Stat(filepath.Join(job.Workspace, "old-join-started")); !os.IsNotExist(err) {
		t.Fatal("original join ran after its required input failed")
	}
	repair := commandGraphSpec{Key: "repair", Nodes: []commandGraphNode{
		{ID: "downloaded", Kind: "reuse", Resources: []string{}, ReuseFrom: &commandGraphReuse{Key: "original", Node: "download"}},
		{ID: "manifest", Kind: "reuse", Resources: []string{}, ReuseFrom: &commandGraphReuse{Key: "original", Node: "manifest"}},
		{ID: "fixed", Command: `grep -q '"download.json"' "$PWNMESH_DEPENDENCIES" && grep -q '"manifest.json"' "$PWNMESH_DEPENDENCIES" && printf 'fixed\n' >> "$PWNMESH_WORKSPACE/effects" && printf '{"integrity":true}' > report.json`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "downloaded"}, {ID: "manifest"}}, Inputs: []commandGraphInput{{Node: "downloaded", Artifact: "download.json"}, {Node: "manifest", Artifact: "manifest.json"}}, Artifacts: []string{"report.json"}},
	}}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := mixedGraphCall(t, ctx, job, opts, repair)
		if err != nil || result.Status != "succeeded" || calls.Load() != 2 {
			t.Fatalf("repair attempt %d reran an Agent or failed: %+v %v calls=%d", attempt, result, err, calls.Load())
		}
	}
	effects, err := os.ReadFile(filepath.Join(job.Workspace, "effects"))
	if err != nil || string(effects) != "download\nfixed\n" {
		t.Fatalf("repair repeated successful work: %q %v", effects, err)
	}
}

func TestCommandGraphDrainStillHonorsParentDeadline(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	var failureObserved atomic.Bool
	var deadlineObserved atomic.Bool
	opts := Options{RunDir: dir, graphDeadline: time.Now().Add(2 * time.Second), graphProvider: func(string) (agent.Provider, error) {
		return scenarioProvider(func(ctx context.Context, _ []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
			if err := os.WriteFile(filepath.Join(job.Workspace, "manifest-started"), []byte("ready"), 0600); err != nil {
				return agent.Message{}, err
			}
			if err := waitForDrainFailure(ctx, dir); err != nil {
				return agent.Message{}, err
			}
			failureObserved.Store(true)
			<-ctx.Done()
			deadlineObserved.Store(errors.Is(ctx.Err(), context.DeadlineExceeded))
			return agent.Message{}, ctx.Err()
		}), nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source, err := mixedGraphCall(t, ctx, job, opts, drainFailureSpec())
	if err == nil || !failureObserved.Load() || !deadlineObserved.Load() {
		t.Fatalf("draining did not wait for the parent deadline: %+v %v failure=%v deadline=%v", source, err, failureObserved.Load(), deadlineObserved.Load())
	}
	manifest, _ := commandNodeValue(t, source, "manifest")
	if manifest.Status != "running" || !strings.Contains(manifest.Error, "interrupted") {
		t.Fatalf("deadline was converted into reusable completed work: %+v", manifest)
	}
	repair := commandGraphSpec{Key: "repair", Nodes: []commandGraphNode{{ID: "downloaded", Kind: "reuse", Resources: []string{}, ReuseFrom: &commandGraphReuse{Key: "original", Node: "download"}}}}
	if _, err := commandGraphCall(t, ctx, job, dir, repair); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("reused a source with an Agent interrupted by the deadline: %v", err)
	}
}
