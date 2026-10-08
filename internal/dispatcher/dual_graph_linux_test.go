//go:build linux

package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
	"pwnmesh/internal/workergraph"
)

func dualGraphObservation(f *updateRequestFixture, run *task, description string) string {
	f.t.Helper()
	var receipt board.StateActionResult
	f.do("POST", projectPath(f.project.ID)+"/state/actions", map[string]any{
		"op": "fact", "idempotency_key": run.Job.RunID + ":observation",
		"payload": map[string]any{
			"description": description, "scope": "synthetic local fixture", "observed_at": time.Now().UTC().Format(time.RFC3339),
			"evidence": []board.EvidenceRef{{RunID: run.Job.RunID, Path: "retained/response.txt", Excerpt: description}},
			"assets":   []board.AssetSpec{{Kind: "endpoint", Value: "https://example.test/local", Method: "GET"}},
		},
	}, &receipt, &run.Lease)
	return receipt.ID
}

// This uses the real command DAG, named artifact delivery, Worker journal,
// Dispatcher bridge, HTTP and SQLite. Only the model and container launch are
// replaced; graph changes happen while an actual command process is waiting.
func TestDualGraphSeparatesAssetDiscoveryCorrectionAndStepAcceptance(t *testing.T) {
	for _, change := range []string{"same_asset", "refuted_source"} {
		t.Run(change, func(t *testing.T) {
			f := newUpdateRequestFixture(t)
			producer := f.prepare("explore", []string{"origin"}, "Initial local observation")
			f.start(producer)
			source := dualGraphObservation(f, producer, updateOriginal)
			run := f.prepare("explore", []string{source}, "Compute from the initial observation")
			f.start(run)
			runDir := t.TempDir()
			var requests [][]agent.Message
			f.runner.options = worker.Options{RunDir: runDir, Provider: updateProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				requests = append(requests, updateHistoryCopy(history))
				if len(requests) == 1 {
					return updateToolCall("real-dag", "run_graph", dualGraphSpec), nil
				}
				if len(requests) != 2 {
					return agent.Message{}, fmt.Errorf("unexpected extra model turn: %s", updateHistoryText(history))
				}
				payload, _ := json.Marshal(map[string]any{"fact": map[string]any{
					"description": "The local computation produced 14", "scope": "local synthetic files", "observed_at": time.Now().UTC().Format(time.RFC3339Nano),
					"evidence": []map[string]any{{"path": filepath.Join(runDir, "graph-tools", "boundary", "nodes", "join", "stdout.log")}},
				}})
				return updateToolCall("finish", "finish_step", string(payload)), nil
			})}
			done := startDualGraphRun(t, f.ctx, func(ctx context.Context) updateRunResult {
				result, err := f.runner.Run(ctx, run.Worker, run.Job)
				return updateRunResult{result, err}
			})
			awaitDualGraphProcess(t, f, done)
			var changedFact string
			if change == "refuted_source" {
				changedFact = f.correct(source)
			} else {
				observer := f.prepare("explore", []string{"origin"}, "A different observation of the same endpoint")
				f.start(observer)
				changedFact = dualGraphObservation(f, observer, "Same asset, independent observation")
			}
			// Neither a same-asset observation nor a correction is an explicit
			// revocation of a running process. Its result is checked separately.
			if err := f.scheduler.renewLease(f.ctx, run); err != nil {
				t.Fatalf("graph update revoked the active command lease: %v", err)
			}
			if err := os.WriteFile(filepath.Join(f.workspace, "dual-graph-release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			result := f.await(done)
			if result.err != nil || result.result.Status != "success" || len(requests) != 2 {
				t.Fatalf("real DAG/Worker failed: %+v %v requests=%d", result.result, result.err, len(requests))
			}
			var graph struct {
				Status string                        `json:"status"`
				Nodes  []struct{ ID, Status string } `json:"nodes"`
			}
			if err := json.Unmarshal([]byte(updateResultText(requests[1], "real-dag")), &graph); err != nil || graph.Status != "succeeded" || len(graph.Nodes) != 2 {
				t.Fatalf("missing real DAG completion: %+v %v", graph, err)
			}
			for _, node := range graph.Nodes {
				if node.Status != "succeeded" {
					t.Fatalf("DAG succeeded with an unfinished node: %+v", node)
				}
			}
			for _, id := range []string{"seed", "join"} {
				if !slices.ContainsFunc(graph.Nodes, func(node struct{ ID, Status string }) bool { return node.ID == id }) {
					t.Fatalf("missing DAG node %s", id)
				}
			}
			seed, err := os.ReadFile(filepath.Join(runDir, "graph-tools", "boundary", "nodes", "seed", "value.json"))
			if err != nil || string(seed) != "7\n" {
				t.Fatalf("missing original named artifact: %q %v", seed, err)
			}
			computed, err := os.ReadFile(filepath.Join(runDir, "graph-tools", "boundary", "nodes", "join", "stdout.log"))
			if err != nil || string(computed) != "14\n" {
				t.Fatalf("named dependency artifact was not consumed: %q %v", computed, err)
			}
			if change == "refuted_source" {
				assertUpdateDelivered(t, requests[1], source, changedFact, "real-dag")
			} else if strings.Contains(updateHistoryText(requests[1]), "Shared graph update:") || strings.Contains(updateHistoryText(requests[1]), "Same asset, independent observation") {
				t.Fatal("asset association was treated as an execution dependency")
			}
			var before board.State
			f.do("GET", projectPath(f.project.ID)+"/state", nil, &before, nil)
			for _, fact := range before.FactRecords {
				if fact.SourceStepID == run.Job.Intent.ID {
					t.Fatal("DAG or local handoff automatically published a shared fact")
				}
			}
			if change == "same_asset" && (len(before.AssetIDs("fact", source)) != 1 || !slices.Equal(before.AssetIDs("fact", source), before.AssetIDs("fact", changedFact))) {
				t.Fatal("fixture did not associate both observations with the same asset")
			}
			frozen, err := f.runner.handler(f.ctx, run.Job, worker.GraphRequest{RequestID: strings.Repeat("d", 32), Op: "read_snapshot", Section: "facts", IDs: []string{source, changedFact}, ExpectedVersion: run.Job.InputSnapshot.StateVersion})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(frozen)
			var page struct {
				Items        []board.FactRecord `json:"items"`
				StateVersion string             `json:"state_version"`
			}
			if json.Unmarshal(raw, &page) != nil || page.StateVersion != run.Job.InputSnapshot.StateVersion || len(page.Items) != 1 || page.Items[0].ID != source || page.Items[0].Status != "valid" {
				t.Fatalf("DAG execution mixed live changes into its original snapshot: %s", raw)
			}
			f.do("POST", executionPath(run)+"/status", map[string]any{"status": "result_pending", "result": result.result}, nil, &run.Lease)
			err = f.scheduler.Client.Do(f.ctx, "POST", executionPath(run)+"/apply", map[string]any{}, nil, &run.Lease)
			if change == "same_asset" && err != nil {
				t.Fatalf("unrelated asset observation blocked acceptance: %v", err)
			}
			if change == "refuted_source" {
				var protocol *ProtocolError
				if !errors.As(err, &protocol) || protocol.Status != 409 || !strings.Contains(err.Error(), "not effective evidence") {
					t.Fatalf("successful DAG bypassed exploration dependency validation: %v", err)
				}
			}
			var after board.State
			f.do("GET", projectPath(f.project.ID)+"/state", nil, &after, nil)
			foundStep := false
			for _, step := range after.Steps {
				foundStep = foundStep || step.ID == run.Job.Intent.ID
				if step.ID == run.Job.Intent.ID && ((step.Result != nil) != (change == "same_asset")) {
					t.Fatalf("Step acceptance confused execution success with valid support: %+v", step)
				}
			}
			if !foundStep {
				t.Fatal("assigned Step disappeared")
			}
			wantFacts := len(before.FactRecords)
			if change == "same_asset" {
				wantFacts++
			}
			if len(after.FactRecords) != wantFacts {
				t.Fatal("rejected completion leaked a Fact or accepted completion lost its observation")
			}
			if change == "same_asset" {
				fact := after.FactRecords[len(after.FactRecords)-1]
				if fact.SourceStepID != run.Job.Intent.ID || fact.RunID != run.Lease.Run || len(fact.Evidence) != 1 || fact.Evidence[0].RunID != run.Job.RunID || fact.Evidence[0].Excerpt != string(computed) {
					t.Fatalf("accepted Fact lost the original DAG evidence: %+v", fact)
				}
				retained, err := os.ReadFile(fact.Evidence[0].Path)
				if err != nil || string(retained) != string(computed) {
					t.Fatalf("accepted evidence bytes differ: %q %v", retained, err)
				}
			}
		})
	}
}

func startDualGraphRun(t *testing.T, parent context.Context, execute func(context.Context) updateRunResult) <-chan updateRunResult {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done, finished := make(chan updateRunResult, 1), make(chan struct{})
	// Registered after TempDir cleanups, so a failed assertion cancels and
	// joins the Worker before any of its files or database are removed.
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("DAG Worker did not exit during cleanup")
		}
	})
	go func() { defer close(finished); done <- execute(ctx) }()
	return done
}

const dualGraphSpec = `{"key":"boundary","nodes":[{"id":"seed","command":"echo $$ > \"$PWNMESH_WORKSPACE/dual-graph-started\"; while [ ! -f \"$PWNMESH_WORKSPACE/dual-graph-release\" ]; do sleep .01; done; printf '7\\n' > value.json; cat value.json","resources":[],"inputs":[],"artifacts":["value.json"]},{"id":"join","command":"python3 -c 'import json,os; deps=json.load(open(os.environ[\"PWNMESH_DEPENDENCIES\"])); print(json.load(open(deps[0][\"output\"][\"files\"][\"value.json\"][\"path\"]))*2)'","resources":[],"inputs":[{"node":"seed","artifact":"value.json"}],"depends_on":[{"id":"seed"}]}]}`

func awaitDualGraphProcess(t *testing.T, f *updateRequestFixture, done <-chan updateRunResult) int {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if raw, err := os.ReadFile(filepath.Join(f.workspace, "dual-graph-started")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		select {
		case <-ticker.C:
		case result := <-done:
			t.Fatalf("Worker ended before actual DAG process: %+v %v", result.result, result.err)
		case <-f.ctx.Done():
			t.Fatal("actual DAG command did not start")
		}
	}
}

func TestDualGraphAbandonCancelsActualDAGWithoutStartingDependent(t *testing.T) {
	f := newUpdateRequestFixture(t)
	run, source := f.dependentRun()
	runDir := t.TempDir()
	requests := 0
	f.runner.options = worker.Options{RunDir: runDir, Provider: updateProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		requests++
		if requests != 1 {
			return agent.Message{}, errors.New("abandoned DAG reached another model request")
		}
		return updateToolCall("real-dag", "run_graph", dualGraphSpec), nil
	})}
	done := startDualGraphRun(t, f.ctx, func(ctx context.Context) updateRunResult {
		status, err := f.scheduler.runTask(ctx, run)
		return updateRunResult{worker.Result{Status: status}, err}
	})
	pid := awaitDualGraphProcess(t, f, done)
	f.correct(source)
	f.decide("step", map[string]string{"action": "abandon", "id": run.Job.Intent.ID, "reason": "Explicitly stop the local computation"})
	result := f.await(done)
	if result.result.Status != "cancelled" || !errors.Is(result.err, context.Canceled) || requests != 1 {
		t.Fatalf("abandon failed to cancel the real DAG: %+v %v requests=%d", result.result, result.err, requests)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("abandoned command process is still alive: pid=%d err=%v", pid, err)
	}
	raw, err := os.ReadFile(filepath.Join(runDir, "graph-tools", "boundary", "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint workergraph.Checkpoint
	if err = json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Status != "interrupted" || len(checkpoint.Nodes) != 2 {
		t.Fatalf("cancelled DAG lost its recoverable checkpoint: %+v", checkpoint)
	}
	for _, node := range checkpoint.Nodes {
		if node.ID == "join" && (node.Attempt != 0 || !node.StartedAt.IsZero()) {
			t.Fatal("abandoned DAG launched the dependent command")
		}
	}
	if _, err := os.Stat(filepath.Join(runDir, "graph-tools", "boundary", "nodes", "seed", "value.json")); !os.IsNotExist(err) {
		t.Fatal("cancelled command produced a success artifact")
	}
}
