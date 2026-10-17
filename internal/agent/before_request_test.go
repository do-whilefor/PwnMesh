package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type beforeRequestProvider struct {
	generate  func(context.Context, []Message) (Message, error)
	summarize func([]Message) (Message, error)
}

func (p beforeRequestProvider) Generate(ctx context.Context, messages []Message, _ []Definition, _ Emit) (Message, error) {
	return p.generate(ctx, messages)
}

func (p beforeRequestProvider) GenerateSummary(_ context.Context, messages []Message, _ int, _ Emit) (Message, error) {
	return p.summarize(messages)
}

func TestRuntimeContinuationRefreshesThenReturnsSettledResult(t *testing.T) {
	requests, boundaries, turns, starts, ends := 0, 0, 0, 0, 0
	loop := &Loop{
		Provider: beforeRequestProvider{generate: func(_ context.Context, messages []Message) (Message, error) {
			requests++
			if boundaries != requests {
				t.Fatalf("request %d skipped its runtime boundary: %d", requests, boundaries)
			}
			if requests == 1 {
				return Text("assistant", "partial result"), nil
			}
			if requests != 2 || messages[len(messages)-1].Text() != "finish the remaining check" {
				t.Fatalf("unexpected continuation request: %+v", messages)
			}
			return Text("assistant", "completed result"), nil
		}},
		BeforeRequest: func(ctx context.Context, _ *Loop) (context.Context, error) {
			boundaries++
			return ctx, nil
		},
		OnTurnEnd: func(ctx context.Context, loop *Loop, message Message) (context.Context, string, error) {
			turns++
			if loop.History[len(loop.History)-1].Text() != message.Text() {
				t.Fatal("continuation was requested before the model response was settled")
			}
			if turns == 1 {
				return ctx, "finish the remaining check", nil
			}
			return ctx, "", nil
		},
		Emit: func(event Event) {
			switch event.Type {
			case "agent_start":
				starts++
			case "agent_end":
				ends++
			}
		},
	}
	result, err := loop.Run(context.Background(), "bounded task")
	if err != nil || result != "completed result" || requests != 2 || boundaries != 2 || turns != 2 || starts != 1 || ends != 1 {
		t.Fatalf("result=%q err=%v requests=%d boundaries=%d turns=%d starts=%d ends=%d", result, err, requests, boundaries, turns, starts, ends)
	}
	if len(loop.History) != 4 || loop.Checkpoint.LastSequence != 4 {
		t.Fatalf("continuation history did not settle exactly once: %+v", loop.History)
	}
}

func TestBeforeRequestSeesPromptAndSettledToolResults(t *testing.T) {
	hooks, requests, executions := 0, 0, 0
	loop := &Loop{
		Tools: []Tool{{
			Definition: Definition{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)},
			Execute: func(context.Context, json.RawMessage) (string, error) {
				executions++
				return "current evidence", nil
			},
		}},
		BeforeRequest: func(ctx context.Context, loop *Loop) (context.Context, error) {
			hooks++
			if err := ctx.Err(); err != nil {
				t.Fatal(err)
			}
			switch hooks {
			case 1:
				if len(loop.History) != 1 || loop.History[0].Text() != "inspect the task" {
					t.Fatalf("initial prompt was not present at the request boundary: %+v", loop.History)
				}
			case 2:
				if len(loop.History) != 4 || executions != 1 {
					t.Fatalf("hook ran before the tool group settled: history=%+v executions=%d", loop.History, executions)
				}
				call, result := loop.History[2].Content[0], loop.History[3].Content[0]
				if call.Type != "tool_use" || result.Type != "tool_result" || result.ToolUseID != call.ID || result.IsError {
					t.Fatalf("tool result is not paired before the hook: call=%+v result=%+v", call, result)
				}
			default:
				t.Fatalf("unexpected request boundary %d", hooks)
			}
			return nil, loop.AppendInstruction(fmt.Sprintf("refreshed task data %d", hooks))
		},
	}
	loop.Provider = beforeRequestProvider{generate: func(_ context.Context, messages []Message) (Message, error) {
		requests++
		if hooks != requests || messages[len(messages)-1].Text() != fmt.Sprintf("refreshed task data %d", requests) {
			t.Fatalf("request did not receive its refreshed instruction: hooks=%d request=%d history=%+v", hooks, requests, messages)
		}
		if requests == 1 {
			return Message{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "read-1", Name: "read", Input: json.RawMessage(`{}`)}}}, nil
		}
		return Text("assistant", "done"), nil
	}}
	result, err := loop.Run(context.Background(), "inspect the task")
	if err != nil || result != "done" || hooks != 2 || requests != 2 || executions != 1 {
		t.Fatalf("result=%q err=%v hooks=%d requests=%d executions=%d", result, err, hooks, requests, executions)
	}
	if err := loop.RepairHistory(); err != nil {
		t.Fatalf("appending boundary instructions corrupted tool pairing: %v", err)
	}
}

func TestBeforeRequestRunsAfterRepairWithoutReplayingTools(t *testing.T) {
	hooks, requests, executions := 0, 0, 0
	loop := &Loop{
		History: []Message{
			Text("user", "original task"),
			{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "interrupted", Name: "write", Input: json.RawMessage(`{}`)}}},
		},
		Tools: []Tool{{
			Definition: Definition{Name: "write", Schema: json.RawMessage(`{"type":"object"}`)},
			Execute: func(context.Context, json.RawMessage) (string, error) {
				executions++
				return "side effect", nil
			},
		}},
		BeforeRequest: func(_ context.Context, loop *Loop) (context.Context, error) {
			hooks++
			if len(loop.History) != 4 || loop.History[3].Text() != "resume" {
				t.Fatalf("repair and resumed prompt must precede the hook: %+v", loop.History)
			}
			result := loop.History[2].Content[0]
			if result.Type != "tool_result" || result.ToolUseID != "interrupted" || !result.IsError || !strings.Contains(string(result.Content), "interrupted") {
				t.Fatalf("interrupted call was not settled before the hook: %+v", result)
			}
			return nil, loop.AppendInstruction("fresh resumed task data")
		},
		Provider: beforeRequestProvider{generate: func(_ context.Context, messages []Message) (Message, error) {
			requests++
			if messages[len(messages)-1].Text() != "fresh resumed task data" {
				t.Fatal("resumed request did not receive the boundary instruction")
			}
			return Text("assistant", "done"), nil
		}},
	}
	if _, err := loop.Run(context.Background(), "resume"); err != nil {
		t.Fatal(err)
	}
	if hooks != 1 || requests != 1 || executions != 0 {
		t.Fatalf("hooks=%d requests=%d replayed side effects=%d", hooks, requests, executions)
	}
}

func TestBeforeRequestFailurePreventsNextProviderCall(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("boundary_%d", failAt), func(t *testing.T) {
			failure := errors.New("task refresh failed")
			hooks, requests := 0, 0
			loop := &Loop{
				BeforeRequest: func(context.Context, *Loop) (context.Context, error) {
					hooks++
					if hooks == failAt {
						return nil, failure
					}
					return nil, nil
				},
				Provider: beforeRequestProvider{generate: func(context.Context, []Message) (Message, error) {
					requests++
					return Message{Role: "assistant", Content: []Block{
						{Type: "text", Text: "checking"},
						{Type: "tool_use", ID: "read-1", Name: "read", Input: json.RawMessage(`{}`)},
					}}, nil
				}},
			}
			result, err := loop.Run(context.Background(), "inspect")
			if !errors.Is(err, failure) || hooks != failAt || requests != failAt-1 {
				t.Fatalf("hook failure did not stop the next request: result=%q err=%v hooks=%d requests=%d", result, err, hooks, requests)
			}
			if failAt == 2 && result != "checking" {
				t.Fatalf("hook failure lost the preceding model response: %q", result)
			}
		})
	}
}

func TestBeforeRequestReplacementContextReachesRequestsAndTools(t *testing.T) {
	replacement, cancel := context.WithCancel(context.Background())
	defer cancel()
	hooks, requests, executions := 0, 0, 0
	loop := &Loop{
		BeforeRequest: func(ctx context.Context, _ *Loop) (context.Context, error) {
			hooks++
			if hooks == 1 {
				return replacement, nil
			}
			if ctx != replacement {
				t.Fatal("subsequent hook did not receive the replacement context")
			}
			return nil, nil
		},
		Tools: []Tool{{
			Definition: Definition{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)},
			Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
				executions++
				if ctx != replacement {
					t.Fatal("tool execution did not receive the replacement context")
				}
				return "evidence", nil
			},
		}},
		Provider: beforeRequestProvider{generate: func(ctx context.Context, _ []Message) (Message, error) {
			requests++
			if ctx != replacement {
				t.Fatal("request did not retain the replacement context")
			}
			if requests == 1 {
				return Message{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "read-1", Name: "read", Input: json.RawMessage(`{}`)}}}, nil
			}
			return Text("assistant", "done"), nil
		}},
	}
	if result, err := loop.Run(context.Background(), "inspect"); err != nil || result != "done" || hooks != 2 || requests != 2 || executions != 1 {
		t.Fatalf("result=%q err=%v hooks=%d requests=%d executions=%d", result, err, hooks, requests, executions)
	}
}

func TestBeforeRequestDoesNotRepeatForSummaryOrOverflowRetry(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		name, historyBytes := "threshold_compaction", 16000
		if overflow {
			name, historyBytes = "context_overflow_retry", 6000
		}
		t.Run(name, func(t *testing.T) {
			hooks, requests, summaries := 0, 0, 0
			loop := &Loop{
				History:      []Message{Text("user", "original task"), Text("assistant", strings.Repeat("x", historyBytes))},
				ContextBytes: 12000,
				SummaryBytes: 256,
				RecentBytes:  256,
				BeforeRequest: func(_ context.Context, loop *Loop) (context.Context, error) {
					hooks++
					return nil, loop.AppendInstruction("fresh boundary data")
				},
				Provider: beforeRequestProvider{
					generate: func(_ context.Context, messages []Message) (Message, error) {
						requests++
						if hooks != 1 || messages[len(messages)-1].Text() != "fresh boundary data" {
							t.Fatalf("boundary instruction was repeated or lost: hooks=%d history=%+v", hooks, messages)
						}
						if overflow && requests == 1 {
							return Message{}, &ModelError{Kind: ErrorContextOverflow, Err: errors.New("request too large")}
						}
						return Text("assistant", "done"), nil
					},
					summarize: func([]Message) (Message, error) {
						summaries++
						if hooks != 1 {
							t.Fatalf("summary ran outside the prepared boundary: hooks=%d", hooks)
						}
						return Text("assistant", `{"notes":"Earlier analysis recorded.","quotes":[]}`), nil
					},
				},
			}
			result, err := loop.Run(context.Background(), "")
			wantRequests := 1
			if overflow {
				wantRequests = 2
			}
			if err != nil || result != "done" || hooks != 1 || requests != wantRequests || summaries != 1 || loop.Checkpoint.CompactionCount != 1 {
				t.Fatalf("result=%q err=%v hooks=%d requests=%d summaries=%d checkpoint=%+v", result, err, hooks, requests, summaries, loop.Checkpoint)
			}
		})
	}
}

func contextDataTestLoop(data string) *Loop {
	return &Loop{
		TaskPrompt:   "inspect the original task",
		ContextData:  []string{data},
		ContextBytes: 12000,
		SummaryBytes: 256,
		RecentBytes:  128,
		History: []Message{
			Text("user", "inspect the original task"),
			Text("assistant", strings.Repeat("earlier analysis ", 1200)),
			Text("user", data),
		},
	}
}

func TestContextDataSurvivesCompactionWithoutSummarySupport(t *testing.T) {
	data := `{"version":7,"correction":"` + strings.Repeat("exact source bytes ", 200) + `"}`
	loop := contextDataTestLoop(data)
	loop.ToolsDisabledReason = "use retained evidence"
	loop.PinnedInstructions = []string{"use the frozen evidence", "repair the result format"}
	loop.ContextData = []string{data, data, "pending dependencies remain", loop.TaskPrompt, loop.PinnedInstructions[0], loop.PinnedInstructions[1]}
	loop.History = append(loop.History, Text("user", "recent boundary"), Text("user", data), Text("user", loop.PinnedInstructions[0]), Text("user", loop.PinnedInstructions[1]))
	requests, summaries := 0, 0
	loop.Provider = beforeRequestProvider{
		generate: func(_ context.Context, messages []Message) (Message, error) {
			requests++
			counts := map[string]int{}
			for _, message := range messages {
				if message.Role == "user" {
					counts[message.Text()]++
				}
			}
			for _, text := range []string{data, "pending dependencies remain", loop.TaskPrompt, loop.PinnedInstructions[0], loop.PinnedInstructions[1]} {
				if counts[text] != 1 {
					t.Fatalf("retained task data or instructions lost/duplicated: count=%d text=%q", counts[text], text)
				}
			}
			for offset, want := range []string{data, "pending dependencies remain", loop.PinnedInstructions[0], loop.PinnedInstructions[1]} {
				if messages[len(messages)-4+offset].Text() != want {
					t.Fatal("runtime data was not rebuilt after the tail and before phase instructions")
				}
			}
			return Text("assistant", "done"), nil
		},
		summarize: func(messages []Message) (Message, error) {
			summaries++
			if strings.Contains(messages[0].Text(), "exact source bytes") {
				t.Fatal("pinned task data was also sent for model summarization")
			}
			return Text("assistant", `{"notes":"Earlier analysis omitted.","quotes":[]}`), nil
		},
	}
	result, err := loop.Run(context.Background(), "")
	if err != nil || result != "done" || requests != 1 || summaries != 1 || loop.Checkpoint.CompactionCount != 1 {
		t.Fatalf("result=%q err=%v requests=%d summaries=%d checkpoint=%+v", result, err, requests, summaries, loop.Checkpoint)
	}
}

func TestContextDataOverBudgetPreventsModelRequest(t *testing.T) {
	for _, budget := range []string{"bytes", "tokens"} {
		t.Run(budget, func(t *testing.T) {
			loop := contextDataTestLoop(strings.Repeat("task data ", 600))
			if budget == "bytes" {
				loop.ContextBytes = 2048
			} else {
				loop.ContextTokens = 512
			}
			calls := 0
			loop.Provider = beforeRequestProvider{
				generate: func(context.Context, []Message) (Message, error) {
					calls++
					return Text("assistant", "unexpected"), nil
				},
				summarize: func([]Message) (Message, error) { calls++; return Text("assistant", "unexpected"), nil },
			}
			_, err := loop.Run(context.Background(), "")
			var modelError *ModelError
			if !errors.As(err, &modelError) || modelError.Kind != ErrorBudget || calls != 0 {
				t.Fatalf("oversized data did not stop before requesting a model: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestContextDataCompactionSaveFailurePreservesOriginalView(t *testing.T) {
	data := strings.Repeat("retained runtime data ", 200)
	loop := contextDataTestLoop(data)
	loop.Provider = beforeRequestProvider{summarize: func([]Message) (Message, error) {
		return Text("assistant", `{"notes":"Earlier analysis omitted.","quotes":[]}`), nil
	}}
	if err := loop.initCheckpoint(); err != nil {
		t.Fatal(err)
	}
	history := append([]Message(nil), loop.History...)
	checkpoint := *loop.Checkpoint
	checkpointPointer := loop.Checkpoint
	failure := errors.New("checkpoint write failed")
	saves := 0
	loop.SaveState = func(messages []Message, next *ContextCheckpoint) error {
		saves++
		if messages[len(messages)-1].Text() != data || next.CompactionCount != 1 {
			t.Fatal("attempted checkpoint did not contain retained data and its compaction")
		}
		return failure
	}
	size, err := loop.compactRequest(context.Background(), false)
	if !errors.Is(err, failure) || size != -1 || saves != 1 || loop.Checkpoint != checkpointPointer || !reflect.DeepEqual(*loop.Checkpoint, checkpoint) || !reflect.DeepEqual(loop.History, history) {
		t.Fatalf("failed save changed the original view/checkpoint: err=%v saves=%d history=%v checkpoint=%+v", err, saves, reflect.DeepEqual(loop.History, history), loop.Checkpoint)
	}
}

func TestCompactionOmitsSupersededRuntimeSnapshotsWithoutChangingEvidence(t *testing.T) {
	for _, saveFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "failed_save"}[saveFailure], func(t *testing.T) {
			const prefix = "Runtime-owned index:\n"
			current := prefix + "current-only"
			loop := summaryTestLoop(nil)
			loop.RecentBytes = 1
			loop.ContextData = []string{current}
			loop.ContextDataPrefixes = []string{"", prefix}
			loop.History = append(loop.History,
				Text("user", prefix+"retired-only"),
				Text("assistant", prefix+"assistant-marker"),
				Message{Role: "user", Content: []Block{{Type: "text", Text: prefix + "paired-user-marker"}, {Type: "text", Text: "additional user content"}}},
				Text("user", "embedded-marker "+prefix),
			)
			for n := 4; n < len(loop.History); n++ {
				loop.History[n].Sequence = uint64(n + 1)
			}
			loop.Checkpoint.LastSequence = uint64(len(loop.History))
			original := cloneMessages(loop.History)
			failure := errors.New("save failed")
			loop.Provider = summaryTestProvider(func(messages []Message) (Message, error) {
				if !reflect.DeepEqual(loop.History, original) {
					t.Fatal("request filtering rewrote original evidence")
				}
				text := messages[0].Text()
				if strings.Contains(text, "retired-only") || strings.Contains(text, "current-only") {
					t.Fatal("runtime snapshot recursively entered summary input")
				}
				for _, marker := range []string{"assistant-marker", "paired-user-marker", "embedded-marker"} {
					if !strings.Contains(text, marker) {
						t.Fatalf("prefix filtering erased other content: %s", marker)
					}
				}
				return Text("assistant", `{"notes":"Continue.","quotes":[]}`), nil
			})
			loop.SaveState = func(messages []Message, _ *ContextCheckpoint) error {
				count := 0
				for _, m := range messages {
					if m.Text() == current {
						count++
					}
					if strings.Contains(m.Text(), "retired-only") {
						t.Fatal("retired snapshot survived in request view")
					}
				}
				if count != 1 {
					t.Fatalf("current snapshot duplicated or omitted: %d", count)
				}
				if saveFailure {
					return failure
				}
				return nil
			}
			_, err := loop.compactRequest(context.Background(), false)
			if saveFailure {
				if !errors.Is(err, failure) || !reflect.DeepEqual(loop.History, original) {
					t.Fatalf("failed compaction rewrote original history: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
