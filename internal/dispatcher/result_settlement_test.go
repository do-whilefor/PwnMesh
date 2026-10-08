package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

func recoverPendingResult(t *testing.T, s *Scheduler, project board.Project) {
	t.Helper()
	ctx := context.Background()
	s.generations[project.ID], s.restartCleaned[project.ID] = project.Generation, project.Generation
	if err := s.loadExecutions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverExecutions(ctx, map[string]string{project.ID: project.Status}); err != nil {
		t.Fatal(err)
	}
	s.wg.Wait()
	s.reap()
}

func TestPendingResultDeliveryRetriesWithoutRestartingWorker(t *testing.T) {
	for _, resumes := range []int{0, 2} {
		t.Run(fmt.Sprintf("prior_worker_resumes_%d", resumes), func(t *testing.T) {
			s, runner, store, run := resultDeliveryFixture(t)
			ctx := context.Background()
			for range resumes {
				if err := s.Client.Do(ctx, "POST", executionPath(run)+"/resume", map[string]any{}, &run.Execution, &run.Lease); err != nil {
					t.Fatal(err)
				}
			}
			runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
				return runner.observe(ctx, job)
			}
			var applies atomic.Int32
			s.Client.HTTP = &http.Client{Transport: executionQueryTransport(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == executionPath(run)+"/apply" && applies.Add(1) <= 4 {
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"detail":"temporary delivery failure"}`)), Request: request}, nil
				}
				return http.DefaultTransport.RoundTrip(request)
			})}
			s.start(ctx, run)
			s.wg.Wait()
			s.reap()
			saved := curationExecution(t, store, run)
			if saved.Status != "result_pending" || applies.Load() != 1 {
				t.Fatalf("failed delivery did not retain the result: status=%s applies=%d", saved.Status, applies.Load())
			}
			for attempt := 1; attempt <= 4; attempt++ {
				until := s.deliveryWaits[run.Job.RunID]
				if !time.Now().Before(until) {
					t.Fatal("temporary delivery failure did not back off")
				}
				s.nextWake = time.Time{}
				recoverPendingResult(t, s, run.Job.Graph.Project)
				if applies.Load() != int32(attempt) || !s.nextWake.Equal(until) {
					t.Fatalf("delivery retried before its wake deadline: applies=%d wake=%s want=%s", applies.Load(), s.nextWake, until)
				}
				// Advance the ephemeral deadline without making the test wait for
				// four real cooldowns. Both admission and wakeup were checked above.
				s.deliveryWaits[run.Job.RunID] = time.Now().Add(-time.Second)
				recoverPendingResult(t, s, run.Job.Graph.Project)
			}
			settled := curationExecution(t, store, run)
			if settled.Status != "succeeded" || settled.Resumes != resumes || runner.calls.Load() != 1 || applies.Load() != 5 || string(settled.Job) != string(saved.Job) || string(settled.Result) != string(saved.Result) {
				t.Fatalf("delivery reran or lost its immutable result: status=%s resumes=%d workers=%d applies=%d", settled.Status, settled.Resumes, runner.calls.Load(), applies.Load())
			}
			var before, after board.State
			path := projectPath(run.Job.Graph.Project.ID) + "/state"
			if err := s.Client.Do(ctx, "GET", path, nil, &before, nil); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := s.Client.Do(ctx, "POST", executionPath(run)+"/apply", map[string]any{}, nil, &run.Lease); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Client.Do(ctx, "GET", path, nil, &after, nil); err != nil {
				t.Fatal(err)
			}
			completed := 0
			for _, step := range after.Steps {
				if step.ID == run.Lease.Intent && step.Status == "completed" {
					completed++
				}
			}
			if completed != 1 || before.Revision != after.Revision {
				t.Fatal("result application missed or duplicated business completion")
			}
		})
	}
}

func pendingResultFixture(t *testing.T) (*Scheduler, *staleCuratorRunner, *board.Store, *task) {
	t.Helper()
	s, runner, store, run := resultDeliveryFixture(t)
	ctx := context.Background()
	if err := s.status(ctx, run, "running", worker.Result{}); err != nil {
		t.Fatal(err)
	}
	result, err := runner.observe(ctx, run.Job)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.status(ctx, run, "result_pending", result); err != nil {
		t.Fatal(err)
	}
	return s, runner, store, run
}

func TestPendingResultSettlementDoesNotRequireOriginalBackend(t *testing.T) {
	for _, change := range []string{"image", "model", "backend_removed", "capability_removed", "backend_at_capacity"} {
		t.Run(change, func(t *testing.T) {
			s, runner, store, run := pendingResultFixture(t)
			original := curationExecution(t, store, run)
			switch change {
			case "image":
				s.Config.Container.Image = "replacement-image"
			case "model":
				s.Config.Workers[0].Env = map[string]string{"ANTHROPIC_MODEL": "replacement-model"}
			case "backend_removed":
				s.Config.Workers[0].Name = "replacement-backend"
			case "capability_removed":
				s.Config.Workers[0].TaskTypes = []string{"reason", "curate"}
			case "backend_at_capacity":
				s.Config.Workers[0].MaxRunning = 1
				s.running["other-project"] = &task{Job: worker.Job{Kind: "reason", Graph: board.Graph{Project: board.Project{ID: "other-project"}}}, Worker: s.Config.Workers[0]}
			}
			recoverPendingResult(t, s, run.Job.Graph.Project)
			settled := curationExecution(t, store, run)
			if settled.Status != "succeeded" || settled.Resumes != 0 || runner.calls.Load() != 0 || string(settled.Result) != string(original.Result) || string(settled.Job) != string(original.Job) {
				t.Fatalf("configuration stranded or reexecuted the saved result: status=%s resumes=%d workers=%d", settled.Status, settled.Resumes, runner.calls.Load())
			}
		})
	}
}

func TestCommittedCurationNeedsNoSeparateResultDelivery(t *testing.T) {
	s, runner, store, graph := pendingCurationFixture(t)
	ctx := context.Background()
	run := prepareCurationTestTask(t, s, graph, "curate", "committed-curation-delivery", nil)
	if err := s.status(ctx, run, "running", worker.Result{}); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.curate(ctx, run.Job); err != nil {
		t.Fatal(err)
	}
	before := runner.calls.Load()
	var applies atomic.Int32
	s.Client.HTTP = &http.Client{Transport: executionQueryTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == executionPath(run)+"/apply" && applies.Add(1) <= 2 {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"detail":"temporary delivery failure"}`)), Request: request}, nil
		}
		return http.DefaultTransport.RoundTrip(request)
	})}
	for range 3 {
		recoverPendingResult(t, s, graph.Project)
		if applies.Load() != 0 {
			t.Fatalf("committed receipt unnecessarily repeated result delivery: %d", applies.Load())
		}
	}
	e := curationExecution(t, store, run)
	if e.Status != "succeeded" || e.Resumes != 0 || runner.calls.Load() != before {
		t.Fatalf("committed curation reran the Worker or consumed delivery recovery: status=%s resumes=%d workers=%d", e.Status, e.Resumes, runner.calls.Load()-before)
	}
}

func TestLegacyCommittedCurationSettlesBeforeBackendAndRecoveryChecks(t *testing.T) {
	for _, change := range []string{"exhausted", "backend_removed", "environment_changed", "backend_at_capacity"} {
		t.Run(change, func(t *testing.T) {
			s, runner, store, graph := staleCuratorFixture(t)
			run := prepareCurationTestTask(t, s, graph, "curate", "legacy-committed", nil)
			if _, err := runner.curate(context.Background(), run.Job); err != nil {
				t.Fatal(err)
			}
			if err := store.Do(context.Background(), func(tx *board.Tx) error {
				if _, err := tx.Exec("UPDATE xloom_executions SET status='running',result=NULL,resumes=2 WHERE project_id=? AND id=?", graph.Project.ID, run.Job.RunID); err != nil {
					return err
				}
				_, err := tx.Exec("DELETE FROM xloom_revoked_runs WHERE project_id=? AND worker=?", graph.Project.ID, run.Lease.Run)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "backend_removed":
				s.Config.Workers = nil
			case "environment_changed":
				s.Config.Container.Image = "replacement-image"
			case "backend_at_capacity":
				s.Config.Workers[0].MaxRunning = 1
				s.running["other-project"] = &task{Job: worker.Job{Kind: "reason", Graph: board.Graph{Project: board.Project{ID: "other"}}}, Worker: s.Config.Workers[0]}
			}
			var resumes, applies atomic.Int32
			s.Client.HTTP = &http.Client{Transport: executionQueryTransport(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == executionPath(run)+"/resume" {
					resumes.Add(1)
				}
				if request.URL.Path == executionPath(run)+"/apply" {
					applies.Add(1)
				}
				return http.DefaultTransport.RoundTrip(request)
			})}
			recoverPendingResult(t, s, graph.Project)
			execution := curationExecution(t, store, run)
			if execution.Status != "succeeded" || execution.Resumes != 2 || resumes.Load() != 0 || applies.Load() != 1 || runner.calls.Load() != 0 {
				t.Fatalf("legacy receipt restarted or stranded: status=%s recovery=%d resumes=%d applies=%d models=%d", execution.Status, execution.Resumes, resumes.Load(), applies.Load(), runner.calls.Load())
			}
		})
	}
}

func TestPendingResultSettlementHonorsHumanLifecycle(t *testing.T) {
	for _, action := range []string{"pause", "terminate", "restart"} {
		t.Run(action, func(t *testing.T) {
			s, runner, store, run := pendingResultFixture(t)
			ctx := context.Background()
			project := run.Job.Graph.Project
			// Retain an already fetched pending page across the management action.
			if err := s.loadExecutions(ctx); err != nil {
				t.Fatal(err)
			}
			base := projectPath(project.ID)
			if action == "pause" {
				if err := s.Client.Do(ctx, "PUT", base+"/status", map[string]string{"status": "stopped"}, nil, nil); err != nil {
					t.Fatal(err)
				}
			} else if err := s.Client.Do(ctx, "POST", base+"/"+action, map[string]int64{"expected_generation": project.Generation}, nil, nil); err != nil {
				t.Fatal(err)
			}
			var current board.Graph
			if err := s.Client.Do(ctx, "GET", base, nil, &current, nil); err != nil {
				t.Fatal(err)
			}
			for _, op := range []string{"resume", "apply"} {
				if err := s.Client.Do(ctx, "POST", executionPath(run)+"/"+op, map[string]any{}, nil, &run.Lease); err == nil {
					t.Fatalf("old result bypassed %s through %s", action, op)
				}
			}
			s.generations[project.ID], s.restartCleaned[project.ID] = current.Project.Generation, current.Project.Generation
			if err := s.recoverExecutions(ctx, map[string]string{project.ID: current.Project.Status}); err != nil {
				t.Fatal(err)
			}
			s.wg.Wait()
			s.reap()
			if runner.calls.Load() != 0 || len(s.running) != 0 {
				t.Fatal("revoked result restarted execution")
			}
			if action == "restart" {
				if current.Project.Generation != project.Generation+1 || len(current.Intents) != 0 {
					t.Fatal("old settlement changed the new project round")
				}
				return
			}
			e := curationExecution(t, store, run)
			var retained worker.Result
			if err := json.Unmarshal(e.Result, &retained); err != nil {
				t.Fatal(err)
			}
			if e.Status != "cancelled" || retained.Status != "success" {
				t.Fatalf("human action applied or erased a pending result: status=%s result=%s", e.Status, retained.Status)
			}
			if action == "pause" {
				if err := s.Client.Do(ctx, "PUT", base+"/status", map[string]string{"status": "active"}, nil, nil); err != nil {
					t.Fatal(err)
				}
				if e = curationExecution(t, store, run); e.Status != "retry_requested" {
					t.Fatalf("pause/continue lost its explicit successor grant: %s", e.Status)
				}
			}
		})
	}
}
