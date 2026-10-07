package dispatcher

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/server"
	"pwnmesh/internal/worker"
)

type invalidatingRetryRunner struct {
	client  *Client
	planner *task
	calls   int
}

func (r *invalidatingRetryRunner) Run(ctx context.Context, _ config.Worker, job worker.Job) (worker.Result, error) {
	r.calls++
	if r.calls > 1 {
		return worker.Result{Status: "failed", Error: "invalid source reached a second process"}, nil
	}
	// A curator corrects the premise while the first process is active.
	// The transient result then attempts the normal retry path.
	err := r.client.Do(ctx, "POST", projectPath(job.Graph.Project.ID)+"/state/actions", map[string]any{
		"op": "curate", "idempotency_key": "correct-before-retry", "expected_version": r.planner.Job.InputSnapshot.StateVersion,
		"payload": board.CuratePayload{ThroughRevision: r.planner.Job.InputSnapshot.Revision, Groups: []board.CurateGroup{}, Relations: []board.CurateRelation{{Kind: "refutes", Source: "f002", Target: "f001", Reason: "A separate check disproved the task's premise"}}},
	}, nil, &r.planner.Lease)
	if err != nil {
		return worker.Result{}, err
	}
	return worker.Result{Status: "failed", Retryable: true, FailureKind: "transient_infrastructure", Error: "synthetic interrupted connection"}, nil
}

func (*invalidatingRetryRunner) Cleanup(context.Context, string, string) error { return nil }
func (*invalidatingRetryRunner) Projects(context.Context) ([]string, error)    { return nil, nil }

func TestRetryRechecksCorrectedStepBeforeStartingAnotherProcess(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "retry-correction.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.Now = func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var state board.State
	err = store.Do(ctx, func(tx *board.Tx) error {
		graph := board.Graph{
			Project: board.Project{ID: "retry-fixture", Status: "active", Title: "Retry fixture", OrchestrationVersion: 1, CreatedAt: tx.Now, Curator: &board.Reason{Worker: "planner@correction", StartedAt: tx.Now, Heartbeat: tx.Now}},
			Facts:   []board.Fact{{ID: "origin", Description: "Synthetic test scope"}, {ID: "goal", Description: "Verify a bounded observation"}, {ID: "f001", Description: "Initial premise"}, {ID: "f002", Description: "Independent corrective observation"}},
			Intents: []board.Intent{{ID: "i001", From: []string{"f001"}, Description: "Verify the initial premise", Creator: "fixture", Worker: board.Ptr("fixture@execute-retry"), Heartbeat: board.Ptr(tx.Now), CreatedAt: tx.Now}},
		}
		if err := tx.Save(graph); err != nil {
			return err
		}
		var err error
		state, err = tx.State(graph.Project.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.New(store))
	defer httpServer.Close()
	runner := &invalidatingRetryRunner{}
	scheduler := New(config.Config{Server: httpServer.URL, Runtime: config.Runtime{MaxWorkers: 1}}, runner)
	runner.client = scheduler.Client
	job := worker.Job{RunID: "execute-retry", Kind: "explore", WorkerType: "go", Workspace: "/workspace", Graph: state.Graph, Intent: &state.Graph.Intents[0], ResultContractVersion: 2, GraphRPC: true}
	active := &task{Job: job, Worker: config.Worker{Name: "fixture", Type: "go"}, Lease: Lease{Run: "fixture@execute-retry", Kind: "explore", Intent: job.Intent.ID}}
	if err := scheduler.register(ctx, active); err != nil {
		t.Fatal(err)
	}
	runner.planner = &task{Job: worker.Job{RunID: "correction", Kind: "curate", WorkerType: "go", Workspace: "/workspace", Graph: state.Graph, ResultContractVersion: 2, GraphRPC: true}, Worker: config.Worker{Name: "planner", Type: "go"}, Lease: Lease{Run: "planner@correction", Kind: "curate"}}
	if err := scheduler.register(ctx, runner.planner); err != nil {
		t.Fatal(err)
	}
	outcome, runErr := scheduler.runRegistered(ctx, active, func() {})
	if runner.calls != 1 || outcome != "cancelled" || runErr == nil || !strings.Contains(runErr.Error(), "not effective evidence") {
		t.Fatalf("invalidated retry started another process or lost its cause: calls=%d outcome=%s err=%v", runner.calls, outcome, runErr)
	}
	executions := testExecutions(t, store)
	if len(executions) != 2 || executions[0].Status != "cancelled" {
		t.Fatalf("retry cancellation was not durable: %+v", executions)
	}
	var result worker.Result
	if err := json.Unmarshal(executions[0].Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.FailureKind != "invalidated_before_start" {
		t.Fatalf("cancellation lost its diagnostic: %+v", result)
	}
	var after board.State
	if err := scheduler.Client.Do(ctx, "GET", projectPath(job.Graph.Project.ID)+"/state", nil, &after, nil); err != nil {
		t.Fatal(err)
	}
	if len(after.FactRecords) != len(state.FactRecords) || after.Steps[0].Result != nil || after.Steps[0].Status != "failed" {
		t.Fatal("blocked retry manufactured an observation or completed its Step")
	}
}
