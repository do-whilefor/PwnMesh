//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func observationCall(name, input string, turn int) agent.Message {
	return agent.Message{Role: "assistant", StopReason: "tool_use", Content: []agent.Block{{Type: "tool_use", ID: fmt.Sprintf("observation-%d", turn), Name: name, Input: json.RawMessage(input)}}}
}

func observationTool(name string, calls *int, fail bool) agent.Tool {
	return agent.Tool{Definition: agent.Definition{Name: name, Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(context.Context, json.RawMessage) (string, error) {
		*calls++
		if fail {
			return "", errors.New("same blocker")
		}
		return "same observation", nil
	}}
}

func correctionCount(history []agent.Message) int {
	count := 0
	for _, message := range history {
		if message.Role == "user" && strings.HasPrefix(message.Text(), "Recent tools have repeated") {
			count++
		}
	}
	return count
}

func TestToolProgressStopsRepeatedErrorsAndUnchangedReads(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail bool
	}{{"probe", true}, {"read", false}, {"read_snapshot", false}} {
		t.Run(tc.name, func(t *testing.T) {
			job, dir := outcomeJob(t, "explore"), t.TempDir()
			job.Budget.Timeout = 0 // The progress bound works without a task timeout.
			turns, calls := 0, 0
			provider := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
				turns++
				if turns > maxRepeatedToolTurns+1 {
					return agent.Message{}, errors.New("test guard: stalled Worker requested another model turn")
				}
				return observationCall(tc.name, `{}`, turns), nil
			})
			result, err := runTestWorker(context.Background(), job, Options{Provider: provider, Tools: []agent.Tool{observationTool(tc.name, &calls, tc.fail)}, RunDir: dir})
			saved := outcomeSession(t, dir)
			if err != nil || result.FailureKind != "tool_no_progress" || result.Retryable || turns != maxRepeatedToolTurns+1 || calls != turns || saved.ToolProgress.Repeated != maxRepeatedToolTurns || correctionCount(saved.History) != 1 {
				t.Fatalf("result=%+v err=%v turns=%d calls=%d progress=%+v corrections=%d", result, err, turns, calls, saved.ToolProgress, correctionCount(saved.History))
			}
		})
	}
}

func TestControlRoleProgressCorrectionKeepsRejectionContract(t *testing.T) {
	for _, kind := range []string{"reason", "curate"} {
		t.Run(kind, func(t *testing.T) {
			job := draftRunJob(t)
			if kind == "curate" {
				job = curationJob(t)
			}
			dir, calls := t.TempDir(), 0
			bridge := &draftTestBridge{dir: dir, handle: func(request GraphRequest) (any, error) {
				switch request.Op {
				case "decision_receipt":
					return board.DecisionReceipt{}, nil
				case "curate_receipt":
					return board.StateActionResult{}, nil
				case "read_graph":
					return map[string]any{"state_version": job.Decision.StateVersion, "items": []any{}}, nil
				default:
					t.Fatalf("stalled control task unexpectedly wrote to the graph: %s", request.Op)
					return nil, nil
				}
			}}
			provider := scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				if calls > maxRepeatedToolTurns+1 {
					t.Fatal("stalled control task never received a progress correction")
				}
				last := history[len(history)-1].Text()
				if strings.HasPrefix(last, "Recent tools have repeated") {
					if !strings.Contains(last, "Reason and Curate return accepted:false with a reason") {
						t.Fatal("progress correction omitted the control role rejection contract")
					}
					return agent.Text("assistant", `{"accepted":false,"reason":"No new evidence is available"}`), nil
				}
				return observationCall("read_graph", `{"section":"overview"}`, calls), nil
			})
			result, err := runTestWorker(context.Background(), job, Options{RunDir: dir, Output: bridge, Provider: provider})
			saved := outcomeSession(t, dir)
			if err != nil || result.Status != "success" || correctionCount(saved.History) != 1 || saved.RepairCount != 0 || saved.ContinuationCount != 0 {
				t.Fatalf("control rejection required repair or continuation: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestToolProgressAllowsNewFailureInformationButBoundsAllFailure(t *testing.T) {
	job, dir := outcomeJob(t, "explore"), t.TempDir()
	turns, calls := 0, 0
	provider := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		turns++
		if turns > maxFailedToolTurns {
			return agent.Message{}, errors.New("test guard: all-failure allowance was reset")
		}
		return observationCall("probe", fmt.Sprintf(`{"attempt":%d}`, turns), turns), nil
	})
	result, err := runTestWorker(context.Background(), job, Options{Provider: provider, Tools: []agent.Tool{observationTool("probe", &calls, true)}, RunDir: dir})
	saved := outcomeSession(t, dir)
	if err != nil || result.FailureKind != "tool_no_progress" || calls != maxFailedToolTurns || saved.ToolProgress.Repeated != 0 || saved.ToolProgress.Failed != maxFailedToolTurns || correctionCount(saved.History) != 1 {
		t.Fatalf("result=%+v err=%v calls=%d progress=%+v", result, err, calls, saved.ToolProgress)
	}
}

func TestToolArgumentCorrectionsDoNotResetRepeatedReadLimit(t *testing.T) {
	job, dir := outcomeJob(t, "explore"), t.TempDir()
	job.Budget.Timeout = 0
	turns, calls := 0, 0
	provider := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		turns++
		if turns > 2*(maxRepeatedToolTurns+1) {
			return agent.Message{}, errors.New("test guard: argument corrections bypassed the progress bound")
		}
		if turns%2 == 1 {
			return agent.Message{}, &agent.ModelError{Kind: agent.ErrorToolArguments, Err: errors.New("invalid JSON")}
		}
		return observationCall("read", `{}`, turns), nil
	})
	result, err := runTestWorker(context.Background(), job, Options{Provider: provider, Tools: []agent.Tool{observationTool("read", &calls, false)}, RunDir: dir})
	saved := outcomeSession(t, dir)
	if err != nil || result.FailureKind != "tool_no_progress" || result.Retryable || turns != 2*(maxRepeatedToolTurns+1) || calls != maxRepeatedToolTurns+1 || saved.ToolProgress.Repeated != maxRepeatedToolTurns || correctionCount(saved.History) != 1 {
		t.Fatalf("argument corrections bypassed progress guard: result=%+v err=%v turns=%d calls=%d progress=%+v", result, err, turns, calls, saved.ToolProgress)
	}
}

func TestToolProgressDoesNotLimitNewReadsWritesOrMixedUsefulBatches(t *testing.T) {
	for _, mode := range []string{"new read arguments", "changed read output", "opaque writes", "mixed batch"} {
		t.Run(mode, func(t *testing.T) {
			job, dir := outcomeJob(t, "explore"), t.TempDir()
			turns, calls, failures := 0, 0, 0
			read := observationTool("read", &calls, false)
			if mode == "changed read output" {
				read.Execute = func(context.Context, json.RawMessage) (string, error) {
					calls++
					return fmt.Sprintf("new observation %d", calls), nil
				}
			}
			provider := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
				turns++
				if turns == maxFailedToolTurns+2 {
					return agent.Text("assistant", completedOutput("explore")), nil
				}
				name, input := "read", `{}`
				if mode == "new read arguments" || mode == "mixed batch" {
					input = fmt.Sprintf(`{"offset":%d}`, turns)
				}
				if mode == "opaque writes" {
					name = "write"
				}
				message := observationCall(name, input, turns)
				if mode == "mixed batch" {
					failed := observationCall("probe", `{}`, turns)
					failed.Content[0].ID += "-failed"
					message.Content = append(message.Content, failed.Content...)
				}
				return message, nil
			})
			result, err := runTestWorker(context.Background(), job, Options{RunDir: dir, Provider: provider, Tools: []agent.Tool{read, observationTool("write", &calls, false), observationTool("probe", &failures, true)}})
			saved := outcomeSession(t, dir)
			if err != nil || result.Status != "success" || calls != maxFailedToolTurns+1 || saved.ToolProgress.Repeated != 0 || saved.ToolProgress.Failed != 0 || correctionCount(saved.History) != 0 {
				t.Fatalf("useful execution was limited: result=%+v err=%v calls=%d progress=%+v", result, err, calls, saved.ToolProgress)
			}
		})
	}
}

func TestToolProgressSurvivesSameRunTransportRecovery(t *testing.T) {
	job, dir := outcomeJob(t, "explore"), t.TempDir()
	turns, calls := 0, 0
	recovering := false
	provider := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		turns++
		if !recovering && calls == maxRepeatedToolTurns-1 {
			return agent.Message{}, &agent.ModelError{Kind: agent.ErrorTransport, Err: errors.New("fixture interrupted")}
		}
		if calls >= maxRepeatedToolTurns+1 {
			return agent.Message{}, errors.New("test guard: recovery replenished progress allowance")
		}
		return observationCall("read", `{}`, turns), nil
	})
	opts := Options{Provider: provider, Tools: []agent.Tool{observationTool("read", &calls, false)}, RunDir: dir}
	first, err := runTestWorker(context.Background(), job, opts)
	before := outcomeSession(t, dir)
	if err != nil || !first.Retryable || before.ToolProgress.Repeated != maxRepeatedToolTurns-2 || !before.ToolProgress.Warned {
		t.Fatalf("first result=%+v err=%v progress=%+v", first, err, before.ToolProgress)
	}
	recovering = true
	result, err := runTestWorker(context.Background(), job, opts)
	after := outcomeSession(t, dir)
	if err != nil || result.FailureKind != "tool_no_progress" || result.Retryable || calls != maxRepeatedToolTurns+1 || after.RecoveryCount != before.RecoveryCount+1 || after.ToolProgress.Repeated != maxRepeatedToolTurns || correctionCount(after.History) != 1 {
		t.Fatalf("resume result=%+v err=%v calls=%d before=%+v after=%+v", result, err, calls, before.ToolProgress, after.ToolProgress)
	}
}

func settledObservation(sequence uint64, name, input, output string, failed bool) []agent.Message {
	message := observationCall(name, input, int(sequence))
	message.Sequence = sequence
	raw, _ := json.Marshal(output)
	return []agent.Message{message, {Role: "user", Sequence: sequence + 1, Content: []agent.Block{{Type: "tool_result", ToolUseID: message.Content[0].ID, Content: raw, IsError: failed}}}}
}

func TestToolProgressCheckpointIdempotenceAndBoundedObservations(t *testing.T) {
	var progress toolProgress
	history := settledObservation(1, "read", `{"a":1,"b":2}`, "same", false)
	if useful, err := progress.settle(history); err != nil || !useful {
		t.Fatalf("first read=%t %v", useful, err)
	}
	for n := uint64(3); n < 11; n += 2 {
		if _, err := progress.settle(settledObservation(n, "read", `{ "b": 2, "a": 1 }`, "same", false)); err != nil {
			t.Fatal(err)
		}
	}
	before := progress.Repeated
	raw, _ := json.Marshal(progress)
	var restored toolProgress
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.settle(settledObservation(9, "read", `{"a":1,"b":2}`, "same", false)); err != nil || restored.Repeated != before {
		t.Fatalf("same settled sequence was consumed twice: %+v %v", restored, err)
	}
	for n := uint64(11); n < 11+2*maxToolObservations+10; n += 2 {
		if _, err := restored.settle(settledObservation(n, "read", fmt.Sprintf(`{"page":%d}`, n), "page", false)); err != nil {
			t.Fatal(err)
		}
	}
	if len(restored.Seen) != maxToolObservations || restored.Repeated != 0 || restored.validate(restored.Sequence) != nil {
		t.Fatalf("unbounded or invalid observations: %+v", restored)
	}
}

func TestToolProgressReadRecheckAfterMutationIsUseful(t *testing.T) {
	var progress toolProgress
	for _, history := range [][]agent.Message{
		settledObservation(1, "read", `{}`, "same", false),
		settledObservation(3, "write", `{}`, "done", false),
		settledObservation(5, "read", `{}`, "same", false),
	} {
		if useful, err := progress.settle(history); err != nil || !useful || progress.Repeated != 0 {
			t.Fatalf("mutation/read verification did not count as progress: useful=%t state=%+v err=%v", useful, progress, err)
		}
	}
}

func TestToolProgressResumeAfterSettledSaveCannotBuyAnotherTurn(t *testing.T) {
	job, dir := outcomeJob(t, "explore"), t.TempDir()
	identity, err := identityFor(job, dir)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.file.Close()
	task := agent.Text("user", "Original task")
	task.Sequence = 1
	saved := session{SchemaVersion: sessionSchemaVersion, Identity: identity, RunID: job.RunID, Kind: job.Kind, StartedAt: time.Now(), TaskPrompt: task.Text(), History: []agent.Message{task}}
	for n := 0; n <= maxRepeatedToolTurns; n++ {
		saved.History = append(saved.History, settledObservation(uint64(2+n*2), "read", `{}`, "same", false)...)
		if _, err := saved.ToolProgress.settle(saved.History); err != nil {
			t.Fatal(err)
		}
	}
	saved.ContextCheckpoint = &agent.ContextCheckpoint{Version: agent.ContextCheckpointVersion, LastSequence: saved.History[len(saved.History)-1].Sequence}
	for _, message := range saved.History {
		if err := journal.append(agent.Event{Type: "message_end", Message: &message}); err != nil {
			t.Fatal(err)
		}
	}
	// This is the crash boundary: tool results and the consumed allowance are
	// durable, but OnTurnEnd has not emitted a failure or terminal result yet.
	if err := saved.save(dir, journal); err != nil {
		t.Fatal(err)
	}
	turns, calls := 0, 0
	result, err := runTestWorker(context.Background(), job, Options{RunDir: dir, Tools: []agent.Tool{observationTool("read", &calls, false)}, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		turns++
		return agent.Text("assistant", completedOutput("explore")), nil
	})})
	if err != nil || result.FailureKind != "tool_no_progress" || turns != 0 || calls != 0 {
		t.Fatalf("crash bought another turn or tool execution: result=%+v err=%v turns=%d calls=%d", result, err, turns, calls)
	}
	if after := outcomeSession(t, dir); after.ToolProgress.Repeated != maxRepeatedToolTurns || after.ToolProgress.Sequence != saved.ToolProgress.Sequence {
		t.Fatalf("crash consumed or restored allowance: before=%+v after=%+v", saved.ToolProgress, after.ToolProgress)
	}
}

func TestToolProgressStillAllowsSoftStopConclusion(t *testing.T) {
	job, dir := outcomeJob(t, "explore"), t.TempDir()
	stop := make(chan struct{})
	turns, calls := 0, 0
	provider := scenarioProvider(func(_ context.Context, _ []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turns++
		if turns == maxRepeatedToolTurns+1 {
			close(stop)
		}
		if turns > maxRepeatedToolTurns+1 {
			if len(definitions) != 0 {
				t.Fatal("soft-stop conclusion retained execution tools")
			}
			return agent.Text("assistant", incompleteOutput), nil
		}
		return observationCall("read", `{}`, turns), nil
	})
	result, err := runTestWorker(context.Background(), job, Options{RunDir: dir, SoftStop: stop, Provider: provider, Tools: []agent.Tool{observationTool("read", &calls, false)}})
	if err != nil || result.FailureKind != "incomplete" || !result.Conclude || turns != maxRepeatedToolTurns+2 {
		t.Fatalf("progress guard prevented the bounded soft-stop conclusion: result=%+v err=%v turns=%d", result, err, turns)
	}
}
