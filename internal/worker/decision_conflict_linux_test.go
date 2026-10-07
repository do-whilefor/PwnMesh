//go:build linux

package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

// A rejected draft can be rebuilt in the same session only after a subsequent
// model request observes refreshed evidence. Its original budget never resets.
func TestDecisionConflictRefreshesInSameSessionWithoutReusingDraft(t *testing.T) {
	for _, operation := range []string{"preview", "commit"} {
		t.Run(operation, func(t *testing.T) {
			job, runDir := draftRunJob(t), t.TempDir()
			job.Budget.Timeout = 300
			start := time.Now()
			now := start
			calls, writes, reads, receipts := 0, 0, 0, 0
			current := strings.Repeat("b", 64)
			const conflict = "state_changed: new execution evidence changed this decision input"
			bridge := &draftTestBridge{dir: runDir, handle: func(request GraphRequest) (any, error) {
				switch request.Op {
				case "decision_receipt":
					receipts++
					return board.DecisionReceipt{}, nil
				case "read_graph":
					reads++
					return map[string]any{"state_version": current, "items": []map[string]string{{"id": "corrected", "description": "Use the corrected observation"}}}, nil
				case "decision_preview", "decision_commit":
					writes++
					if writes == 1 {
						if request.Op != "decision_"+operation || request.Batch.ExpectedVersion != job.Decision.StateVersion || len(request.Batch.Actions) != 1 {
							t.Fatalf("draft lost its original input: %+v", request)
						}
						return nil, errors.New(conflict)
					}
					if calls != 2 || writes != 2 || reads != 2 || request.Op != "decision_commit" || request.Batch.ExpectedVersion != current || len(request.Batch.Actions) != 1 || !strings.Contains(string(request.Batch.Actions[0].Payload), "corrected") {
						t.Fatalf("stale or unseen evidence reached commit: calls=%d reads=%d request=%+v", calls, reads, request)
					}
					return board.DecisionReceipt{Committed: true, StateVersion: current, ChangedActions: 1}, nil
				default:
					t.Fatalf("unexpected operation: %s", request.Op)
					return nil, nil
				}
			}}
			provider := scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				now = start.Add(284 * time.Second)
				var messages []agent.Message
				if calls == 1 {
					messages = []agent.Message{
						draftModelCall("stage", "graph_action", `{"op":"step","idempotency_key":"old","payload":{"action":"add","from":["origin"],"description":"Follow old input"}}`),
						draftModelCall("publish", "graph_action", `{"op":"`+operation+`","idempotency_key":"publish","payload":{}}`),
						draftModelCall("blind", "graph_action", `{"op":"step","idempotency_key":"blind","payload":{"action":"add","from":["origin"],"description":"Blindly repeat old input"}}`),
						draftModelCall("overview", "read_graph", `{"section":"overview"}`),
						draftModelCall("facts", "read_graph", `{"section":"facts","ids":["corrected"]}`),
						draftModelCall("unseen", "graph_action", `{"op":"step","idempotency_key":"unseen","payload":{"action":"add","from":["origin"],"description":"Use unseen results"}}`),
						draftModelCall("unseen-commit", "graph_action", `{"op":"commit","idempotency_key":"unseen-commit","payload":{}}`),
					}
				} else if calls == 2 {
					last := history[len(history)-1]
					if last.Role != "user" || len(last.Content) != 7 {
						t.Fatalf("conflicted tool group was not fully settled: %+v", last)
					}
					for _, index := range []int{1, 2, 5, 6} {
						if !last.Content[index].IsError {
							t.Fatalf("preplanned action %d escaped conflict fencing", index)
						}
					}
					messages = []agent.Message{
						draftModelCall("new-stage", "graph_action", `{"op":"step","idempotency_key":"new","payload":{"action":"add","from":["corrected"],"description":"Check the corrected input"}}`),
						draftModelCall("new-commit", "graph_action", `{"op":"commit","idempotency_key":"new-commit","payload":{}}`),
					}
				} else {
					t.Fatal("committed receipt requested another model call")
				}
				message := messages[0]
				for _, next := range messages[1:] {
					message.Content = append(message.Content, next.Content...)
				}
				return message, nil
			})
			opts := Options{Provider: provider, RunDir: runDir, Output: bridge, Now: func() time.Time { return now }}
			result, err := runTestWorker(context.Background(), job, opts)
			if err != nil || result.Status != "success" || calls != 2 || writes != 2 || receipts != 1 {
				t.Fatalf("same-session conflict recovery failed: %+v err=%v calls=%d writes=%d", result, err, calls, writes)
			}
			if result.Metrics == nil || result.Metrics.StateChanged != 1 || !result.Metrics.Committed {
				t.Fatalf("metrics lost rejected and rebuilt transactions: %+v", result.Metrics)
			}
			saved := outcomeSession(t, runDir)
			deadline := start.Add(300 * time.Second)
			if saved.DecisionConflict != "" || !saved.ExecutionDeadline.Equal(deadline) || !saved.ReasonDeadline.Equal(deadline) || saved.RepairCount != 0 || saved.RecoveryCount != 0 {
				t.Fatal("conflict changed identity, budget or recovery allowance")
			}
			if replay, err := runTestWorker(context.Background(), job, opts); err != nil || replay.Status != "success" || calls != 2 || writes != 2 {
				t.Fatalf("successful replay repeated model work: %+v err=%v", replay, err)
			}
		})
	}
}
