//go:build linux

package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"pwnmesh/internal/workergraph"
)

// A nonzero exit is not a rollback: the parent must retain the failed attempt
// and must not silently repeat its already-visible side effect.
func TestFailedCommandKeepsEffectsWithoutReplayingOrRunningRequiredConsumer(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	spec := commandGraphSpec{Key: "partial-effect", Nodes: []commandGraphNode{
		{ID: "write-then-fail", Command: `printf 'applied\n' >> "$PWNMESH_WORKSPACE/effects"; exit 7`, Resources: []string{"effects"}},
		{ID: "consumer", Command: `touch "$PWNMESH_WORKSPACE/consumer-ran"`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "write-then-fail"}}},
	}}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := commandGraphCall(t, context.Background(), job, dir, spec)
		if err == nil || result.Status != "failed" {
			t.Fatalf("failed graph was accepted on attempt %d: %+v %v", attempt, result, err)
		}
		failed, _ := commandNodeValue(t, result, "write-then-fail")
		consumer, _ := commandNodeValue(t, result, "consumer")
		if failed.Status != "failed" || consumer.Status != "blocked" {
			t.Fatalf("lost partial failure boundary: %+v", result)
		}
	}
	data, err := os.ReadFile(filepath.Join(job.Workspace, "effects"))
	if err != nil || string(data) != "applied\n" {
		t.Fatalf("failed side effect was erased or replayed: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(job.Workspace, "consumer-ran")); !os.IsNotExist(err) {
		t.Fatalf("required consumer ran with a failed dependency: %v", err)
	}
	reuse := commandGraphSpec{Key: "invalid-reuse", Nodes: []commandGraphNode{
		{ID: "failed-output", Kind: "reuse", Resources: []string{}, ReuseFrom: &commandGraphReuse{Key: spec.Key, Node: "write-then-fail"}},
	}}
	if _, err := commandGraphCall(t, context.Background(), job, dir, reuse); err == nil {
		t.Fatal("failed work was imported as a successful reusable result")
	}
}
