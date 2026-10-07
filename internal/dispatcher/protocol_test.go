package dispatcher

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/worker"
)

// This runner records the immutable job and stops immediately. It never starts
// a process, calls a model, or touches a container.
type protocolRunner struct{ jobs chan worker.Job }

func (r *protocolRunner) Run(_ context.Context, _ config.Worker, job worker.Job) (worker.Result, error) {
	r.jobs <- job
	return worker.Result{Status: "failed", Error: "fixture finished"}, nil
}
func (*protocolRunner) Cleanup(context.Context, string, string) error { return nil }
func (*protocolRunner) Projects(context.Context) ([]string, error)    { return nil, nil }

func TestNewJobsRegisterTheCurrentResultProtocol(t *testing.T) {
	for _, kind := range []string{"reason", "explore"} {
		t.Run(kind, func(t *testing.T) {
			scheduler, _, _, graph, store := batchSchedulerFixture(t, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer func() { cancel(); scheduler.wg.Wait() }()
			var intent *board.Intent
			if kind == "explore" {
				// Authorize the Step through the current planner's committed batch.
				retryTicks(t, scheduler, 1)
				scheduler.reap()
				if err := scheduler.Client.Do(ctx, "GET", projectPath(graph.Project.ID), nil, &graph, nil); err != nil {
					t.Fatal(err)
				}
				intent = &graph.Intents[0]
			}
			runner := &protocolRunner{jobs: make(chan worker.Job, 1)}
			scheduler.Runner = runner
			started, err := scheduler.launch(ctx, graph, kind, intent, "initial", board.ExecutionCheck{})
			if err != nil || !started {
				t.Fatalf("launch: started=%v, err=%v", started, err)
			}
			var job worker.Job
			select {
			case job = <-runner.jobs:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if job.ResultContractVersion != 2 || !job.GraphRPC || job.Graph.Project.OrchestrationVersion != 1 || job.WorkerType != "go" {
				t.Fatalf("wrong protocol: kind=%s version=%d graph_rpc=%v orchestration=%d backend=%s", kind, job.ResultContractVersion, job.GraphRPC, job.Graph.Project.OrchestrationVersion, job.WorkerType)
			}
			executions := testExecutions(t, store)
			var persisted worker.Job
			if err = json.Unmarshal(executions[len(executions)-1].Job, &persisted); err != nil {
				t.Fatal(err)
			}
			if digest(persisted) != digest(job) {
				t.Fatal("worker protocol differed from immutable registered job")
			}
		})
	}
}

func TestDispatcherDoesNotLaunchRetiredProtocol(t *testing.T) {
	s := New(config.Config{Workers: []config.Worker{{Name: "go", Type: "go", TaskTypes: []string{"reason", "explore"}, MaxRunning: 1}}}, &protocolRunner{})
	for _, tc := range []struct {
		version int
		kind    string
	}{{0, "reason"}, {0, "explore"}, {1, "bootstrap"}} {
		g := board.Graph{Project: board.Project{ID: "retained", OrchestrationVersion: tc.version}}
		if started, err := s.launch(context.Background(), g, tc.kind, nil, "initial", board.ExecutionCheck{}); err != nil || started {
			t.Fatalf("retired input launched: version=%d kind=%s started=%v err=%v", tc.version, tc.kind, started, err)
		}
	}
}
