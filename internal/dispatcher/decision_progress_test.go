package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

func TestRejectedGoalOnlyDecisionCanCommitExecutableCorrection(t *testing.T) {
	s, runner, store, graph := staleCuratorFixture(t)
	runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
		if job.Kind == "explore" {
			return runner.observe(ctx, job)
		}
		if job.Kind != "reason" {
			return worker.Result{}, fmt.Errorf("unexpected role %s", job.Kind)
		}
		batch := fixtureDecisionBatch(job, []board.DecisionAction{{Op: "goal", Ref: "investigate", Payload: json.RawMessage(`{"action":"add","condition":"Investigate the requested target"}`)}})
		for _, op := range []string{"decision_preview", "decision_commit"} {
			if _, err := runner.graph(ctx, job, worker.GraphRequest{Op: op, Batch: batch}); err == nil || !strings.Contains(err.Error(), "decision would leave the project idle") {
				return worker.Result{}, fmt.Errorf("goal-only %s was not rejected: %v", op, err)
			}
		}
		// The rejected draft must leave this same run able to authorize work.
		batch = fixtureDecisionBatch(job, []board.DecisionAction{{Op: "step", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"Observe the requested target"}`)}})
		if _, err := runner.graph(ctx, job, worker.GraphRequest{Op: "decision_commit", Batch: batch}); err != nil {
			return worker.Result{}, err
		}
		return worker.Result{Status: "success", Text: `{"accepted":true,"data":{"decided":true}}`}, nil
	}
	retryTicks(t, s, 2)
	var state board.State
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		var err error
		state, err = tx.State(graph.Project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	executions := testExecutions(t, store)
	if len(executions) != 2 || executions[0].Kind != "reason" || executions[1].Kind != "explore" || executions[0].Status != "succeeded" || executions[1].Status != "succeeded" {
		t.Fatalf("corrected decision did not reach execution: %+v", executions)
	}
	if len(state.Goals) != 1 || len(state.Steps) != 1 || state.Steps[0].Status != "completed" || !state.Steps[0].SupportValid {
		t.Fatalf("rejected draft leaked a Goal or replacement work did not finish: %+v", state)
	}
}
