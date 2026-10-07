package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

// Exercise the schema actually supplied to the model, the Agent Loop's strict
// argument validation, the private draft and file bridge, and a real Board
// transaction. Direct StateAction tests cannot detect a missing tool field.
func TestStepWritePathsSurviveWorkerLoopDraftAndBoardCommit(t *testing.T) {
	ctx := context.Background()
	store, err := board.Open(filepath.Join(t.TempDir(), "writes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	job, runDir := draftRunJob(t), t.TempDir()
	fence := board.ExecutionFence{Run: "planner@" + job.RunID, Lease: "reason"}
	if err = store.Do(ctx, func(tx *board.Tx) error {
		job.Graph.Project.CreatedAt = tx.Now
		job.Graph.Project.Reason = &board.Reason{Worker: fence.Run, Trigger: "initial", StartedAt: tx.Now, Heartbeat: tx.Now}
		if err := tx.Save(job.Graph); err != nil {
			return err
		}
		state, err := tx.State(job.Graph.Project.ID)
		if err != nil {
			return err
		}
		job.State, job.Graph = &state, state.Graph
		job.Decision, err = board.BuildDecisionContextFromCursor(state, nil, nil, board.DefaultContextViewBytes)
		if err != nil {
			return err
		}
		job.Decision.Version = 2
		raw, err := json.Marshal(job)
		if err != nil {
			return err
		}
		return tx.RegisterExecution(board.Execution{ProjectID: job.Graph.Project.ID, ID: job.RunID, Namespace: "test", Backend: "planner", Kind: "reason", Lease: fence.Run, Job: raw, RetryKey: "reason:writes"})
	}); err != nil {
		t.Fatal(err)
	}
	wantPaths := map[string][]string{
		"android":  {"/workspace/release-audit/android"},
		"linux":    {"/workspace/release-audit/linux"},
		"delivery": {"/workspace/release-audit/report.json"},
	}
	calls, commits := 0, 0
	var committed board.DecisionReceipt
	bridge := &draftTestBridge{dir: runDir, handle: func(request GraphRequest) (any, error) {
		var receipt board.DecisionReceipt
		err := store.Do(ctx, func(tx *board.Tx) error {
			var err error
			switch request.Op {
			case "decision_receipt":
				receipt, err = tx.DecisionReceipt(job.Graph.Project.ID, fence)
			case "decision_commit":
				commits++
				if request.Batch == nil || len(request.Batch.Actions) != 3 {
					return fmt.Errorf("lost draft actions: %+v", request.Batch)
				}
				for _, action := range request.Batch.Actions {
					var payload struct {
						WritePaths []string `json:"write_paths"`
					}
					if err := json.Unmarshal(action.Payload, &payload); err != nil {
						return err
					}
					if !reflect.DeepEqual(payload.WritePaths, wantPaths[action.Ref]) {
						return fmt.Errorf("draft lost %s output paths: %v", action.Ref, payload.WritePaths)
					}
				}
				receipt, err = tx.CommitDecision(job.Graph.Project.ID, fence, *request.Batch)
				committed = receipt
			default:
				return fmt.Errorf("unexpected request: %s", request.Op)
			}
			return err
		})
		return receipt, err
	}}
	provider := scenarioProvider(func(_ context.Context, _ []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls != 1 {
			return agent.Message{}, fmt.Errorf("valid write declarations required another model turn")
		}
		found := false
		for _, definition := range definitions {
			if definition.Name != "graph_action" {
				continue
			}
			var schema struct {
				Properties struct {
					Payload struct {
						Properties map[string]struct {
							Type     string `json:"type"`
							MaxItems int    `json:"maxItems"`
							Items    struct {
								Type string `json:"type"`
							} `json:"items"`
						} `json:"properties"`
					} `json:"payload"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(definition.Schema, &schema); err != nil {
				return agent.Message{}, err
			}
			field := schema.Properties.Payload.Properties["write_paths"]
			if field.Type != "array" || field.Items.Type != "string" || field.MaxItems != 16 {
				return agent.Message{}, fmt.Errorf("provider received an incomplete write_paths schema: %+v", field)
			}
			found = true
		}
		if !found {
			return agent.Message{}, fmt.Errorf("provider did not receive graph_action")
		}
		message := draftModelCall("android", "graph_action", `{"op":"step","idempotency_key":"android","payload":{"action":"add","from":["origin"],"description":"Audit Android","write_paths":["/workspace/release-audit/android"]}}`)
		message.Content = append(message.Content, draftModelCall("linux", "graph_action", `{"op":"step","idempotency_key":"linux","payload":{"action":"add","from":["origin"],"description":"Audit Linux","write_paths":["/workspace/release-audit/linux"]}}`).Content...)
		message.Content = append(message.Content, draftModelCall("delivery", "graph_action", `{"op":"step","idempotency_key":"delivery","payload":{"action":"add","from":["origin"],"description":"Combine accepted platform results","write_paths":["/workspace/release-audit/report.json"],"depends_on":["$android","$linux"]}}`).Content...)
		message.Content = append(message.Content, draftModelCall("commit", "graph_action", `{"op":"commit","idempotency_key":"commit","payload":{}}`).Content...)
		return message, nil
	})
	result, err := Run(ctx, job, Options{RunDir: runDir, Provider: provider, Output: bridge})
	if err != nil || result.Status != "success" || calls != 1 || commits != 1 || !committed.Committed {
		t.Fatalf("write declarations did not commit through the Worker: %+v err=%v calls=%d commits=%d", result, err, calls, commits)
	}
	if err := store.Do(ctx, func(tx *board.Tx) error {
		state, err := tx.State(job.Graph.Project.ID)
		if err != nil {
			return err
		}
		if len(state.Steps) != 3 {
			return fmt.Errorf("expected three persisted Steps, got %d", len(state.Steps))
		}
		for alias, want := range wantPaths {
			var saved *board.Step
			for n := range state.Steps {
				if state.Steps[n].ID == committed.IDs[alias] {
					saved = &state.Steps[n]
				}
			}
			if saved == nil || !reflect.DeepEqual(saved.WritePaths, want) {
				return fmt.Errorf("persisted Step %s lost declared output scope: %+v", alias, saved)
			}
			if alias == "delivery" {
				if !reflect.DeepEqual(saved.DependsOn, []string{committed.IDs["android"], committed.IDs["linux"]}) {
					return fmt.Errorf("delivery lost resolved prerequisite IDs: %v", saved.DependsOn)
				}
			} else if len(saved.DependsOn) != 0 {
				return fmt.Errorf("independent platform %s was serialized: %v", alias, saved.DependsOn)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
