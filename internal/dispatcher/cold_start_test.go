package dispatcher

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/server"
	"pwnmesh/internal/worker"
)

// The scripted planner supplies the intended directions, not model quality.
// Execute waits at a barrier so the real scheduler's limits can be checked.
type coldStartRunner struct {
	batchProtocolRunner
	executeStarted chan worker.Job
	release        chan struct{}
	mu             sync.Mutex
	kinds          []string
}

func (r *coldStartRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	r.mu.Lock()
	r.kinds = append(r.kinds, job.Kind)
	r.mu.Unlock()
	if job.Kind == "explore" {
		r.executeStarted <- job
		select {
		case <-r.release:
		case <-ctx.Done():
			return worker.Result{}, ctx.Err()
		}
	}
	return r.batchProtocolRunner.Run(ctx, backend, job)
}
func (*coldStartRunner) Cleanup(context.Context, string, string) error { return nil }
func (*coldStartRunner) Projects(context.Context) ([]string, error)    { return nil, nil }

func TestNewProjectsStartWithDecideAndRespectExecuteLimits(t *testing.T) {
	for _, directions := range []int{1, 3} {
		t.Run(fmt.Sprintf("directions_%d", directions), func(t *testing.T) {
			store, err := board.Open(filepath.Join(t.TempDir(), "cold-start.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			httpServer := httptest.NewServer(server.New(store))
			defer httpServer.Close()
			runner := &coldStartRunner{batchProtocolRunner: batchProtocolRunner{directions: directions}, executeStarted: make(chan worker.Job, 8), release: make(chan struct{})}
			cfg := config.Config{
				Server:    httpServer.URL,
				Runtime:   config.Runtime{Interval: 1, MaxWorkers: 3, MaxProjects: 1, MaxProjectWorkers: 3, HealthTimeout: 5, HealthMode: "disabled"},
				Tasks:     config.Tasks{Reason: config.Task{MaxIntents: 3}, Explore: config.Task{ConcludeTimeout: 60}},
				Container: config.Container{Image: "fixture", Network: "bridge", CompletedAction: "stop"},
				Workers:   []config.Worker{{Name: "fixture", Type: "go", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: 3, Env: map[string]string{"ANTHROPIC_BASE_URL": "http://unused.invalid", "ANTHROPIC_AUTH_TOKEN": "fixture", "ANTHROPIC_MODEL": "fixture"}}},
			}
			if err = cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			scheduler := New(cfg, runner)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer func() { cancel(); scheduler.wg.Wait() }()
			var graph board.Graph
			if err = scheduler.Client.Do(ctx, "POST", "/projects", map[string]any{"title": "Cold start", "origin": "Synthetic inputs", "goal": "Check all requested fixtures"}, &graph, nil); err != nil {
				t.Fatal(err)
			}
			if graph.Project.Bootstrap {
				t.Fatal("Web creation parameter enabled bootstrap")
			}
			if err = scheduler.Step(ctx); err != nil {
				t.Fatal(err)
			}
			scheduler.wg.Wait()
			runner.mu.Lock()
			firstIsDecide := len(runner.kinds) == 1 && runner.kinds[0] == "reason"
			runner.mu.Unlock()
			if !firstIsDecide {
				t.Fatal("first activity was not a single Decide")
			}
			if err = scheduler.Step(ctx); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < min(directions, 2); i++ {
				select {
				case <-runner.executeStarted:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if len(scheduler.running) != min(directions, 2) {
				t.Fatalf("active tasks exceeded or missed configured capacity: %d", len(scheduler.running))
			}
			close(runner.release)
			scheduler.wg.Wait()
			for round := 0; round < 8; round++ {
				if err = scheduler.Step(ctx); err != nil {
					t.Fatal(err)
				}
				scheduler.wg.Wait()
				if err = scheduler.Client.Do(ctx, "GET", projectPath(graph.Project.ID), nil, &graph, nil); err != nil {
					t.Fatal(err)
				}
				if graph.Project.Status == "completed" {
					break
				}
			}
			if graph.Project.Status != "completed" {
				t.Fatalf("Decide/Execute fixture stalled: %+v", graph)
			}
			if len(graph.Intents) != directions+1 || len(graph.Facts) != directions+2 {
				t.Fatalf("unexpected duplicate or missing work: %d intents, %d facts", len(graph.Intents), len(graph.Facts))
			}
			runner.mu.Lock()
			defer runner.mu.Unlock()
			executed := 0
			for _, kind := range runner.kinds {
				if kind == "bootstrap" {
					t.Fatal("new project entered legacy bootstrap")
				}
				if kind == "explore" {
					executed++
				}
			}
			if executed != directions {
				t.Fatalf("Execute ran %d times, want %d", executed, directions)
			}
		})
	}
}
