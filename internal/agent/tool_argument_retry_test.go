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

func TestToolArgumentRetryRejectsInvalidCheckpointAllowance(t *testing.T) {
	for _, count := range []int{-1, 2} {
		loop := &Loop{
			Checkpoint: &ContextCheckpoint{Version: ContextCheckpointVersion, ToolArgumentRetries: count},
			Provider: beforeRequestProvider{generate: func(context.Context, []Message) (Message, error) {
				t.Fatal("invalid checkpoint reached provider")
				return Message{}, nil
			}},
		}
		if _, err := loop.Run(context.Background(), "task"); err == nil {
			t.Fatalf("accepted invalid retry count %d", count)
		}
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
