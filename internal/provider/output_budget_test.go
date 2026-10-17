package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
)

func TestGenerateOutputBudgetDefaultsAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured int
		want       int
	}{
		{"default", 0, 32768},
		{"bounded evaluation", 8192, 8192},
		{"larger explicit allowance", 65536, 65536},
		{"production allowance", 384000, 384000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := testProvider(func(req *http.Request) (*http.Response, error) {
				calls++
				var payload struct {
					MaxTokens    int `json:"max_tokens"`
					OutputConfig struct {
						Effort string `json:"effort"`
					} `json:"output_config"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if payload.MaxTokens != tc.want || payload.OutputConfig.Effort != "max" {
					t.Fatalf("request budget=%d effort=%q, want %d/max", payload.MaxTokens, payload.OutputConfig.Effort, tc.want)
				}
				return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`), nil
			})
			p.MaxTokens = tc.configured
			message, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Do the task")}, nil, nil)
			if err != nil || message.Text() != "done" || calls != 1 {
				t.Fatalf("response=%q requests=%d error=%v", message.Text(), calls, err)
			}
		})
	}
}

func TestNonObjectToolArgumentsRegenerateBeforeSavingOrExecuting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, input := range []string{"", `null`, `[]`, `"bad"`, `123`} {
			t.Run(map[bool]string{false: "json", true: "stream"}[stream]+"/"+input, func(t *testing.T) {
				requests, effects := 0, 0
				p := testProvider(func(req *http.Request) (*http.Response, error) {
					requests++
					if requests == 1 {
						if stream {
							body := toolResponseStream(t, "tool_use", streamedToolCall{"good-in-rejected-batch", `{}`}, streamedToolCall{"bad", input})
							if input == "" {
								body = strings.ReplaceAll(body, `"input":{},`, "")
							}
							return testResponse(req, http.StatusOK, "text/event-stream", body), nil
						}
						field := ""
						if input != "" {
							field = `,"input":` + input
						}
						return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"tool_use","id":"good-in-rejected-batch","name":"record","input":{}},{"type":"tool_use","id":"bad","name":"record"`+field+`}],"stop_reason":"tool_use"}`), nil
					}
					if requests == 2 {
						if effects != 0 {
							t.Fatal("a call from the rejected response executed")
						}
						return testResponse(req, http.StatusOK, "text/event-stream", toolResponseStream(t, "tool_use", streamedToolCall{"replacement", `{}`})), nil
					}
					return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`), nil
				})
				loop := &agent.Loop{Provider: p, SaveState: func(history []agent.Message, _ *agent.ContextCheckpoint) error {
					for _, m := range history {
						for _, b := range m.Content {
							if b.ID == "bad" || b.ID == "good-in-rejected-batch" {
								t.Fatal("invalid response entered saved history")
							}
						}
					}
					return nil
				}, Tools: []agent.Tool{{Definition: agent.Definition{Name: "record"}, Execute: func(context.Context, json.RawMessage) (string, error) {
					effects++
					return "recorded", nil
				}}}}
				result, err := loop.Run(context.Background(), "Record once")
				if err != nil || result != "done" || requests != 3 || effects != 1 {
					t.Fatalf("result=%q requests=%d effects=%d error=%v", result, requests, effects, err)
				}
			})
		}
	}
}

func TestMalformedHistoricalArgumentsUseSafeWireViewWithoutChangingEvidence(t *testing.T) {
	for _, input := range []string{"", `null`, `[]`, `"bad"`, `123`} {
		for _, interrupted := range []bool{false, true} {
			t.Run(input+map[bool]string{false: "/settled", true: "/interrupted"}[interrupted], func(t *testing.T) {
				var rawInput json.RawMessage
				if input != "" {
					rawInput = json.RawMessage(input)
				}
				history := []agent.Message{agent.Text("user", "Continue"), {Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "old", Name: "record", Input: rawInput}}}}
				if !interrupted {
					history = append(history, agent.Message{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "old", Content: json.RawMessage(`"retained original receipt"`)}}})
				}
				for i := range history {
					history[i].Sequence = uint64(i + 1)
				}
				saved, err := json.Marshal(history)
				if err != nil {
					t.Fatal(err)
				}
				var restored []agent.Message
				if err := json.Unmarshal(saved, &restored); err != nil {
					t.Fatal(err)
				}
				requests := 0
				p := testProvider(func(req *http.Request) (*http.Response, error) {
					requests++
					var payload struct {
						Messages []agent.Message `json:"messages"`
					}
					if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					call := payload.Messages[1]
					if string(call.Content[0].Input) != `{}` || !strings.Contains(call.Text(), "Historical tool arguments") || payload.Messages[2].Content[0].ToolUseID != "old" {
						t.Fatalf("unsafe historical view or broken pairing: %+v", payload.Messages)
					}
					return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`), nil
				})
				loop := &agent.Loop{Provider: p, History: restored, ContextBytes: 12000, SaveState: func(saved []agent.Message, _ *agent.ContextCheckpoint) error {
					if !reflect.DeepEqual(saved[:len(history)], history) {
						t.Fatal("persistence rewrote original evidence")
					}
					return nil
				}, Tools: []agent.Tool{{Definition: agent.Definition{Name: "record"}, Execute: func(context.Context, json.RawMessage) (string, error) {
					t.Fatal("historical tool was executed")
					return "", nil
				}}}}
				result, err := loop.Run(context.Background(), "")
				if err != nil || result != "done" || requests != 1 || !reflect.DeepEqual(loop.History[:len(history)], history) {
					t.Fatalf("result=%q requests=%d error=%v", result, requests, err)
				}
			})
		}
	}
}

type streamedToolCall struct {
	id, input string
}

func toolResponseStream(t *testing.T, stop string, calls ...streamedToolCall) string {
	t.Helper()
	var stream strings.Builder
	write := func(event any) {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		stream.WriteString("data: ")
		stream.Write(raw)
		stream.WriteString("\n\n")
	}
	write(map[string]any{"type": "message_start"})
	for index, call := range calls {
		write(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": call.id, "name": "record", "input": map[string]any{}}})
		write(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": call.input}})
		write(map[string]any{"type": "content_block_stop", "index": index})
	}
	write(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}})
	write(map[string]any{"type": "message_stop"})
	return stream.String()
}

func TestStreamedTruncationSettlesToolsAndAllowsCompleteReissue(t *testing.T) {
	for _, stop := range []string{"max_tokens", "length"} {
		t.Run(stop, func(t *testing.T) {
			requests, effects := 0, 0
			p := testProvider(func(req *http.Request) (*http.Response, error) {
				requests++
				switch requests {
				case 1:
					stream := toolResponseStream(t, stop,
						streamedToolCall{"complete-before-truncation", `{"value":"must not execute"}`},
						streamedToolCall{"partial", `{"value":"unfinished`})
					return testResponse(req, http.StatusOK, "text/event-stream", stream), nil
				case 2:
					if effects != 0 {
						t.Fatal("a tool from the truncated response executed")
					}
					var payload struct {
						Messages []agent.Message `json:"messages"`
					}
					if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if len(payload.Messages) != 3 || len(payload.Messages[2].Content) != 2 {
						t.Fatalf("truncated tool group was not settled: %+v", payload.Messages)
					}
					for index, id := range []string{"complete-before-truncation", "partial"} {
						result := payload.Messages[2].Content[index]
						if result.Type != "tool_result" || result.ToolUseID != id || !result.IsError || !strings.Contains(string(result.Content), "truncated") {
							t.Fatalf("missing explicit truncation result for %s: %+v", id, result)
						}
					}
					return testResponse(req, http.StatusOK, "text/event-stream", toolResponseStream(t, "tool_use", streamedToolCall{"reissued", `{"value":"complete"}`})), nil
				case 3:
					return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`), nil
				default:
					t.Fatal("unexpected extra model request")
					return nil, errors.New("unexpected request")
				}
			})
			loop := &agent.Loop{Provider: p, Tools: []agent.Tool{{
				Definition: agent.Definition{Name: "record", Schema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)},
				Execute: func(_ context.Context, input json.RawMessage) (string, error) {
					effects++
					if string(input) != `{"value":"complete"}` {
						t.Fatalf("executed unintended arguments: %s", input)
					}
					return "recorded", nil
				},
			}}}
			result, err := loop.Run(context.Background(), "Record the value")
			if err != nil || result != "done" || requests != 3 || effects != 1 {
				t.Fatalf("result=%q requests=%d effects=%d error=%v", result, requests, effects, err)
			}
			if loop.History[1].StopReason != stop || !json.Valid(loop.History[1].Content[1].Input) {
				t.Fatal("truncation reason or serializable call identity was lost")
			}
			if err := loop.RepairHistory(); err != nil {
				t.Fatalf("reissue left an invalid transcript: %v", err)
			}
		})
	}
}

func TestStreamedInvalidArgumentsWithNormalStopAreRejected(t *testing.T) {
	for _, stop := range []string{"end_turn", "tool_use"} {
		t.Run(stop, func(t *testing.T) {
			requests, effects := 0, 0
			p := testProvider(func(req *http.Request) (*http.Response, error) {
				requests++
				stream := toolResponseStream(t, stop, streamedToolCall{"invalid", `{"value":"unfinished`})
				return testResponse(req, http.StatusOK, "text/event-stream", stream), nil
			})
			loop := &agent.Loop{Provider: p, Tools: []agent.Tool{{
				Definition: agent.Definition{Name: "record", Schema: json.RawMessage(`{"type":"object"}`)},
				Execute: func(context.Context, json.RawMessage) (string, error) {
					effects++
					return "must not execute", nil
				},
			}}}
			_, err := loop.Run(context.Background(), "Record the value")
			var modelErr *agent.ModelError
			if !errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorToolArguments || !strings.Contains(err.Error(), "invalid streamed tool arguments") || requests != 2 || effects != 0 {
				t.Fatalf("malformed argument recovery exceeded its bound or executed calls: requests=%d effects=%d error=%v", requests, effects, err)
			}
			if len(loop.History) != 2 || loop.Checkpoint.ToolArgumentRetries != 1 || loop.History[1].Role != "user" {
				t.Fatal("malformed provider response entered the durable conversation")
			}
		})
	}
}

func TestStreamedInvalidArgumentsRegenerateWithoutPartialEffects(t *testing.T) {
	requests, effects := 0, 0
	savedAllowance := -1
	p := testProvider(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return testResponse(req, http.StatusOK, "text/event-stream", toolResponseStream(t, "tool_use",
				streamedToolCall{"good-in-rejected-batch", `{"value":"never"}`},
				streamedToolCall{"bad", `{"value":`})), nil
		}
		if requests == 2 {
			if effects != 0 || savedAllowance != 1 {
				t.Fatalf("correction executed a rejected call or lost its persisted allowance: effects=%d allowance=%d", effects, savedAllowance)
			}
			return testResponse(req, http.StatusOK, "text/event-stream", toolResponseStream(t, "tool_use", streamedToolCall{"new", `{"value":"once"}`})), nil
		}
		if effects != 1 || savedAllowance != 0 {
			t.Fatalf("successful receipt did not durably renew correction allowance: effects=%d allowance=%d", effects, savedAllowance)
		}
		return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`), nil
	})
	loop := &agent.Loop{Provider: p, SaveState: func(_ []agent.Message, checkpoint *agent.ContextCheckpoint) error {
		savedAllowance = checkpoint.ToolArgumentRetries
		return nil
	}, Tools: []agent.Tool{{
		Definition: agent.Definition{Name: "record", Schema: json.RawMessage(`{"type":"object"}`)},
		Execute: func(_ context.Context, raw json.RawMessage) (string, error) {
			effects++
			if string(raw) != `{"value":"once"}` {
				t.Fatalf("unexpected tool arguments: %s", raw)
			}
			return "recorded", nil
		},
	}}}
	result, err := loop.Run(context.Background(), "Record once")
	if err != nil || result != "done" || requests != 3 || effects != 1 || loop.Checkpoint.ToolArgumentRetries != 0 {
		t.Fatalf("result=%q err=%v requests=%d effects=%d allowance=%d", result, err, requests, effects, loop.Checkpoint.ToolArgumentRetries)
	}
	if err := loop.RepairHistory(); err != nil {
		t.Fatal(err)
	}
}
