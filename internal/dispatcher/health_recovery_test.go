package dispatcher

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/worker"
)

func TestReadinessRecoveryBlocksModelWorkButSettlesStoredResults(t *testing.T) {
	for _, mode := range []string{"startup_only", "startup_and_task"} {
		for _, gate := range []string{"incompatible", "transient"} {
			for _, boundary := range []string{"model_required", "result_pending", "curation_committed"} {
				t.Run(mode+"/"+gate+"/"+boundary, func(t *testing.T) {
					var s *Scheduler
					var runner *staleCuratorRunner
					var store *board.Store
					var run *task
					if boundary == "curation_committed" {
						var graph board.Graph
						s, runner, store, graph = staleCuratorFixture(t)
						run = prepareCurationTestTask(t, s, graph, "curate", "readiness-curator", nil)
						if _, err := runner.curate(context.Background(), run.Job); err != nil {
							t.Fatal(err)
						}
					} else {
						s, runner, store, run = resultDeliveryFixture(t)
						if err := s.status(context.Background(), run, "running", worker.Result{}); err != nil {
							t.Fatal(err)
						}
						if boundary == "result_pending" {
							result, err := runner.observe(context.Background(), run.Job)
							if err != nil {
								t.Fatal(err)
							}
							if err := s.status(context.Background(), run, "result_pending", result); err != nil {
								t.Fatal(err)
							}
						}
					}
					s.Config.Runtime.HealthMode = mode
					var probes atomic.Int32
					s.CheckHealth = func(context.Context, config.Worker) error {
						probes.Add(1)
						return errors.New("settlement must not contact the model")
					}
					s.recordHealth(run.Worker.Name, &healthFailure{Kind: gate, Err: errors.New("fixture readiness failure")})
					if err := s.loadExecutions(context.Background()); err != nil {
						t.Fatal(err)
					}
					project := run.Job.Graph.Project
					s.generations[project.ID], s.restartCleaned[project.ID] = project.Generation, project.Generation
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := s.recoverExecutions(ctx, map[string]string{project.ID: "active"}); err != nil {
						t.Fatal(err)
					}
					s.wg.Wait()
					execution := curationExecution(t, store, run)
					if probes.Load() != 0 || runner.calls.Load() != 0 {
						t.Fatalf("readiness gate allowed model work or probed a receipt: probes=%d workers=%d", probes.Load(), runner.calls.Load())
					}
					if boundary == "model_required" {
						if execution.Resumes != 0 || execution.Status != "running" || len(s.running) != 0 {
							t.Fatalf("unready model work consumed recovery or entered dispatch: %+v", execution)
						}
					} else if execution.Status != "succeeded" || execution.Resumes != 1 {
						t.Fatalf("readiness stranded an existing result: status=%s resumes=%d", execution.Status, execution.Resumes)
					}
				})
			}
		}
	}
}
