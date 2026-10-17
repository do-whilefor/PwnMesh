package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSummaryTruncationRetriesOnceWithinSavedBudget(t *testing.T) {
	for _, stop := range []string{"max_tokens", "length"} {
		t.Run(stop, func(t *testing.T) {
			loop := summaryTestLoop(nil)
			original := cloneMessages(loop.History)
			requests, savedAllowance, observedBytes := 0, 0, 0
			var usage []Usage
			loop.ObserveRequests = true
			loop.SummaryMaxTokens = 1024
			loop.Emit = func(event Event) {
				if event.Type == "model_call_start" {
					observedBytes = event.Request.InputBytes
					if event.Request.InputBasis != "provider_request" {
						t.Fatal("custom request sizing was not observed")
					}
				}
				if event.Type == "model_call_end" {
					usage = append(usage, *event.Request.Usage)
				}
			}
			loop.SaveState = func(history []Message, checkpoint *ContextCheckpoint) error {
				savedAllowance = checkpoint.SummaryRetries
				if checkpoint.CompactionCount == 0 && (!reflect.DeepEqual(history, original) || savedAllowance != 1) {
					t.Fatal("retry modified history or did not consume its allowance")
				}
				return nil
			}
			p := &measuringProvider{measurements: map[string]int{}}
			loop.Provider = p
			p.summary = func(ctx context.Context, messages []Message, limit int, _ Emit) (Message, error) {
				requests++
				if limit != 1024 || ctx.Err() != nil {
					t.Fatal("retry changed its budget")
				}
				if size, err := loop.inputBytes(messages, nil); err != nil || size > loop.ContextBytes {
					t.Fatalf("retry exceeds input allowance: %d %v", size, err)
				}
				if observedBytes != len(requestMeasurement(messages, nil)) {
					t.Fatal("retry observation used stale request size")
				}
				message := Text("assistant", `{"notes":"Continue.","quotes":[]}`)
				message.Usage = &Usage{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 4, CacheWriteTokens: 5}
				if requests == 1 {
					message.StopReason = stop
				} else if savedAllowance != 1 || !strings.Contains(messages[0].Text(), "previous summary hit") {
					t.Fatal("retry was not durably authorized or lacks rewrite guidance")
				}
				return message, nil
			}
			if _, err := loop.compactRequest(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			if requests != 2 || savedAllowance != 0 || loop.Checkpoint.SummaryRetries != 0 || len(usage) != 2 || *loop.Checkpoint.LastCompaction.Usage != (Usage{InputTokens: 20, OutputTokens: 6, CacheReadTokens: 8, CacheWriteTokens: 10}) {
				t.Fatalf("incorrect bounded retry or usage: requests=%d saved=%d usage=%+v checkpoint=%+v", requests, savedAllowance, usage, loop.Checkpoint)
			}
		})
	}
}

func TestSummaryRetryFailurePreservesHistoryAndConsumedAllowance(t *testing.T) {
	for _, scenario := range []string{"repeated_truncation", "save_failure", "cancelled", "retry_transport", "commit_failure"} {
		t.Run(scenario, func(t *testing.T) {
			loop := summaryTestLoop(nil)
			original := cloneMessages(loop.History)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requests := 0
			var saved []byte
			type state struct {
				History    []Message
				Checkpoint *ContextCheckpoint
			}
			failure := errors.New("interrupted")
			loop.SaveState = func(history []Message, checkpoint *ContextCheckpoint) error {
				if scenario == "save_failure" || (scenario == "commit_failure" && checkpoint.CompactionCount > 0) {
					return failure
				}
				var err error
				saved, err = json.Marshal(state{history, checkpoint})
				return err
			}
			loop.Provider = summaryTestProvider(func([]Message) (Message, error) {
				requests++
				message := Text("assistant", `{"notes":"Continue.","quotes":[]}`)
				if requests == 1 || scenario == "repeated_truncation" {
					message.StopReason = "max_tokens"
				}
				if scenario == "cancelled" {
					cancel()
				}
				if requests == 2 && scenario == "retry_transport" {
					return message, failure
				}
				return message, nil
			})
			if _, err := loop.compactRequest(ctx, false); err == nil {
				t.Fatal("failure was accepted")
			}
			wantRequests := 2
			if scenario == "save_failure" || scenario == "cancelled" {
				wantRequests = 1
			}
			if requests != wantRequests || !reflect.DeepEqual(original, loop.History) || loop.Checkpoint.CompactionCount != 0 {
				t.Fatalf("failed recovery changed history or exceeded allowance: requests=%d", requests)
			}
			if scenario == "save_failure" && loop.Checkpoint.SummaryRetries != 0 {
				t.Fatal("failed save consumed the in-memory retry allowance")
			}
			if len(saved) == 0 {
				return
			}
			var restored state
			if err := json.Unmarshal(saved, &restored); err != nil {
				t.Fatal(err)
			}
			loop.History, loop.Checkpoint = restored.History, restored.Checkpoint
			if _, err := loop.compactRequest(context.Background(), false); err == nil || requests != wantRequests || loop.Checkpoint.SummaryRetries != 1 {
				t.Fatalf("restart replenished the consumed retry: requests=%d error=%v", requests, err)
			}
		})
	}
}

func TestContextOverflowRecoveryRenewsAfterDurableResponse(t *testing.T) {
	for _, scenario := range []string{"accepted", "truncated", "save_failure"} {
		t.Run(scenario, func(t *testing.T) {
			loop := summaryTestLoop(nil)
			loop.ContextBytes = 1 << 20
			loop.Checkpoint.OverflowRetries = 1
			boundary := errors.New("stop at saved response")
			var saved []byte
			type state struct {
				History    []Message
				Checkpoint *ContextCheckpoint
			}
			loop.SaveState = func(history []Message, checkpoint *ContextCheckpoint) error {
				if scenario == "save_failure" {
					return boundary
				}
				var err error
				saved, err = json.Marshal(state{history, checkpoint})
				return err
			}
			loop.Provider = summaryTestProvider(func([]Message) (Message, error) {
				message := Text("assistant", "The retained context was accepted.")
				if scenario == "truncated" {
					message.StopReason = "max_tokens"
				}
				return message, nil
			})
			loop.OnTurnEnd = func(context.Context, *Loop, Message) (context.Context, string, error) {
				return nil, "", boundary
			}
			if _, err := loop.Run(context.Background(), ""); !errors.Is(err, boundary) {
				t.Fatalf("unexpected first boundary: %v", err)
			}
			wantAllowance := 1
			if scenario == "accepted" {
				wantAllowance = 0
			}
			if loop.Checkpoint.OverflowRetries != wantAllowance {
				t.Fatalf("overflow allowance after %s response = %d, want %d", scenario, loop.Checkpoint.OverflowRetries, wantAllowance)
			}
			if scenario != "accepted" {
				return
			}
			var restored state
			if err := json.Unmarshal(saved, &restored); err != nil {
				t.Fatal(err)
			}
			turns, summaries := 0, 0
			restarted := &Loop{History: restored.History, Checkpoint: restored.Checkpoint, TaskPrompt: loop.TaskPrompt,
				SummaryBytes: 4096, RecentBytes: 128,
				Provider: observedProvider{
					generate: func(context.Context, []Message, []Definition, Emit) (Message, error) {
						turns++
						if turns == 1 {
							return Message{}, &ModelError{Kind: ErrorContextOverflow, Err: errors.New("a later context overflow")}
						}
						return Text("assistant", "Recovered from the later overflow."), nil
					},
					summary: func(context.Context, []Message, int, Emit) (Message, error) {
						summaries++
						return Text("assistant", `{"notes":"Continue from the retained evidence.","quotes":[]}`), nil
					},
				},
			}
			if _, err := restarted.Run(context.Background(), ""); err != nil || turns != 2 || summaries != 1 || restarted.Checkpoint.OverflowRetries != 0 {
				t.Fatalf("later independent overflow could not recover: err=%v turns=%d summaries=%d checkpoint=%+v", err, turns, summaries, restarted.Checkpoint)
			}
		})
	}
}
