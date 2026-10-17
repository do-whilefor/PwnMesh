package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestToolArgumentRetryPersistsAcrossRestart(t *testing.T) {
	requests, hooks := 0, 0
	var saved []byte
	type session struct {
		History    []Message
		Checkpoint *ContextCheckpoint
	}
	malformed := &ModelError{Kind: ErrorToolArguments, Err: errors.New("invalid JSON")}
	loop := &Loop{
		Provider: beforeRequestProvider{generate: func(context.Context, []Message) (Message, error) {
			requests++
			return Message{}, malformed
		}},
		SaveState: func(history []Message, checkpoint *ContextCheckpoint) error {
			var err error
			saved, err = json.Marshal(session{history, checkpoint})
			return err
		},
		BeforeRequest: func(ctx context.Context, _ *Loop) (context.Context, error) {
			hooks++
			return ctx, nil
		},
	}
	_, err := loop.Run(context.Background(), "task")
	if !errors.Is(err, malformed) || requests != 2 || hooks != 2 {
		t.Fatalf("retry did not recheck the boundary or exceeded allowance: err=%v requests=%d hooks=%d", err, requests, hooks)
	}
	var restored session
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	restarted := &Loop{Provider: loop.Provider, History: restored.History, Checkpoint: restored.Checkpoint}
	_, err = restarted.Run(context.Background(), "")
	if !errors.Is(err, malformed) || requests != 3 || restarted.Checkpoint.ToolArgumentRetries != 1 {
		t.Fatalf("restart granted another retry: err=%v requests=%d", err, requests)
	}
}

func TestToolArgumentRetryStopsAtPersistenceAndRuntimeBoundaries(t *testing.T) {
	for _, scenario := range []string{"save_failure", "cancellation", "runtime_changed", "permanent_provider"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requests, hooks := 0, 0
			boundaryErr := errors.New("runtime boundary failed")
			loop := &Loop{
				Provider: beforeRequestProvider{generate: func(context.Context, []Message) (Message, error) {
					requests++
					if scenario == "cancellation" {
						cancel()
					}
					kind := ErrorToolArguments
					if scenario == "permanent_provider" {
						kind = ErrorProvider
					}
					return Message{}, &ModelError{Kind: kind, Err: errors.New("invalid response")}
				}},
				SaveState: func(_ []Message, checkpoint *ContextCheckpoint) error {
					if checkpoint.ToolArgumentRetries > 0 && scenario == "save_failure" {
						return boundaryErr
					}
					return nil
				},
				BeforeRequest: func(ctx context.Context, _ *Loop) (context.Context, error) {
					hooks++
					if hooks > 1 && scenario == "runtime_changed" {
						return nil, boundaryErr
					}
					return ctx, nil
				},
			}
			_, err := loop.Run(ctx, "task")
			if err == nil || requests != 1 {
				t.Fatalf("retried through a failed boundary: err=%v requests=%d", err, requests)
			}
			if (scenario == "save_failure" || scenario == "runtime_changed") && !errors.Is(err, boundaryErr) {
				t.Fatalf("lost boundary error: %v", err)
			}
		})
	}
}

func TestRetryAllowancesRejectInvalidCheckpoint(t *testing.T) {
	for _, allowance := range []string{"tool_arguments", "context_overflow", "summary"} {
		for _, count := range []int{-1, 2} {
			checkpoint := &ContextCheckpoint{Version: ContextCheckpointVersion, LastSequence: 1}
			if allowance == "tool_arguments" {
				checkpoint.ToolArgumentRetries = count
			} else if allowance == "context_overflow" {
				checkpoint.OverflowRetries = count
			} else {
				checkpoint.SummaryRetries = count
			}
			requests, saves := 0, 0
			message := Text("user", "retained task")
			message.Sequence = 1
			loop := &Loop{
				History: []Message{message}, Checkpoint: checkpoint,
				Provider: beforeRequestProvider{generate: func(context.Context, []Message) (Message, error) {
					requests++
					return Text("assistant", "done"), nil
				}},
				SaveState: func([]Message, *ContextCheckpoint) error { saves++; return nil },
			}
			if _, err := loop.Run(context.Background(), ""); err == nil || requests != 0 || saves != 0 {
				t.Errorf("invalid %s count %d reached runtime: err=%v requests=%d saves=%d", allowance, count, err, requests, saves)
			}
		}
	}
}

func TestConsumedContextOverflowAllowanceSurvivesRestart(t *testing.T) {
	var checkpoint ContextCheckpoint
	if err := json.Unmarshal([]byte(`{"version":1,"last_sequence":1,"overflow_retries":1}`), &checkpoint); err != nil {
		t.Fatal(err)
	}
	message := Text("user", "retained task")
	message.Sequence = 1
	overflow := &ModelError{Kind: ErrorContextOverflow, Err: errors.New("context full")}
	requests, saves := 0, 0
	loop := &Loop{
		History: []Message{message}, Checkpoint: &checkpoint,
		Provider: beforeRequestProvider{generate: func(context.Context, []Message) (Message, error) {
			requests++
			return Message{}, overflow
		}},
		SaveState: func([]Message, *ContextCheckpoint) error { saves++; return nil },
	}
	if _, err := loop.Run(context.Background(), ""); !errors.Is(err, overflow) || requests != 1 || saves != 0 || loop.Checkpoint.OverflowRetries != 1 {
		t.Fatalf("valid retained allowance was rejected or replenished: err=%v requests=%d saves=%d checkpoint=%+v", err, requests, saves, loop.Checkpoint)
	}
}

func TestToolArgumentRetryRenewsAfterDurableSuccessfulTool(t *testing.T) {
	for _, saveFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved_receipt", true: "unsaved_receipt"}[saveFailure], func(t *testing.T) {
			malformed := &ModelError{Kind: ErrorToolArguments, Err: errors.New("invalid JSON")}
			interrupted := errors.New("interrupted at settled boundary")
			requests, effects := 0, 0
			var saved []byte
			type session struct {
				History    []Message
				Checkpoint *ContextCheckpoint
			}
			tool := Tool{Definition: Definition{Name: "work"}, Execute: func(context.Context, json.RawMessage) (string, error) {
				effects++
				return "retained work", nil
			}}
			loop := &Loop{
				Tools: []Tool{tool},
				Provider: beforeRequestProvider{generate: func(context.Context, []Message) (Message, error) {
					requests++
					if requests == 1 {
						return Message{}, malformed
					}
					return Message{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "work-1", Name: "work", Input: json.RawMessage(`{}`)}}}, nil
				}},
				SaveState: func(history []Message, checkpoint *ContextCheckpoint) error {
					if hasResults(history[len(history)-1]) {
						if checkpoint.ToolArgumentRetries != 0 {
							t.Fatal("successful receipt did not replenish its checkpoint atomically")
						}
						if saveFailure {
							return interrupted
						}
					}
					var err error
					saved, err = json.Marshal(session{history, checkpoint})
					return err
				},
				OnTurnEnd: func(context.Context, *Loop, Message) (context.Context, string, error) {
					return nil, "", interrupted
				},
			}
			if _, err := loop.Run(context.Background(), "task"); !errors.Is(err, interrupted) || requests != 2 || effects != 1 {
				t.Fatalf("unexpected first boundary: err=%v requests=%d effects=%d", err, requests, effects)
			}
			if saveFailure && loop.Checkpoint.ToolArgumentRetries != 1 {
				t.Fatal("failed persistence replenished the in-memory allowance")
			}
			var restored session
			if err := json.Unmarshal(saved, &restored); err != nil {
				t.Fatal(err)
			}
			loop = &Loop{Tools: []Tool{tool}, History: restored.History, Checkpoint: restored.Checkpoint,
				Provider: beforeRequestProvider{generate: func(context.Context, []Message) (Message, error) {
					requests++
					return Message{}, malformed
				}},
			}
			_, err := loop.Run(context.Background(), "")
			wantRequests := 4 // A durable success permits one correction of a later error.
			if saveFailure {
				wantRequests = 3 // RepairHistory must not invent a successful receipt.
			}
			if !errors.Is(err, malformed) || requests != wantRequests || effects != 1 || loop.Checkpoint.ToolArgumentRetries != 1 {
				t.Fatalf("wrong resumed allowance or repeated side effect: err=%v requests=%d effects=%d", err, requests, effects)
			}
		})
	}
}

func TestToolArgumentRetryDoesNotRenewWithoutSuccessfulTool(t *testing.T) {
	for _, scenario := range []string{"text", "tool_error", "unknown_tool", "truncated"} {
		t.Run(scenario, func(t *testing.T) {
			malformed := &ModelError{Kind: ErrorToolArguments, Err: errors.New("invalid JSON")}
			requests, effects := 0, 0
			loop := &Loop{
				Tools: []Tool{{Definition: Definition{Name: "work"}, Execute: func(context.Context, json.RawMessage) (string, error) {
					effects++
					return "", errors.New("work failed")
				}}},
				Provider: beforeRequestProvider{generate: func(context.Context, []Message) (Message, error) {
					requests++
					if requests != 2 {
						return Message{}, malformed
					}
					if scenario == "text" {
						return Text("assistant", "continuing"), nil
					}
					message := Message{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "work-1", Name: "work", Input: json.RawMessage(`{}`)}}}
					if scenario == "unknown_tool" {
						message.Content[0].Name = "missing"
					}
					if scenario == "truncated" {
						message.StopReason = "max_tokens"
					}
					return message, nil
				}},
				OnTurnEnd: func(context.Context, *Loop, Message) (context.Context, string, error) {
					return nil, "continue", nil
				},
			}
			_, err := loop.Run(context.Background(), "task")
			wantEffects := 0
			if scenario == "tool_error" {
				wantEffects = 1
			}
			if !errors.Is(err, malformed) || requests != 3 || effects != wantEffects || loop.Checkpoint.ToolArgumentRetries != 1 {
				t.Fatalf("unsuccessful turn replenished allowance: err=%v requests=%d effects=%d", err, requests, effects)
			}
		})
	}
}
