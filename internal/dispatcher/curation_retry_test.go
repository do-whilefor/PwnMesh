package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

func TestCuratorInfrastructureSuccessorRunsBeforeMainAgent(t *testing.T) {
	s, runner, store, graph := pendingCurationFixture(t)
	jobs := make(chan worker.Job, 8)
	var attempts atomic.Int32
	runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
		jobs <- job
		if job.Kind == "curate" {
			if attempts.Add(1) == 1 {
				return worker.Result{Status: "failed", FailureKind: "budget_exhausted", FailureCause: "transport", Error: "typed model request interruption at the original deadline"}, nil
			}
			return runner.curate(ctx, job)
		}
		return runner.plan(ctx, job)
	}
	retryTicks(t, s, 1)
	first := <-jobs
	if first.Kind != "curate" {
		t.Fatal("pending observations did not receive curation priority")
	}
	var original board.Execution
	if err := store.Do(context.Background(), func(tx *board.Tx) (err error) {
		original, err = tx.Execution(graph.Project.ID, first.RunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	retryTicks(t, s, 4)
	if len(jobs) != 2 {
		t.Fatalf("recovery stalled or created extra control work: jobs=%d", len(jobs))
	}
	second, main := <-jobs, <-jobs
	if second.Kind != "curate" || second.PreviousRunID != first.RunID || second.RunID == first.RunID || main.Kind != "reason" || second.InputSnapshot.StateVersion != first.InputSnapshot.StateVersion {
		t.Fatalf("main bypassed recovery or successor changed its input: first=%s second=%s previous=%s main=%s", first.Kind, second.Kind, second.PreviousRunID, main.Kind)
	}
	var state board.State
	if err := s.Client.Do(context.Background(), "GET", projectPath(graph.Project.ID)+"/state", nil, &state, nil); err != nil {
		t.Fatal(err)
	}
	if len(state.Steps) != 3 || state.Curation.ThroughRevision != first.InputSnapshot.Revision {
		t.Fatal("infrastructure recovery invented business work or lost curation progress")
	}
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		old, err := tx.Execution(graph.Project.ID, first.RunID)
		if err != nil {
			return err
		}
		next, err := tx.Execution(graph.Project.ID, second.RunID)
		if err != nil {
			return err
		}
		if old.Status != "retried" || next.Status != "succeeded" || old.RetryKey != next.RetryKey || string(old.Job) != string(original.Job) || string(old.Result) != string(original.Result) {
			t.Fatal("successor lost its durable grant or rewrote the original attempt")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCuratorInfrastructureAllowanceSurvivesDispatcherRestart(t *testing.T) {
	s, runner, store, graph := pendingCurationFixture(t)
	var attempts atomic.Int32
	runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
		if job.Kind == "curate" {
			attempts.Add(1)
			return worker.Result{Status: "failed", FailureKind: "recovery_exhausted", FailureCause: "transport", Error: "model stream interrupted"}, nil
		}
		return runner.plan(ctx, job)
	}
	retryTicks(t, s, 1)
	s = New(s.Config, runner)
	retryTicks(t, s, 1)
	s = New(s.Config, runner)
	retryTicks(t, s, 4)
	if attempts.Load() != 2 {
		t.Fatalf("dispatcher restart lost or replenished the curator allowance: %d", attempts.Load())
	}
	curators := []board.Execution{}
	for _, e := range testExecutions(t, store) {
		if e.ProjectID == graph.Project.ID && e.Kind == "curate" {
			curators = append(curators, e)
		}
	}
	if len(curators) != 2 || curators[0].Status != "retried" || curators[1].Status != "failed" {
		t.Fatalf("curation infrastructure bound did not persist: executions=%d", len(curators))
	}
	path := projectPath(graph.Project.ID) + "/executions/" + curators[1].ID + "/retry"
	if err := s.Client.Do(context.Background(), "POST", path, map[string]bool{"automatic": true}, nil, nil); err == nil {
		t.Fatal("server granted a third automatic attempt")
	}
}

func TestCuratorAutomaticRetryGrantSurvivesLostAcknowledgement(t *testing.T) {
	s, runner, store, graph := pendingCurationFixture(t)
	var attempts atomic.Int32
	runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
		if job.Kind == "curate" {
			if attempts.Add(1) == 1 {
				return worker.Result{Status: "failed", FailureKind: "transport", Error: "connection reset"}, nil
			}
			return runner.curate(ctx, job)
		}
		return runner.plan(ctx, job)
	}
	retryTicks(t, s, 1)
	var first board.Execution
	for _, e := range testExecutions(t, store) {
		if e.ProjectID == graph.Project.ID && e.Kind == "curate" {
			first = e
		}
	}
	path := projectPath(graph.Project.ID) + "/executions/" + first.ID + "/retry"
	for range 2 {
		if err := s.Client.Do(context.Background(), "POST", path, map[string]bool{"automatic": true}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	s = New(s.Config, runner)
	retryTicks(t, s, 3)
	if attempts.Load() != 2 {
		t.Fatalf("lost grant acknowledgement duplicated successor: %d", attempts.Load())
	}
	for _, e := range testExecutions(t, store) {
		if e.ProjectID == graph.Project.ID && e.Kind == "curate" && e.ID != first.ID {
			var job worker.Job
			if json.Unmarshal(e.Job, &job) != nil || job.PreviousRunID != first.ID || e.Status != "succeeded" {
				t.Fatal("restart did not consume the same durable grant")
			}
		}
	}
	// Automatic retry remains a management action, never a Worker capability.
	err := s.Client.Do(context.Background(), "POST", path, map[string]bool{"automatic": true}, nil, &Lease{Run: first.Lease, Kind: "curate"})
	if pe, ok := err.(*ProtocolError); !ok || pe.Status != http.StatusForbidden {
		t.Fatalf("curator was allowed to authorize its own fresh run: %v", err)
	}
}

func TestCuratorInfrastructureRetryReleasesOnlyItsInterruptedTerminalLease(t *testing.T) {
	s, runner, store, graph := pendingCurationFixture(t)
	var attempts atomic.Int32
	runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
		if job.Kind == "curate" {
			if attempts.Add(1) == 1 {
				return worker.Result{Status: "failed", FailureKind: "transport", Error: "connection reset"}, nil
			}
			return runner.curate(ctx, job)
		}
		return runner.plan(ctx, job)
	}
	retryTicks(t, s, 1)
	var first board.Execution
	for _, e := range testExecutions(t, store) {
		if e.ProjectID == graph.Project.ID && e.Kind == "curate" {
			first = e
		}
	}
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		g, err := tx.Load(graph.Project.ID)
		if err != nil {
			return err
		}
		// Simulate dispatcher exit after its durable terminal write, before the
		// deferred lease release. The old run remains revoked throughout.
		g.Project.Curator = &board.Reason{Worker: first.Lease, Trigger: "interrupted_release", StartedAt: tx.Now, Heartbeat: tx.Now}
		return tx.Save(g)
	}); err != nil {
		t.Fatal(err)
	}
	s = New(s.Config, runner)
	retryTicks(t, s, 3)
	if attempts.Load() != 2 {
		t.Fatalf("revoked terminal lease prevented bounded infrastructure recovery: %d", attempts.Load())
	}
}
