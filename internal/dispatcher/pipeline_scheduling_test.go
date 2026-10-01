package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/server"
	"pwnmesh/internal/worker"
)

// Block each role independently. Ordering assertions then prove that an
// authorized execution does not require a curator response, without measuring
// wall-clock sleeps or replacing the production registration/result protocol.
type pipelineGateRunner struct {
	orchestrationLoopRunner
	extra          bool
	reconcile      bool
	requestCurator bool
	rejectCurator  bool
	started        chan worker.Job
	reasonStarted  chan worker.Job
	curateStarted  chan worker.Job
	release        map[string]chan struct{}
	allowCurator   chan struct{}
}

func (r *pipelineGateRunner) Run(ctx context.Context, _ config.Worker, job worker.Job) (worker.Result, error) {
	switch job.Kind {
	case "reason":
		var steps []board.Step
		if err := r.page(ctx, job, "steps", &steps); err != nil {
			return worker.Result{}, err
		}
		if len(steps) > 0 {
			r.reasonStarted <- job
			actions := []board.DecisionAction{}
			complete, sources := true, []string{}
			for _, step := range steps {
				complete = complete && step.Status == "completed"
				if step.Result != nil {
					sources = append(sources, *step.Result)
				}
			}
			if complete {
				payload, _ := json.Marshal(map[string]any{"from": sources, "description": "The joined observation retains both accepted producer results"})
				actions = append(actions, board.DecisionAction{Op: "complete", Payload: payload})
			} else if r.requestCurator {
				payload, _ := json.Marshal(map[string]any{"sources": sources, "reason": "Reconcile the retained observation before continuing the authorized pipeline"})
				actions = append(actions, board.DecisionAction{Op: "curation_request", Payload: payload})
			}
			_, err := r.graph(ctx, job, worker.GraphRequest{Op: "decision_commit", Batch: &board.DecisionBatch{ExpectedVersion: job.Decision.StateVersion, Actions: actions}})
			return worker.Result{Status: "success", Text: `{"accepted":true,"data":{"decided":true}}`}, err
		}
		actions := []board.DecisionAction{
			{Op: "step", Ref: "left", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"left","priority":30}`)},
			{Op: "step", Ref: "right", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"right","priority":20}`)},
			{Op: "step", Ref: "join", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"join","priority":100,"depends_on":["$left","$right"]}`)},
		}
		if r.extra {
			actions = append(actions, board.DecisionAction{Op: "step", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"independent","priority":10}`)})
		}
		_, err := r.graph(ctx, job, worker.GraphRequest{Op: "decision_commit", Batch: &board.DecisionBatch{ExpectedVersion: job.Decision.StateVersion, Actions: actions}})
		return worker.Result{Status: "success", Text: `{"accepted":true,"data":{"decided":true}}`}, err
	case "curate":
		r.curateStarted <- job
		select {
		case <-ctx.Done():
			return worker.Result{}, ctx.Err()
		case <-r.allowCurator:
			if r.rejectCurator {
				return worker.Result{Status: "success", Text: `{"accepted":false,"reason":"The retained response needs another observation before reconciliation"}`}, nil
			}
			return r.curate(ctx, job)
		}
	case "explore":
		payload, _ := json.Marshal(map[string]any{"description": "Accepted pipeline observation", "scope": job.Intent.Description, "observed_at": time.Now().UTC().Format(time.RFC3339),
			"evidence": []board.EvidenceRef{{RunID: job.RunID, Path: "/workspace/" + job.RunID + ".txt", Excerpt: "fixture"}}})
		value, err := r.graph(ctx, job, worker.GraphRequest{Op: "graph_action", Action: board.StateAction{Op: "fact", IdempotencyKey: job.RunID + ":fact", Payload: payload}})
		if err != nil {
			return worker.Result{}, err
		}
		if r.reconcile {
			candidate, _ := json.Marshal(map[string]any{"claim": "The pipeline retained the expected response", "scope": "pipeline", "status": "verified", "sources": []string{value.(board.StateActionResult).ID}, "reason": "Recorded response from this independent producer"})
			if _, err := r.graph(ctx, job, worker.GraphRequest{Op: "graph_action", Action: board.StateAction{Op: "candidate", IdempotencyKey: job.RunID + ":candidate", Payload: candidate}}); err != nil {
				return worker.Result{}, err
			}
		}
		r.started <- job
		select {
		case <-ctx.Done():
			return worker.Result{}, ctx.Err()
		case <-r.release[job.Intent.Description]:
		}
		final, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]string{"fact_id": value.(board.StateActionResult).ID}})
		return worker.Result{Status: "success", Text: string(final)}, nil
	default:
		return worker.Result{}, fmt.Errorf("unexpected pipeline role %s", job.Kind)
	}
}

func pipelineFixture(t *testing.T, slots int, extra bool) (*Scheduler, *pipelineGateRunner, *board.Store, board.Graph, context.Context) {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	api := httptest.NewServer(server.New(store))
	t.Cleanup(api.Close)
	r := &pipelineGateRunner{extra: extra, started: make(chan worker.Job, 4), reasonStarted: make(chan worker.Job, 4), curateStarted: make(chan worker.Job, 4),
		release: map[string]chan struct{}{}, allowCurator: make(chan struct{})}
	for _, name := range []string{"left", "right", "join", "independent"} {
		r.release[name] = make(chan struct{})
	}
	cfg := config.Config{Server: api.URL,
		Runtime: config.Runtime{Interval: 1, MaxWorkers: slots, MaxProjects: 1, MaxProjectWorkers: slots, HealthMode: "disabled"},
		Tasks:   config.Tasks{Reason: config.Task{MaxIntents: 4}, Explore: config.Task{ConcludeTimeout: 60}},
		Workers: []config.Worker{{Name: "pipeline", Type: "go", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: slots}}}
	s := New(cfg, r)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(func() { cancel(); s.wg.Wait() })
	var graph board.Graph
	if err = s.Client.Do(ctx, "POST", "/projects", map[string]any{"title": "Pipeline", "origin": "Local synthetic observations", "goal": "Join two accepted results", "orchestration_version": 1}, &graph, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	s.wg.Wait()
	s.reap()
	return s, r, store, graph, ctx
}

func pipelineStarted(t *testing.T, ctx context.Context, r *pipelineGateRunner) worker.Job {
	t.Helper()
	select {
	case job := <-r.started:
		return job
	case <-r.curateStarted:
		t.Fatal("authorized ready execution waited for a curator model response")
	case <-ctx.Done():
		t.Fatal("pipeline did not launch the authorized execution", ctx.Err())
	}
	return worker.Job{}
}

func TestAuthorizedPipelineDoesNotWaitForCuration(t *testing.T) {
	for _, slots := range []int{1, 4} {
		t.Run(fmt.Sprintf("%d_slots", slots), func(t *testing.T) {
			s, r, store, graph, ctx := pipelineFixture(t, slots, false)
			if err := s.Step(ctx); err != nil {
				t.Fatal(err)
			}
			left := pipelineStarted(t, ctx, r)
			var right worker.Job
			if slots > 1 {
				// Neither producer is allowed to finish yet: both arrivals prove
				// outer Worker overlap rather than merely two queued executions.
				right = pipelineStarted(t, ctx, r)
				if left.Intent.Description == "right" {
					left, right = right, left
				}
			}
			if left.Intent.Description != "left" {
				t.Fatal("priority did not select the left producer")
			}
			close(r.release["left"])
			if slots == 1 {
				s.wg.Wait()
				s.reap()
				if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
					t.Fatalf("second producer did not start: %v %v", ok, err)
				}
				right = pipelineStarted(t, ctx, r)
			}
			if right.Intent.Description != "right" {
				t.Fatal("join started before both producers completed")
			}
			// Both Facts exist, but right still has no accepted final result.
			for range 2 {
				if err := s.Step(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if len(r.started) != 0 || len(r.curateStarted) != 0 {
				t.Fatal("unaccepted producer evidence bypassed the dependency barrier or burst coalescing")
			}
			close(r.release["right"])
			s.wg.Wait()
			s.reap()
			if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
				t.Fatalf("join did not start: %v %v", ok, err)
			}
			join := pipelineStarted(t, ctx, r)
			if join.Intent.Description != "join" || len(join.DependencyResults) != 2 {
				t.Fatalf("join lost its two accepted producer results: %+v", join.DependencyResults)
			}
			bindings := map[string]string{}
			for _, dependency := range join.DependencyResults {
				bindings[dependency.StepID] = dependency.RunID
			}
			if bindings[left.Intent.ID] != left.RunID || bindings[right.Intent.ID] != right.RunID {
				t.Fatal("join consumed a result from an unbound producer")
			}
			close(r.release["join"])
			s.wg.Wait()
			s.reap()
			if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
				t.Fatalf("drained pipeline did not start completion assessment: %v %v", ok, err)
			}
			select {
			case <-r.reasonStarted:
			case <-r.curateStarted:
				t.Fatal("plain observations inserted a curation turn")
			case <-ctx.Done():
				t.Fatal("completion assessment did not run after authorized work drained")
			}
			s.wg.Wait()
			curations := 0
			for _, execution := range testExecutions(t, store) {
				if execution.Status != "succeeded" {
					t.Fatalf("pipeline role did not persist its result: %s %s", execution.Kind, execution.Status)
				}
				if execution.Kind == "curate" {
					curations++
				}
			}
			if curations != 0 {
				t.Fatalf("finite authorized pipeline inserted extra curation calls: %d", curations)
			}
		})
	}
}

func TestReadyPipelinePreservesReservedCuratorCapacity(t *testing.T) {
	s, r, _, graph, ctx := pipelineFixture(t, 3, true)
	r.reconcile = true
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		job := pipelineStarted(t, ctx, r)
		if job.Intent.Description == "independent" || job.Intent.Description == "join" {
			t.Fatal("lower-priority work consumed the reserved control capacity")
		}
	}
	// Continuous producers are allowed only a bounded coalescing period. Keep
	// both live and a third independent Step ready when that deadline expires.
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	s.curationWaits[graph.Project.ID] = reasonWait{First: time.Now().Add(-2 * reasonMaxWait)}
	if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
		t.Fatalf("reserved control slot did not start curation: %v %v", ok, err)
	}
	select {
	case <-r.curateStarted:
	case job := <-r.started:
		t.Fatalf("ready Step %s consumed the reserved control slot", job.Intent.Description)
	case <-ctx.Done():
		t.Fatal("ready executions starved curation")
	}
	if len(s.running) != 3 {
		t.Fatalf("expected two live executions and one curator, got %d", len(s.running))
	}
}

func TestUserCorrectionKeepsControlAheadOfAuthorizedPipeline(t *testing.T) {
	s, r, _, graph, ctx := pipelineFixture(t, 1, false)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	left := pipelineStarted(t, ctx, r)
	if left.Intent.Description != "left" {
		t.Fatal("unexpected first producer")
	}
	close(r.release["left"])
	s.wg.Wait()
	s.reap()
	if err := s.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/hints", map[string]string{"content": "Reconsider the remaining pipeline before continuing", "creator": "user"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
		t.Fatalf("urgent control did not start: %v %v", ok, err)
	}
	select {
	case <-r.reasonStarted:
	case <-r.curateStarted:
		t.Fatal("plain observations delayed user correction behind curation")
	case job := <-r.started:
		t.Fatalf("old authorization %s overtook a new user correction", job.Intent.Description)
	case <-ctx.Done():
		t.Fatal("user correction did not wake a control role")
	}
}

func TestQueuedPipelineDependencyBecomesUrgentOnlyWhenItsSupportFails(t *testing.T) {
	s := New(config.Config{}, nil)
	previous := board.SchedulePage{}
	waiting := board.SchedulePage{Steps: []board.Step{{ID: "upstream", Status: "open"}, {ID: "join", Status: "blocked", BlockedBy: []string{"upstream"}}}}
	s.noteInvalidDependencies("project", previous, waiting)
	if s.reasonWaits["project"].Urgent {
		t.Fatal("an authorized dependency waiting to execute was treated as new invalid evidence")
	}
	failed := board.SchedulePage{Steps: []board.Step{{ID: "upstream", Status: "failed"}, {ID: "join", Status: "blocked", BlockedBy: []string{"upstream"}}}}
	s.noteInvalidDependencies("project", waiting, failed)
	if !s.reasonWaits["project"].Urgent {
		t.Fatal("failed upstream did not require prompt reconsideration of the authorized plan")
	}
}

func TestExplicitCurationRequestPausesWorkUntilAcknowledgedOrTerminal(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprintf("rejected_%v", reject), func(t *testing.T) {
			s, r, _, graph, ctx := pipelineFixture(t, 2, false)
			r.rejectCurator = reject
			if err := s.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if first := pipelineStarted(t, ctx, r); first.Intent.Description != "left" {
				t.Fatal("expected the first producer")
			}
			close(r.release["left"])
			s.wg.Wait()
			s.reap()
			r.requestCurator = true
			if err := s.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/hints", map[string]string{"content": "Reconcile the observed response before continuing", "creator": "user"}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
				t.Fatalf("request planner did not start: %v %v", ok, err)
			}
			select {
			case <-r.reasonStarted:
			case <-ctx.Done():
				t.Fatal("request planner did not observe the correction")
			}
			s.wg.Wait()
			s.reap()
			if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
				t.Fatalf("explicit request did not start immediate curation: %v %v", ok, err)
			}
			select {
			case <-r.curateStarted:
			case job := <-r.started:
				t.Fatalf("ready %s overtook explicit reconciliation", job.Intent.Description)
			case <-ctx.Done():
				t.Fatal("explicit request waited for ordinary coalescing")
			}
			if ok, err := s.dispatch(ctx, graph.Project.ID); ok || err != nil || len(r.started) != 0 {
				t.Fatalf("new execution raced the unacknowledged correction: %v %v", ok, err)
			}
			close(r.allowCurator)
			s.wg.Wait()
			s.reap()
			var state board.State
			if err := s.Client.Do(ctx, "GET", projectPath(graph.Project.ID)+"/state", nil, &state, nil); err != nil {
				t.Fatal(err)
			}
			if (state.PendingCurationRequest() != nil) != reject {
				t.Fatal("only successful curation may acknowledge the original request")
			}
			if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
				t.Fatalf("acknowledged correction held ready work: %v %v", ok, err)
			}
			if next := pipelineStarted(t, ctx, r); next.Intent.Description != "right" {
				t.Fatalf("wrong ready work after reconciliation: %s", next.Intent.Description)
			}
		})
	}
}
