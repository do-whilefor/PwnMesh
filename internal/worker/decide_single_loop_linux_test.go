//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func TestDecideUsesOnePlannerDespiteRetiredShadowSettings(t *testing.T) {
	for _, setting := range []string{"PWNMESH_REPLAN_SHADOW", "XLOOM_REPLAN_SHADOW"} {
		for _, resume := range []bool{false, true} {
			t.Run(setting+map[bool]string{false: "/fresh", true: "/resume"}[resume], func(t *testing.T) {
				t.Setenv("PWNMESH_REPLAN_SHADOW", "")
				if err := os.Unsetenv("PWNMESH_REPLAN_SHADOW"); err != nil {
					t.Fatal(err)
				}
				t.Setenv(setting, "1")
				job, dir := draftRunJob(t), t.TempDir()
				// This changes-mode input with an open task used to trigger the
				// optional shadow Loop before the real planner.
				job.Graph.Intents = []board.Intent{{ID: "open", From: []string{"origin"}, Description: "Inspect fixture"}}
				job.State.Graph, job.State.Revision = job.Graph, 1
				job.State.Steps = []board.Step{{ID: "open", From: []string{"origin"}, Description: "Inspect fixture", Status: "open"}}
				cursor := &board.DecisionCursor{ProjectID: job.Graph.Project.ID, Generation: job.Graph.Project.Generation}
				decision, err := board.BuildDecisionContextFromCursor(*job.State, cursor, []board.StateEvent{{Revision: 1, Op: "step", ID: "open"}}, board.DefaultContextViewBytes)
				if err != nil || decision.Mode != "changes" {
					t.Fatalf("fixture needs a bound changes view: %+v %v", decision, err)
				}
				decision.Version, job.Decision = 2, decision
				wantCalls := 1
				if resume {
					seedHistoricalShadowSession(t, job, dir)
					wantCalls++ // Recovery rereads evidence before staging a new draft.
				}
				calls, commits := 0, 0
				bridge := &draftTestBridge{dir: dir, handle: func(request GraphRequest) (any, error) {
					switch request.Op {
					case "decision_receipt":
						return board.DecisionReceipt{}, nil
					case "read_graph":
						return map[string]any{"state_version": job.Decision.StateVersion, "items": []board.Fact{{ID: "origin", Description: "Current fixture scope"}}}, nil
					case "decision_commit":
						commits++
						if request.Batch == nil || len(request.Batch.Actions) != 1 || request.Batch.Actions[0].Op != "step" {
							t.Fatalf("normal planning action was lost: %+v", request.Batch)
						}
						return board.DecisionReceipt{Committed: true, StateVersion: job.Decision.StateVersion, ChangedActions: 1}, nil
					default:
						t.Fatalf("unexpected graph operation: %s", request.Op)
						return nil, nil
					}
				}}
				provider := scenarioProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
					calls++
					if calls > wantCalls || len(history) == 0 || (!resume && calls == 1 && len(history) != 1) {
						t.Fatalf("extra Loop or historical private transcript reached planning: calls=%d history=%+v", calls, history)
					}
					for _, message := range history {
						if strings.Contains(message.Text(), "private shadow") {
							t.Fatal("historical shadow transcript reached the planner")
						}
					}
					planner := false
					for _, definition := range definitions {
						planner = planner || definition.Name == "graph_action"
					}
					if !planner {
						t.Fatal("reason started a read-only observer instead of the planner")
					}
					if resume && calls == 1 {
						message := draftModelCall("overview", "read_graph", `{"section":"overview"}`)
						message.Content = append(message.Content, draftModelCall("evidence", "read_graph", `{"section":"facts","ids":["origin"]}`).Content...)
						return message, nil
					}
					message := draftModelCall("stage", "graph_action", `{"op":"step","idempotency_key":"inspect","payload":{"action":"add","from":["origin"],"description":"Inspect new evidence"}}`)
					message.Content = append(message.Content, draftModelCall("commit", "graph_action", `{"op":"commit","idempotency_key":"commit"}`).Content...)
					return message, nil
				})
				result, err := runTestWorker(context.Background(), job, Options{RunDir: dir, Provider: provider, Output: bridge})
				if err != nil || result.Status != "success" || calls != wantCalls || commits != 1 || result.Metrics == nil || result.Metrics.Outcome != "actions_committed" {
					t.Fatalf("single planner did not commit: %+v err=%v calls=%d commits=%d", result, err, calls, commits)
				}
				state := outcomeSession(t, dir)
				if state.ContextCheckpoint == nil || state.ContextCheckpoint.LastSequence >= 999 {
					t.Fatal("historical shadow sequence contaminated the planner checkpoint")
				}
				raw, err := os.ReadFile(filepath.Join(dir, "session.json"))
				if err != nil || strings.Contains(string(raw), `"replan"`) {
					t.Fatalf("retired experiment receipt remained in active session: %v", err)
				}
			})
		}
	}
}

func seedHistoricalShadowSession(t *testing.T, job Job, dir string) {
	t.Helper()
	identity, err := identityFor(job, dir)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.file.Close()
	message := agent.Text("assistant", "private shadow transcript")
	message.Sequence = 999
	if err := journal.append(agent.Event{Type: "replan_message_end", Message: &message}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	state := session{SchemaVersion: sessionSchemaVersion, Identity: identity, RunID: job.RunID, Kind: job.Kind, StartedAt: start, ExecutionDeadline: start.Add(time.Duration(job.Budget.Timeout) * time.Second), GraphVersion: job.Decision.StateVersion}
	if err := state.save(dir, journal); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	// Historical fields are ignored on read, without changing the bound job
	// identity or the checksummed event log.
	raw = append(raw[:len(raw)-1], []byte(`,"replan":{"mode":"shadow","status":"running","fallback":"decide"}}`)...)
	if err := os.WriteFile(filepath.Join(dir, "session.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalDecisionResultIgnoresRetiredShadowReceipt(t *testing.T) {
	raw := []byte(`{"type":"result","status":"success","text":"saved outcome","metrics":{"version":1,"model_calls":3,"outcome":"actions_observed","replan":{"mode":"shadow","status":"judged","judgment":{"decision":"keep","basis":["f001"],"missing":[]}}}}`)
	var result Result
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "success" || result.Text != "saved outcome" || result.Metrics == nil || result.Metrics.ModelCalls != 3 || result.Metrics.Outcome != "actions_observed" {
		t.Fatalf("historical result lost its outcome or accounting: %+v", result)
	}
}
