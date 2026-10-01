package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/server"
	"pwnmesh/internal/worker"
)

type businessRetryRunner struct {
	orchestrationLoopRunner
	attempts      chan worker.Job
	planningRetry chan struct{}
	authorize     chan struct{}
}

func (r *businessRetryRunner) Run(ctx context.Context, _ config.Worker, j worker.Job) (worker.Result, error) {
	switch j.Kind {
	case "curate":
		return r.curate(ctx, j)
	case "reason":
		var steps []board.Step
		if err := r.page(ctx, j, "steps", &steps); err != nil {
			return worker.Result{}, err
		}
		actions := []board.DecisionAction{}
		if len(steps) == 0 {
			actions = append(actions, board.DecisionAction{Op: "step", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"Run the authorized fixture check"}`)})
		} else if steps[0].Status == "failed" {
			select {
			case r.planningRetry <- struct{}{}:
			default:
			}
			select {
			case <-r.authorize:
			case <-ctx.Done():
				return worker.Result{}, ctx.Err()
			}
			payload, _ := json.Marshal(map[string]any{"action": "retry", "id": steps[0].ID, "latest_run_id": steps[0].LatestRunID, "reason": "Authorize one new attempt after the fixture's transient failure"})
			actions = append(actions, board.DecisionAction{Op: "step", Payload: payload})
		}
		batch := &board.DecisionBatch{ExpectedVersion: j.Decision.StateVersion, Actions: actions}
		if _, err := r.graph(ctx, j, worker.GraphRequest{Op: "decision_preview", Batch: batch}); err != nil {
			return worker.Result{}, err
		}
		_, err := r.graph(ctx, j, worker.GraphRequest{Op: "decision_commit", Batch: batch})
		return worker.Result{Status: "success", Text: `{"accepted":true,"data":{"decided":true}}`}, err
	case "explore":
		r.attempts <- j
		if j.PreviousRunID == "" {
			return worker.Result{Status: "failed", FailureKind: "fixture_failure", Error: "First attempt deliberately fails"}, nil
		}
		return r.observe(ctx, j)
	default:
		return worker.Result{}, errors.New("unexpected fixture role")
	}
}

func TestSchedulerBusinessRetryRequiresDecisionAndConsumesOneGrant(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := httptest.NewServer(server.New(store))
	defer api.Close()
	r := &businessRetryRunner{attempts: make(chan worker.Job, 8), planningRetry: make(chan struct{}, 1), authorize: make(chan struct{})}
	cfg := config.Config{Server: api.URL, Runtime: config.Runtime{Interval: 1, MaxWorkers: 4, MaxProjects: 1, MaxProjectWorkers: 4, HealthMode: "disabled"},
		Tasks:   config.Tasks{Reason: config.Task{MaxIntents: 1}, Explore: config.Task{ConcludeTimeout: 60}},
		Workers: []config.Worker{{Name: "scripted", Type: "go", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: 4}}}
	s := New(cfg, r)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer func() { cancel(); s.wg.Wait() }()
	var g board.Graph
	if err := s.Client.Do(ctx, "POST", "/projects", map[string]any{"title": "Retry", "origin": "Local fixture", "goal": "Check one explicit retry", "orchestration_version": 1}, &g, nil); err != nil {
		t.Fatal(err)
	}
	advanceUntil := func(ready func() bool) {
		t.Helper()
		for !ready() {
			if ctx.Err() != nil {
				t.Fatal("retry fixture did not progress", ctx.Err())
			}
			if err := s.Step(ctx); err != nil {
				t.Fatal(err)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	advanceUntil(func() bool { return len(r.planningRetry) != 0 })
	first := <-r.attempts
	for range 5 {
		if err := s.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.attempts) != 0 {
		t.Fatal("failed business task retried without main Agent authorization")
	}
	close(r.authorize)
	advanceUntil(func() bool { return len(r.attempts) != 0 })
	second := <-r.attempts
	if second.RunID == first.RunID || second.PreviousRunID != first.RunID || second.Intent.ID != first.Intent.ID || second.InputSnapshot.ID == first.InputSnapshot.ID {
		t.Fatalf("retry did not create a fresh attempt of the authorized Step: first=%s second=%s previous=%s", first.RunID, second.RunID, second.PreviousRunID)
	}
	var current, previous board.Execution
	advanceUntil(func() bool {
		if err := store.Do(ctx, func(tx *board.Tx) error {
			var err error
			current, err = tx.Execution(g.Project.ID, second.RunID)
			if err != nil {
				return err
			}
			previous, err = tx.Execution(g.Project.ID, first.RunID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return !current.Pending()
	})
	if current.Status != "succeeded" || previous.Status != "retried" {
		t.Fatalf("retry not consumed successfully: %s / %s", previous.Status, current.Status)
	}
	var firstResult worker.Result
	if json.Unmarshal(previous.Result, &firstResult) != nil || firstResult.FailureKind != "fixture_failure" {
		t.Fatal("retry lost the original failure record")
	}
	for range 8 {
		if err := s.Step(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(r.attempts) != 0 {
		t.Fatal("one retry authorization started more than one new attempt")
	}
}
