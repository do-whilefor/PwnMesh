package dispatcher

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/worker"
)

type stepOrderRunner struct {
	batchProtocolRunner
	newPriority int
}

func (r *stepOrderRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	steps, _, _ := fixturePlanInput(job)
	if job.Kind != "reason" || steps != 2 {
		return r.batchProtocolRunner.Run(ctx, backend, job)
	}
	payload, _ := json.Marshal(map[string]any{"action": "add", "from": []string{"origin"}, "description": "Newly authorized fixture", "priority": r.newPriority})
	_, err := r.graph(ctx, job, worker.GraphRequest{Op: "decision_commit", Batch: fixtureDecisionBatch(job, []board.DecisionAction{{Op: "step", Payload: payload}})})
	return worker.Result{Status: "success", Text: `{"accepted":true,"data":{"decided":true}}`}, err
}

func TestReadyStepOrderPreservesPriorityAndFIFO(t *testing.T) {
	for _, priority := range []int{0, 10} {
		name := "same_priority"
		if priority != 0 {
			name = "new_high_priority"
		}
		t.Run(name, func(t *testing.T) {
			s, _, transport, graph, store := batchSchedulerFixture(t, 2)
			runner := &stepOrderRunner{batchProtocolRunner: batchProtocolRunner{directions: 2}, newPriority: priority}
			s.Runner = runner
			s.configureGraphHandler()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer func() { cancel(); s.wg.Wait() }()
			created := time.Now().UTC().Truncate(time.Second)
			store.Now = func() time.Time { return created }
			if err := s.Step(ctx); err != nil {
				t.Fatal(err)
			}
			s.wg.Wait()
			s.reap()

			// A later decision appends work while both equal-priority Steps from
			// the first batch remain ready. No execution history is rewritten.
			created = created.Add(time.Second)
			if err := s.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/hints", map[string]string{"content": "Also check one additional fixture", "creator": "user"}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
				t.Fatalf("additional plan did not start: %v %v", ok, err)
			}
			s.wg.Wait()
			s.reap()
			for range 3 {
				if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
					t.Fatalf("ready Step did not start: %v %v", ok, err)
				}
				s.wg.Wait()
				s.reap()
			}
			got := []string{}
			for _, job := range runner.jobs {
				if job.Kind == "explore" {
					got = append(got, job.Intent.Description)
				}
			}
			want := []string{"Check independent fixture 0", "Check independent fixture 1", "Newly authorized fixture"}
			if priority != 0 {
				want = []string{want[2], want[0], want[1]}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("execution order = %v, want %v", got, want)
			}
			assertBatchExecutionReceipts(t, store, transport, graph.Project.ID)
		})
	}
}
