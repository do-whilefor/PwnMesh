package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/worker"
)

// Scripted planners read the same bounded input that the model receives.
// They must not depend on a second, hidden full graph inside new Jobs.
func fixturePlanInput(job worker.Job) (steps, open int, facts []board.FactRecord) {
	if ref := job.InputSnapshot; ref != nil {
		var view struct {
			Facts []board.FactRecord `json:"fact_records"`
		}
		if job.Decision != nil {
			_ = json.Unmarshal(job.Decision.View, &view)
			if review := job.Decision.CompletionAssessment; review != nil {
				view.Facts = review.FactRecords
			}
		}
		return ref.StepCount, ref.OpenCount, view.Facts
	}
	if job.State != nil {
		facts = job.State.FactRecords
	} else {
		for _, fact := range job.Graph.Facts {
			facts = append(facts, board.FactRecord{ID: fact.ID, Description: fact.Description})
		}
	}
	return len(job.Graph.Intents), job.Graph.OpenCount(), facts
}

// Scripted model fixtures explicitly supply the same root judgment required
// from the production model. This helper never changes the server's policy.
func fixtureDecisionBatch(job worker.Job, actions []board.DecisionAction) *board.DecisionBatch {
	batch := &board.DecisionBatch{ExpectedVersion: job.Decision.StateVersion, Actions: append([]board.DecisionAction{}, actions...)}
	if job.Decision.ClosureProtocol != 1 {
		return batch
	}
	batch.Assessment = &board.RootAssessment{Status: "missing", Description: "The scripted fixture still has an original condition to check", Gaps: []board.RequirementGap{{ID: "fixture", InputIDs: []string{"goal"}, Description: "Obtain or reconcile the outstanding fixture observation"}}}
	for n, action := range batch.Actions {
		var payload struct {
			Action string   `json:"action"`
			From   []string `json:"from"`
		}
		_ = json.Unmarshal(action.Payload, &payload)
		if action.Op == "complete" {
			batch.Assessment = &board.RootAssessment{Status: "satisfied", From: payload.From, Description: "The recorded fixture observations satisfy its original goal"}
		}
		if action.Op == "curation_request" || action.Op == "goal" && payload.Action == "add" || action.Op == "step" && (payload.Action == "add" || payload.Action == "retry") {
			batch.Actions[n].GapID = "fixture"
		}
	}
	return batch
}

// Authorize fixture work through the same committed primary-agent batch that
// production uses. Tests inject the proposed plan, never a weaker write route.
func authorizeFixtureSteps(t *testing.T, scheduler *Scheduler, project string, payloads ...map[string]any) []board.Intent {
	t.Helper()
	ctx := context.Background()
	if err := scheduler.Client.Do(ctx, "POST", projectPath(project)+"/hints", map[string]string{"content": fmt.Sprintf("Authorize the next %d fixture checks", len(payloads)), "creator": "user"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var graph board.Graph
	if err := scheduler.Client.Do(ctx, "GET", projectPath(project), nil, &graph, nil); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("fixture-plan-%d", time.Now().UnixNano())
	lease := Lease{Run: "fixture@" + id, Kind: "reason"}
	if err := scheduler.Client.Do(ctx, "POST", projectPath(project)+"/reason/claim", map[string]string{"worker": lease.Run, "trigger": "initial"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	run := &task{Job: worker.Job{RunID: id, Kind: "reason", WorkerType: "go", Graph: graph, GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", Budget: config.Task{MaxIntents: len(payloads)}}, Worker: config.Worker{Name: "fixture", Type: "go"}, Lease: lease}
	if err := scheduler.register(ctx, run); err != nil {
		t.Fatal(err)
	}
	actions := []board.DecisionAction{}
	for n, payload := range payloads {
		payload["action"] = "add"
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		actions = append(actions, board.DecisionAction{Op: "step", Ref: fmt.Sprintf("step%d", n), Payload: raw})
	}
	batch := fixtureDecisionBatch(run.Job, actions)
	var receipt board.DecisionReceipt
	if err := scheduler.Client.Do(ctx, "POST", projectPath(project)+"/state/decisions/commit", batch, &receipt, &lease); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Client.Do(ctx, "GET", projectPath(project), nil, &graph, nil); err != nil {
		t.Fatal(err)
	}
	steps := make([]board.Intent, len(payloads))
	for n := range steps {
		id := receipt.IDs[fmt.Sprintf("step%d", n)]
		for _, intent := range graph.Intents {
			if intent.ID == id {
				steps[n] = intent
			}
		}
		if steps[n].ID == "" {
			t.Fatalf("committed fixture step %d is missing: %+v", n, receipt)
		}
	}
	return steps
}

// Inspect retained fixture records without restoring the retired whole-history
// HTTP endpoint. Scheduling and graph operations still use the real server.
func testExecutions(t *testing.T, store *board.Store) []board.Execution {
	t.Helper()
	var executions []board.Execution
	err := store.Do(context.Background(), func(tx *board.Tx) error {
		rows, err := tx.Query("SELECT project_id,id FROM xloom_executions WHERE namespace=? ORDER BY created_at,rowid", "pwnmesh")
		if err != nil {
			return err
		}
		var identities [][2]string
		for rows.Next() {
			var identity [2]string
			if err := rows.Scan(&identity[0], &identity[1]); err != nil {
				rows.Close()
				return err
			}
			identities = append(identities, identity)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, identity := range identities {
			execution, err := tx.Execution(identity[0], identity[1])
			if err != nil {
				return err
			}
			executions = append(executions, execution)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return executions
}

// Preserve historical job shapes for recovery/bridge fixtures. New execution
// registration tests use the production /executions/prepare path instead.
func registerLegacyExecution(t *testing.T, store *board.Store, execution board.Execution) board.Execution {
	t.Helper()
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		if err := tx.RegisterExecution(execution); err != nil {
			return err
		}
		var err error
		execution, err = tx.Execution(execution.ProjectID, execution.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return execution
}
