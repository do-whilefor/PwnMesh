package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/worker"
)

type staleCuratorRunner struct {
	orchestrationLoopRunner
	run   func(context.Context, worker.Job) (worker.Result, error)
	calls atomic.Int32
}

func (r *staleCuratorRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	r.calls.Add(1)
	if r.run != nil {
		return r.run(ctx, job)
	}
	return r.orchestrationLoopRunner.Run(ctx, backend, job)
}

func staleCuratorFixture(t *testing.T) (*Scheduler, *staleCuratorRunner, *board.Store, board.Graph) {
	t.Helper()
	fixture, _, store, legacy := automaticRetryFixture(t, 0, "")
	if err := fixture.Client.Do(context.Background(), "PUT", projectPath(legacy.Project.ID)+"/status", map[string]string{"status": "stopped"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	runner := &staleCuratorRunner{}
	fixture.Config.Runtime.MaxWorkers, fixture.Config.Runtime.MaxProjectWorkers = 4, 4
	fixture.Config.Workers[0].MaxRunning = 4
	fixture.Config.Workers[0].TaskTypes = []string{"reason", "curate", "explore"}
	s := New(fixture.Config, runner)
	s.leaseTimeout = 10 * time.Second
	var graph board.Graph
	if err := s.Client.Do(context.Background(), "POST", "/projects", map[string]any{"title": "Curator cancellation", "origin": "Synthetic input", "goal": "Preserve committed curation", "orchestration_version": 1}, &graph, nil); err != nil {
		t.Fatal(err)
	}
	return s, runner, store, graph
}

func prepareCurationTestTask(t *testing.T, s *Scheduler, graph board.Graph, kind, id string, intent *board.Intent) *task {
	t.Helper()
	backend := s.Config.Workers[0]
	// Tests also call runTask directly, bypassing start's lease initialization.
	run := &task{Job: worker.Job{RunID: id, Kind: kind, Graph: graph, Intent: intent, GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", EnvironmentID: s.environmentID(backend), Budget: s.Config.Task(kind)}, Worker: backend, Lease: Lease{Run: backend.Name + "@" + id, Kind: kind}, LeaseTimeout: s.leaseTimeout}
	if intent != nil {
		run.Lease.Intent = intent.ID
	}
	path := s.leasePath(run) + "/claim"
	if kind == "explore" {
		path = s.leasePath(run) + "/heartbeat"
	}
	if kind == "reason" {
		run.Job.DecisionTrigger = "initial"
	}
	if err := s.Client.Do(context.Background(), "POST", path, map[string]string{"worker": run.Lease.Run, "trigger": "initial"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.register(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

func changeCurationInput(ctx context.Context, s *Scheduler, graph board.Graph) error {
	return s.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/hints", map[string]string{"content": "A concurrent observation supersedes the immutable input", "creator": "fixture"}, nil, nil)
}

func curationExecution(t *testing.T, store *board.Store, run *task) board.Execution {
	t.Helper()
	var execution board.Execution
	if err := store.Do(context.Background(), func(tx *board.Tx) (err error) {
		execution, err = tx.Execution(run.Job.Graph.Project.ID, run.Job.RunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return execution
}

func assertStaleCuration(t *testing.T, store *board.Store, run *task) board.Execution {
	t.Helper()
	e := curationExecution(t, store, run)
	var result worker.Result
	if json.Unmarshal(e.Result, &result) != nil || e.Status != "failed" || result.FailureKind != "state_changed" || result.Retryable {
		t.Fatalf("stale curator did not finish as one terminal state_changed failure: run=%s status=%s result=%+v", e.ID, e.Status, result)
	}
	return e
}

func TestCuratorPreflightSkipsStaleQueuedAndRetryRequests(t *testing.T) {
	for _, mode := range []string{"queued", "retry"} {
		t.Run(mode, func(t *testing.T) {
			s, runner, store, graph := staleCuratorFixture(t)
			s.Config.Runtime.Interval = 30
			run := prepareCurationTestTask(t, s, graph, "curate", "stale-curator", nil)
			wantCalls := int32(0)
			if mode == "queued" {
				if err := changeCurationInput(context.Background(), s, graph); err != nil {
					t.Fatal(err)
				}
			} else {
				wantCalls = 1
				runner.run = func(ctx context.Context, _ worker.Job) (worker.Result, error) {
					if err := changeCurationInput(ctx, s, graph); err != nil {
						return worker.Result{}, err
					}
					return worker.Result{Status: "failed", Retryable: true, FailureKind: "transient_infrastructure", Error: "synthetic transport failure"}, nil
				}
			}
			outcome, err := s.runTask(context.Background(), run)
			if outcome != "failed" || err == nil || runner.calls.Load() != wantCalls {
				t.Fatalf("stale request entered model or retried: outcome=%s err=%v calls=%d", outcome, err, runner.calls.Load())
			}
			original := assertStaleCuration(t, store, run)
			runner.run = nil
			next := prepareCurationTestTask(t, s, graph, "curate", "fresh-curator", nil)
			if immutableInputVersion(next) == immutableInputVersion(run) || next.Job.InputSnapshot.Revision <= run.Job.InputSnapshot.Revision || next.Job.PreviousRunID != "" {
				t.Fatal("replacement did not receive distinct fresh immutable input")
			}
			if outcome, err := s.runTask(context.Background(), next); outcome != "success" || err != nil {
				t.Fatalf("fresh replacement failed: %s %v", outcome, err)
			}
			if after := curationExecution(t, store, run); string(after.Job) != string(original.Job) || string(after.Result) != string(original.Result) || after.Status != original.Status {
				t.Fatal("replacement rewrote the obsolete attempt")
			}
		})
	}
}

func TestCuratorHeartbeatCancelsBlockedModelAndHealth(t *testing.T) {
	for _, mode := range []string{"model", "health"} {
		t.Run(mode, func(t *testing.T) {
			s, runner, store, graph := staleCuratorFixture(t)
			run := prepareCurationTestTask(t, s, graph, "curate", "blocked-curator", nil)
			started, stopped := make(chan struct{}), make(chan error, 1)
			block := func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				stopped <- context.Cause(ctx)
				return ctx.Err()
			}
			if mode == "model" {
				runner.run = func(ctx context.Context, _ worker.Job) (worker.Result, error) { return worker.Result{}, block(ctx) }
			} else {
				s.Config.Runtime.HealthMode = "startup_and_task"
				s.CheckHealth = func(ctx context.Context, _ config.Worker) error { return block(ctx) }
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer func() { cancel(); s.wg.Wait() }()
			s.start(ctx, run)
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("curator did not reach the blocking operation")
			}
			if err := changeCurationInput(ctx, s, graph); err != nil {
				t.Fatal(err)
			}
			// No scheduler tick, graph tool or final response is allowed to rescue it.
			select {
			case cause := <-stopped:
				if !decisionStateChanged(cause) {
					t.Fatalf("curator lost the stale-input cancellation cause: %v", cause)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("heartbeat did not cancel the stale curator")
			}
			s.wg.Wait()
			assertStaleCuration(t, store, run)
			if mode == "health" && runner.calls.Load() != 0 {
				t.Fatal("cancelled readiness probe still started a model")
			}
		})
	}
}

func TestCuratorCommitWinsStaleCancellationWithoutSeparateResultDelivery(t *testing.T) {
	for _, mode := range []string{"final_lost", "commit_ack_lost", "receipt_lost"} {
		t.Run(mode, func(t *testing.T) {
			s, runner, store, graph := staleCuratorFixture(t)
			s.Config.Runtime.Interval = 30
			run := prepareCurationTestTask(t, s, graph, "curate", "commit-race", nil)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			stale := &ProtocolError{Status: http.StatusConflict, Detail: `{"detail":"state_changed: late in-flight heartbeat"}`}
			runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
				_, err := runner.curate(ctx, job)
				cancel(stale)
				return worker.Result{}, errors.Join(err, errors.New("synthetic lost final response"))
			}
			var receiptReads atomic.Int32
			var resultDeliveries atomic.Int32
			s.Client.HTTP = &http.Client{Transport: executionQueryTransport(func(request *http.Request) (*http.Response, error) {
				if mode == "receipt_lost" && strings.HasSuffix(request.URL.Path, "/state/curation/receipt") && receiptReads.Add(1) == 2 {
					return nil, errors.New("synthetic first receipt transport failure")
				}
				if request.URL.Path == executionPath(run)+"/apply" {
					resultDeliveries.Add(1)
					return nil, errors.New("curation must not require separate result application")
				}
				response, err := http.DefaultTransport.RoundTrip(request)
				if mode == "commit_ack_lost" && err == nil && response.StatusCode == http.StatusOK && strings.HasSuffix(request.URL.Path, "/state/actions") {
					_ = response.Body.Close()
					return nil, errors.New("synthetic lost business acknowledgement")
				}
				return response, err
			})}
			outcome, err := s.runTask(ctx, run)
			if outcome != "success" || err != nil || !decisionStateChanged(context.Cause(ctx)) || runner.calls.Load() != 1 || resultDeliveries.Load() != 0 {
				t.Fatalf("committed curator was lost or rerun: outcome=%s err=%v cause=%v calls=%d", outcome, err, context.Cause(ctx), runner.calls.Load())
			}
			e := curationExecution(t, store, run)
			if e.Status != "succeeded" || !strings.Contains(string(e.Result), "curated") {
				t.Fatalf("receipt reported success without applying its execution: %+v", e)
			}
			var state board.State
			if err := s.Client.Do(context.Background(), "GET", projectPath(graph.Project.ID)+"/state", nil, &state, nil); err != nil {
				t.Fatal(err)
			}
			if state.Graph.Project.Curator != nil || state.Revision != run.Job.InputSnapshot.Revision+1 {
				t.Fatal("receipt recovery duplicated curation or retained its lease")
			}
		})
	}
}

func TestCuratorHeartbeatCancellationLeavesIndependentExecutionRunning(t *testing.T) {
	s, runner, store, graph := staleCuratorFixture(t)
	planner := prepareCurationTestTask(t, s, graph, "reason", "initial-planner", nil)
	if outcome, err := s.runTask(context.Background(), planner); outcome != "success" || err != nil {
		t.Fatalf("initial plan failed: %s %v", outcome, err)
	}
	var state board.State
	if err := s.Client.Do(context.Background(), "GET", projectPath(graph.Project.ID)+"/state", nil, &state, nil); err != nil {
		t.Fatal(err)
	}
	execute := prepareCurationTestTask(t, s, state.Graph, "explore", "independent-worker", &state.Graph.Intents[0])
	curator := prepareCurationTestTask(t, s, state.Graph, "curate", "stale-curator", nil)
	started, stopExecute, stopped := make(chan string, 2), make(chan struct{}), make(chan string, 2)
	runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
		started <- job.Kind
		if job.Kind == "explore" {
			select {
			case <-stopExecute:
				return runner.observe(ctx, job)
			case <-ctx.Done():
				stopped <- job.Kind
				return worker.Result{}, ctx.Err()
			}
		}
		<-ctx.Done()
		stopped <- job.Kind
		return worker.Result{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer func() { cancel(); s.wg.Wait() }()
	s.start(ctx, execute)
	s.start(ctx, curator)
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("parallel roles did not start")
		}
	}
	if err := changeCurationInput(ctx, s, graph); err != nil {
		t.Fatal(err)
	}
	select {
	case kind := <-stopped:
		if kind != "curate" {
			t.Fatal("curator invalidation cancelled independent Execute")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parallel curator did not cancel")
	}
	if err := s.renewLease(ctx, execute); err != nil {
		t.Fatalf("independent execution lost its own lease: %v", err)
	}
	close(stopExecute)
	s.wg.Wait()
	assertStaleCuration(t, store, curator)
	if e := curationExecution(t, store, execute); e.Status != "succeeded" {
		t.Fatalf("independent execution could not finish after curator cancellation: %+v", e)
	}
}

func pendingCurationFixture(t *testing.T) (*Scheduler, *staleCuratorRunner, *board.Store, board.Graph) {
	t.Helper()
	s, runner, store, graph := staleCuratorFixture(t)
	runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
		actions := []board.DecisionAction{}
		for _, description := range []string{"Observe the fixture: producer A", "Observe the fixture: producer B", "Observe the fixture: matching producer"} {
			payload, _ := json.Marshal(map[string]any{"action": "add", "from": []string{"origin"}, "description": description})
			actions = append(actions, board.DecisionAction{Op: "step", Payload: payload})
		}
		_, err := runner.graph(ctx, job, worker.GraphRequest{Op: "decision_commit", Batch: fixtureDecisionBatch(job, actions)})
		return worker.Result{Status: "success", Text: `{"accepted":true,"data":{"decided":true}}`}, err
	}
	planner := prepareCurationTestTask(t, s, graph, "reason", "seed-plan", nil)
	if outcome, err := s.runTask(context.Background(), planner); outcome != "success" || err != nil {
		t.Fatalf("seed plan failed: %s %v", outcome, err)
	}
	runner.run = nil
	if err := s.Client.Do(context.Background(), "GET", projectPath(graph.Project.ID), nil, &graph, nil); err != nil {
		t.Fatal(err)
	}
	producer := prepareCurationTestTask(t, s, graph, "explore", "seed-observation", &graph.Intents[0])
	if outcome, err := s.runTask(context.Background(), producer); outcome != "success" || err != nil {
		t.Fatalf("seed observation failed: %s %v", outcome, err)
	}
	// These fixtures exercise genuine reconciliation: two independent runs
	// publish matching claims. One ordinary producer no longer requires Curate.
	second := prepareCurationTestTask(t, s, graph, "explore", "matching-observation", &graph.Intents[2])
	if outcome, err := s.runTask(context.Background(), second); outcome != "success" || err != nil {
		t.Fatalf("matching observation failed: %s %v", outcome, err)
	}
	// Leave one authorized Step open so planning can continue after recovery,
	// but keep these dispatches focused on control roles.
	s.Config.Workers[0].TaskTypes = []string{"reason", "curate"}
	return s, runner, store, graph
}

func TestTerminalCuratorAllowsMainAgentWithoutRetryingUnchangedInput(t *testing.T) {
	for _, terminal := range []string{"failed", "rejected"} {
		t.Run(terminal, func(t *testing.T) {
			s, runner, store, graph := pendingCurationFixture(t)
			run := prepareCurationTestTask(t, s, graph, "curate", "terminal-curator", nil)
			runner.run = func(context.Context, worker.Job) (worker.Result, error) {
				if terminal == "rejected" {
					return worker.Result{Status: "success", Text: `{"accepted":false,"reason":"Synthetic curator refusal"}`}, nil
				}
				return worker.Result{Status: "failed", FailureKind: "fixture_failure", Error: "Synthetic terminal curator failure"}, nil
			}
			if outcome, err := s.runTask(context.Background(), run); outcome != terminal || terminal == "rejected" && err != nil {
				t.Fatalf("curator did not enter expected terminal state: %s %v", outcome, err)
			}
			var mainCalls, curatorCalls atomic.Int32
			runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
				if job.Kind == "curate" {
					curatorCalls.Add(1)
					return runner.curate(ctx, job)
				}
				mainCalls.Add(1)
				// Pending agreement merging is optional for completion. The main
				// Agent can assess the original observations directly.
				var completionErr error
				if err := store.Do(ctx, func(tx *board.Tx) error {
					state, err := tx.State(graph.Project.ID)
					if err != nil {
						return err
					}
					completionErr = tx.ValidateStateCompletion(graph.Project.ID, state.Candidates[0].Sources)
					return nil
				}); err != nil {
					return worker.Result{}, err
				}
				if completionErr != nil {
					return worker.Result{}, fmt.Errorf("optional agreement merging blocked supported completion: %w", completionErr)
				}
				_, err := runner.graph(ctx, job, worker.GraphRequest{Op: "decision_commit", Batch: fixtureDecisionBatch(job, nil)})
				return worker.Result{Status: "success", Text: `{"accepted":true,"data":{"decided":true}}`}, err
			}
			retryTicks(t, s, 3)
			if mainCalls.Load() != 1 || curatorCalls.Load() != 0 {
				t.Fatalf("terminal curator blocked main Agent or auto-retried unchanged input: main=%d curate=%d", mainCalls.Load(), curatorCalls.Load())
			}
			if err := changeCurationInput(context.Background(), s, graph); err != nil {
				t.Fatal(err)
			}
			// A changed version gets one fresh curation attempt before planning.
			if ok, err := s.dispatch(context.Background(), graph.Project.ID); !ok || err != nil {
				t.Fatalf("new input failed to schedule curation: launched=%v err=%v", ok, err)
			}
			s.wg.Wait()
			if curatorCalls.Load() != 1 || mainCalls.Load() != 1 {
				t.Fatalf("new input did not preserve curation priority: main=%d curate=%d", mainCalls.Load(), curatorCalls.Load())
			}
			if curationExecution(t, store, run).Status != terminal {
				t.Fatal("fresh curation rewrote the previous terminal attempt")
			}
		})
	}
}

func TestPendingOrActiveCuratorStillGatesMainAgent(t *testing.T) {
	for _, mode := range []string{"lease", "pending", "local"} {
		t.Run(mode, func(t *testing.T) {
			s, runner, _, graph := pendingCurationFixture(t)
			run := prepareCurationTestTask(t, s, graph, "curate", "pending-curator", nil)
			if mode != "lease" {
				if err := s.Client.Do(context.Background(), "POST", s.leasePath(run)+"/release", map[string]string{"worker": run.Lease.Run}, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "local" {
				s.running[run.Job.RunID] = run
			}
			before := runner.calls.Load()
			if launched, err := s.dispatch(context.Background(), graph.Project.ID); launched || err != nil {
				t.Fatalf("main Agent bypassed %s curator: launched=%v err=%v", mode, launched, err)
			}
			if runner.calls.Load() != before {
				t.Fatal("another control role ran while curation was pending")
			}
		})
	}
}

func TestCommittedCuratorHistorySurvivesLaterHumanStopOrRestart(t *testing.T) {
	for _, action := range []string{"stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			s, runner, store, graph := staleCuratorFixture(t)
			run := prepareCurationTestTask(t, s, graph, "curate", "management-race", nil)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
				result, err := runner.curate(ctx, job)
				if err != nil {
					return result, err
				}
				cancel(&ProtocolError{Status: http.StatusConflict, Detail: `{"detail":"state_changed: late heartbeat"}`})
				if action == "stop" {
					err = s.Client.Do(context.Background(), "PUT", projectPath(graph.Project.ID)+"/status", map[string]string{"status": "stopped"}, nil, nil)
				} else {
					err = s.Client.Do(context.Background(), "POST", projectPath(graph.Project.ID)+"/restart", map[string]any{}, nil, nil)
				}
				return result, err
			}
			if outcome, err := s.runTask(ctx, run); action == "stop" && (outcome != "success" || err != nil) {
				t.Fatalf("a later pause erased accepted history: %s %v", outcome, err)
			}
			var state board.State
			if err := s.Client.Do(context.Background(), "GET", projectPath(graph.Project.ID)+"/state", nil, &state, nil); err != nil {
				t.Fatal(err)
			}
			if state.Graph.Project.Curator != nil || runner.calls.Load() != 1 {
				t.Fatal("reconciliation reclaimed a lease or restarted a model")
			}
			if action == "stop" && (state.Graph.Project.Status != "stopped" || curationExecution(t, store, run).Status != "succeeded") || action == "restart" && state.Graph.Project.Generation != graph.Project.Generation+1 {
				t.Fatal("reconciliation changed stopped project or new generation")
			}
		})
	}
}
