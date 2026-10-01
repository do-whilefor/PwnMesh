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

type dependencyRunner struct {
	orchestrationLoopRunner
	producerPublished chan worker.Job
	consumerStarted   chan worker.Job
	consumerStopped   chan error
	allowProducer     chan struct{}
	allowConsumer     chan struct{}
}

func (r *dependencyRunner) Run(ctx context.Context, _ config.Worker, j worker.Job) (worker.Result, error) {
	if j.Kind == "curate" {
		return r.curate(ctx, j)
	}
	if j.Kind == "reason" {
		steps, _, _ := fixturePlanInput(j)
		actions := []board.DecisionAction{}
		if steps == 0 {
			actions = []board.DecisionAction{
				{Op: "step", Ref: "produce", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"Produce checked data"}`)},
				{Op: "step", Ref: "consume", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"Consume checked data","priority":100,"depends_on":["$produce"]}`)},
			}
		}
		_, err := r.graph(ctx, j, worker.GraphRequest{Op: "decision_commit", Batch: &board.DecisionBatch{ExpectedVersion: j.Decision.StateVersion, Actions: actions}})
		return worker.Result{Status: "success", Text: `{"accepted":true,"data":{"decided":true}}`}, err
	}
	if j.Kind != "explore" || j.Intent == nil {
		return worker.Result{}, errors.New("unexpected dependency role")
	}
	payload, _ := json.Marshal(map[string]any{"description": "Fixture data retained by this run", "scope": j.Intent.Description,
		"observed_at": time.Now().UTC().Format(time.RFC3339), "evidence": []board.EvidenceRef{{RunID: j.RunID, Path: "/workspace/" + j.RunID + ".txt", Excerpt: "fixture"}}})
	value, err := r.graph(ctx, j, worker.GraphRequest{Op: "graph_action", Action: board.StateAction{Op: "fact", IdempotencyKey: j.RunID + ":fact", Payload: payload}})
	if err != nil {
		return worker.Result{}, err
	}
	fact := value.(board.StateActionResult)
	gate := r.allowProducer
	if j.Intent.Description == "Produce checked data" {
		r.producerPublished <- j
	} else {
		r.consumerStarted <- j
		gate = r.allowConsumer
	}
	select {
	case <-gate:
	case <-ctx.Done():
		if j.Intent.Description == "Consume checked data" {
			r.consumerStopped <- context.Cause(ctx)
		}
		return worker.Result{}, ctx.Err()
	}
	final, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]string{"fact_id": fact.ID}})
	return worker.Result{Status: "success", Text: string(final)}, nil
}

func TestSchedulerWaitsForAcceptedDependencyAndCancelsInvalidatedConsumer(t *testing.T) {
	for _, invalidate := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful_chain", true: "invalidation"}[invalidate], func(t *testing.T) {
			store, err := board.Open(filepath.Join(t.TempDir(), "dependencies.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			api := httptest.NewServer(server.New(store))
			defer api.Close()
			r := &dependencyRunner{producerPublished: make(chan worker.Job, 2), consumerStarted: make(chan worker.Job, 2), consumerStopped: make(chan error, 2), allowProducer: make(chan struct{}), allowConsumer: make(chan struct{})}
			cfg := config.Config{Server: api.URL, Runtime: config.Runtime{Interval: 1, MaxWorkers: 4, MaxProjects: 1, MaxProjectWorkers: 4, HealthMode: "disabled"},
				Tasks:   config.Tasks{Reason: config.Task{MaxIntents: 3}, Explore: config.Task{ConcludeTimeout: 60}},
				Workers: []config.Worker{{Name: "scripted", Type: "go", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: 4}}}
			s := New(cfg, r)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer func() { cancel(); s.wg.Wait() }()
			var g board.Graph
			if err = s.Client.Do(ctx, "POST", "/projects", map[string]any{"title": "Dependencies", "origin": "Local fixture", "goal": "Check dependent work", "orchestration_version": 1}, &g, nil); err != nil {
				t.Fatal(err)
			}
			advanceUntil := func(ready func() bool) {
				t.Helper()
				for !ready() {
					if ctx.Err() != nil {
						t.Fatal("dependency fixture did not progress", ctx.Err())
					}
					if err := s.Step(ctx); err != nil {
						t.Fatal(err)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			advanceUntil(func() bool { return len(r.producerPublished) > 0 })
			producer := <-r.producerPublished
			// The producer has already published a Fact. Even a higher-priority
			// consumer cannot start until the final result and receipt commit.
			for range 5 {
				if err := s.Step(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if len(r.consumerStarted) != 0 {
				t.Fatal("published evidence was mistaken for successful completion")
			}
			close(r.allowProducer)
			advanceUntil(func() bool { return len(r.consumerStarted) > 0 })
			consumer := <-r.consumerStarted
			if len(consumer.DependencyResults) != 1 || consumer.DependencyResults[0].StepID != producer.Intent.ID || consumer.DependencyResults[0].RunID != producer.RunID {
				t.Fatalf("consumer lost its accepted producer binding: %+v", consumer.DependencyResults)
			}
			if invalidate {
				fact := consumer.DependencyResults[0].FactID
				err = store.Do(ctx, func(tx *board.Tx) error {
					_, err := tx.Exec(`UPDATE xloom_state SET data=json_set(data,'$.facts['||(SELECT key FROM json_each(data,'$.facts') WHERE json_extract(value,'$.id')=?)||'].status','refuted'),revision=revision+1,decision_revision=decision_revision+1 WHERE project_id=?`, fact, g.Project.ID)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				// Stop ticking the scheduler: heartbeat must interrupt the active
				// process even if no scheduler capacity or wakeup remains.
				select {
				case cause := <-r.consumerStopped:
					if !dependencyInvalidated(cause) {
						t.Fatalf("lost cancellation cause: %v", cause)
					}
				case <-ctx.Done():
					t.Fatal("invalidated consumer did not stop")
				}
			} else {
				close(r.allowConsumer)
			}
			for {
				var current board.Execution
				err := store.Do(ctx, func(tx *board.Tx) error {
					var err error
					current, err = tx.Execution(g.Project.ID, consumer.RunID)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				if !current.Pending() {
					if invalidate {
						var result worker.Result
						if err := json.Unmarshal(current.Result, &result); err != nil {
							t.Fatal(err)
						}
						if current.Status != "cancelled" || result.FailureKind != "dependency_invalidated" || result.Retryable {
							t.Fatalf("invalidation was not terminal: %+v %+v", current, result)
						}
					} else if current.Status != "succeeded" {
						t.Fatalf("consumer did not succeed: %+v", current)
					}
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}
