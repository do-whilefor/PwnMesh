package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

func TestReviewStartupDoesNotLaunchRedundantReasonAfterQuietPeriod(t *testing.T) {
	s, runner, _, graph := staleCuratorFixture(t)
	reviewStarted := make(chan worker.Job, 1)
	var decisions atomic.Int32
	runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
		if job.Kind == "reason" {
			decisions.Add(1)
		}
		if job.Kind == "explore" && strings.Contains(job.Intent.Description, "Independently") {
			reviewStarted <- job
			<-ctx.Done() // No observation has been added since its authorization.
			return worker.Result{}, ctx.Err()
		}
		return runner.orchestrationLoopRunner.Run(ctx, s.Config.Workers[0], job)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer func() { cancel(); s.wg.Wait() }()
	for {
		if err := s.Step(ctx); err != nil {
			t.Fatal(err)
		}
		planning := false
		for _, task := range s.running {
			planning = planning || task.Job.Kind == "reason"
		}
		if len(reviewStarted) != 0 && !planning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("independent review did not start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if decisions.Load() != 2 {
		t.Fatalf("expected the initial plan and review authorization, got %d", decisions.Load())
	}
	// Simulate both coalescing deadlines having expired while the authorized
	// review remains live. Starting it alone must not create another decision.
	old := time.Now().Add(-2 * reasonMaxWait)
	s.reasonWaits[graph.Project.ID] = reasonWait{First: old, Changed: old}
	if launched, err := s.dispatch(ctx, graph.Project.ID); err != nil || launched {
		t.Fatalf("mechanical review startup launched an unnecessary role: %v %v", launched, err)
	}
}

func TestReviewStartupReplacesInvalidatedPlannerWithoutBlockingWorker(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart_%t", restart), func(t *testing.T) {
			s, runner, store, graph := staleCuratorFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			var replacement *Scheduler
			defer func() {
				cancel()
				s.wg.Wait()
				if replacement != nil && replacement != s {
					replacement.wg.Wait()
				}
			}()
			readState := func() board.State {
				var state board.State
				if err := s.Client.Do(ctx, "GET", projectPath(graph.Project.ID)+"/state", nil, &state, nil); err != nil {
					t.Fatal(err)
				}
				return state
			}
			runRole := func(kind, id string, intent *board.Intent) {
				state := readState()
				run := prepareCurationTestTask(t, s, state.Graph, kind, id, intent)
				if outcome, err := s.runTask(ctx, run); outcome != "success" || err != nil {
					t.Fatalf("setup role %s failed: %s %v", id, outcome, err)
				}
			}
			// Authorize the review through the ordinary producers, curator and
			// planner. It remains ready but has not been registered or started.
			runRole("reason", "initial-plan", nil)
			for _, intent := range readState().Graph.Intents {
				runRole("explore", "producer-"+intent.ID, &intent)
			}
			runRole("curate", "record-conflict", nil)
			runRole("reason", "authorize-review", nil)
			if err := changeCurationInput(ctx, s, graph); err != nil {
				t.Fatal(err)
			}
			before := readState()
			planner := prepareCurationTestTask(t, s, before.Graph, "reason", "hint-planner", nil)
			planned, reviewing := make(chan worker.Job, 2), make(chan worker.Job, 1)
			reviewEntered := make(chan struct{})
			var calls atomic.Int32
			runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
				if job.Kind == "explore" {
					reviewing <- job
					close(reviewEntered)
					<-ctx.Done() // No new review evidence can rescue a lost signal.
					return worker.Result{}, ctx.Err()
				}
				if job.Kind != "reason" {
					return worker.Result{}, fmt.Errorf("unexpected role %s", job.Kind)
				}
				attempt := calls.Add(1)
				planned <- job
				if attempt == 1 {
					select {
					case <-reviewEntered:
					case <-ctx.Done():
						return worker.Result{}, ctx.Err()
					}
				}
				_, err := runner.graph(ctx, job, worker.GraphRequest{Op: "decision_commit", Batch: &board.DecisionBatch{ExpectedVersion: job.Decision.StateVersion, Actions: []board.DecisionAction{}}})
				if attempt == 1 {
					if !decisionStateChanged(err) {
						return worker.Result{}, fmt.Errorf("review startup did not reject the old CAS: %v", err)
					}
					return worker.Result{Status: "failed", FailureKind: "state_changed", Error: err.Error()}, nil
				}
				return worker.Result{Status: "success", Text: `{"accepted":true,"data":{"decided":true}}`}, err
			}
			s.start(ctx, planner)
			select {
			case <-planned:
			case <-ctx.Done():
				t.Fatal("hint planner did not start")
			}
			// Production dispatch must preserve already-authorized concurrency
			// while the hint planner is still computing against the old status.
			if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
				t.Fatalf("live planner blocked its authorized review: %v %v", ok, err)
			}
			var review worker.Job
			select {
			case review = <-reviewing:
			case <-ctx.Done():
				t.Fatal("authorized review did not run concurrently")
			}
			waitTerminal := func(scheduler *Scheduler, run *task, status string) board.Execution {
				for {
					execution := curationExecution(t, store, run)
					if !execution.Pending() {
						if execution.Status != status {
							t.Fatalf("unexpected planner result: %s %s", execution.Status, execution.Result)
						}
						scheduler.reap()
						return execution
					}
					select {
					case <-ctx.Done():
						t.Fatal("planner did not reach its terminal boundary")
					case <-time.After(5 * time.Millisecond):
					}
				}
			}
			old := waitTerminal(s, planner, "failed")
			var failure worker.Result
			if json.Unmarshal(old.Result, &failure) != nil || failure.FailureKind != "state_changed" || failure.Retryable {
				t.Fatalf("original planner was not a terminal stale snapshot: %s", old.Result)
			}
			after := readState()
			if after.DecisionRevision != before.DecisionRevision+1 || after.Disputes[0].Status != "reviewing" {
				t.Fatal("mechanical invalidation did not preserve the outstanding hint signal")
			}
			if err := s.renewLease(ctx, s.running[review.RunID]); err != nil {
				t.Fatalf("stale planner cancelled the independent review: %v", err)
			}
			replacement = s
			if restart {
				// All eligibility/boundaries must come from durable server state,
				// including a still-live independently owned review lease.
				replacement = New(s.Config, runner)
			}
			var next worker.Job
			for next.RunID == "" {
				replacement.reap()
				if _, err := replacement.dispatch(ctx, graph.Project.ID); err != nil {
					t.Fatal(err)
				}
				select {
				case next = <-planned:
				case <-ctx.Done():
					t.Fatal("mechanically invalidated planning signal remained blocked")
				case <-time.After(5 * time.Millisecond):
				}
			}
			newRun := replacement.running[next.RunID]
			accepted := waitTerminal(replacement, newRun, "succeeded")
			if next.RunID == planner.Job.RunID || next.PreviousRunID != "" || next.InputSnapshot == nil || next.InputSnapshot.DecisionRevision != after.DecisionRevision || next.Decision.StateVersion == planner.Job.Decision.StateVersion || accepted.RetryKey == old.RetryKey || calls.Load() != 2 {
				t.Fatal("replacement did not bind exactly one fresh current input")
			}
			retained := curationExecution(t, store, planner)
			if string(retained.Job) != string(old.Job) || string(retained.Result) != string(old.Result) || retained.Status != "failed" {
				t.Fatal("fresh planning rewrote or retried the obsolete attempt")
			}
		})
	}
}
