//go:build linux

package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

// Providers can return tool-shaped XML as ordinary assistant text. It must
// remain inert even when every embedded argument resembles a valid tool call.
const textToolCalls = `<function_calls><invoke name="assess_root"><parameter name="status">satisfied</parameter><parameter name="from">["f001"]</parameter><parameter name="description">Retained observation satisfies the task.</parameter></invoke><invoke name="read_graph"><parameter name="section">facts</parameter></invoke><invoke name="graph_action"><parameter name="op">commit</parameter><parameter name="idempotency_key">finish</parameter></invoke></function_calls>`

func nativeContinuationJob(t *testing.T, kind string) Job {
	t.Helper()
	if kind == "curate" {
		return curationJob(t)
	}
	job := draftRunJob(t)
	job.State.FactRecords = assessmentFixture("").FactRecords
	var err error
	job.Decision, err = board.BuildDecisionContextFromCursor(*job.State, nil, nil, board.DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	job.Decision.Version, job.Decision.ClosureProtocol = 2, 1
	if err := board.UseCompletionAssessment(*job.State, job.Decision, assessmentFixture(job.Decision.StateVersion)); err != nil || job.Decision.Mode != "completion" || job.Decision.CompletionAssessment == nil {
		t.Fatalf("completion fixture lacks its evidence assessment: %v", err)
	}
	return job
}

func TestControlTextToolCallsAreCorrectedBeforeNativeCommit(t *testing.T) {
	for _, kind := range []string{"reason", "curate"} {
		t.Run(kind, func(t *testing.T) {
			job, dir, start := nativeContinuationJob(t, kind), t.TempDir(), time.Now()
			calls, commits, previews := 0, 0, 0
			version := board.DecisionStateVersion(*job.State)
			bridge := &draftTestBridge{dir: dir, handle: func(request GraphRequest) (any, error) {
				switch request.Op {
				case "decision_receipt":
					return board.DecisionReceipt{}, nil
				case "curate_receipt":
					return board.StateActionResult{}, nil
				case "decision_preview":
					if calls != 3 || request.Batch.Assessment == nil || request.Batch.Assessment.Status != "satisfied" || len(request.Batch.Actions) != 1 {
						t.Fatal("text bypassed the native root assessment and draft")
					}
					previews++
					return assessmentPreview(job.Decision.CompletionAssessment, request.Batch.Actions[0].Payload), nil
				case "decision_commit":
					if calls != 3 || previews != 1 {
						t.Fatal("completion bypassed its native tools and preview")
					}
					commits++
					return board.DecisionReceipt{Committed: true, Completed: true, StateVersion: version}, nil
				case "graph_action":
					if kind != "curate" || calls != 2 || request.Action.Op != "curate" {
						t.Fatal("text caused a graph mutation")
					}
					commits++
					return board.StateActionResult{Committed: true, StateVersion: version}, nil
				default:
					t.Fatalf("text executed an embedded tool: %s", request.Op)
					return nil, nil
				}
			}}
			provider := scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				switch calls {
				case 1:
					return agent.Text("assistant", textToolCalls), nil
				case 2:
					feedback := history[len(history)-1].Text()
					if !strings.Contains(feedback, "native tool calls") || !strings.Contains(feedback, "XML/JSON text is not executed") || commits != 0 || previews != 0 {
						t.Fatalf("missing tool-format correction before effects: %q", feedback)
					}
					saved := outcomeSession(t, dir)
					if saved.ContinuationCount != 1 || !saved.ExecutionDeadline.Equal(start.Add(time.Minute)) || saved.Repairing {
						t.Fatal("tool-format correction changed the continuation or deadline protocol")
					}
					if kind == "curate" {
						return draftModelCall("curate", "graph_action", `{"op":"curate","payload":{"groups":[]}}`), nil
					}
					return draftModelCall("assess", "assess_root", `{"status":"satisfied","from":["f001"],"description":"Retained observation satisfies the task."}`), nil
				case 3:
					message := draftModelCall("finish", "graph_action", `{"op":"complete","idempotency_key":"finish","payload":{"from":["f001"],"description":"Retained observation satisfies the task."}}`)
					message.Content = append(message.Content, draftModelCall("commit", "graph_action", `{"op":"commit","idempotency_key":"commit"}`).Content...)
					return message, nil
				default:
					t.Fatal("commit did not terminate the native tool sequence")
					return agent.Message{}, nil
				}
			})
			result, err := runTestWorker(context.Background(), job, Options{RunDir: dir, Output: bridge, Provider: provider, Now: func() time.Time { return start }})
			wantCalls := 3
			if kind == "curate" {
				wantCalls = 2
			}
			if err != nil || result.Status != "success" || commits != 1 || calls != wantCalls {
				t.Fatalf("native tool correction failed: %+v %v calls=%d commits=%d", result, err, calls, commits)
			}
			if saved := outcomeSession(t, dir); saved.ContinuationCount != 0 || !saved.ExecutionDeadline.Equal(start.Add(time.Minute)) || saved.RepairCount != 0 {
				t.Fatal("native progress changed the existing continuation or deadline policy")
			}
		})
	}
}

func TestRepeatedControlTextToolCallsKeepTheOriginalContinuationLimit(t *testing.T) {
	for _, kind := range []string{"reason", "curate"} {
		t.Run(kind, func(t *testing.T) {
			job, dir, start := nativeContinuationJob(t, kind), t.TempDir(), time.Now()
			calls := 0
			bridge := &draftTestBridge{dir: dir, handle: func(request GraphRequest) (any, error) {
				if request.Op == "decision_receipt" || request.Op == "curate_receipt" {
					return board.StateActionResult{}, nil
				}
				t.Fatalf("text caused a tool side effect: %+v", request)
				return nil, nil
			}}
			provider := scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				if calls > maxContinuations+1 {
					t.Fatal("text tool syntax replenished the continuation allowance")
				}
				if calls > 1 && !strings.Contains(history[len(history)-1].Text(), "XML/JSON text is not executed") {
					t.Fatal("repeated text lost its protocol correction")
				}
				return agent.Text("assistant", textToolCalls), nil
			})
			result, err := runTestWorker(context.Background(), job, Options{RunDir: dir, Output: bridge, Provider: provider, Now: func() time.Time { return start }})
			if err != nil || result.Status != "failed" || result.Retryable || calls != maxContinuations+1 || !strings.Contains(result.Error, "continuation_exhausted") {
				t.Fatalf("text bypassed the original continuation limit: %+v %v calls=%d", result, err, calls)
			}
			if saved := outcomeSession(t, dir); saved.ContinuationCount != maxContinuations || !saved.ExecutionDeadline.Equal(start.Add(time.Minute)) || saved.RepairCount != 0 {
				t.Fatal("text continuation changed the allowance or deadline")
			}
		})
	}
}
