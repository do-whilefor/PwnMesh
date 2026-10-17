package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
)

const openAITextResponse = `{"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":31,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":11}}}`

func openAIProvider(roundTrip roundTripFunc) *Client {
	p := testProvider(roundTrip)
	p.Protocol, p.Model = "openai", "test-model"
	return p
}

func TestOpenAIRequestsUseConfiguredEndpointAndOwnWireFormat(t *testing.T) {
	for _, tc := range []struct{ base, endpoint string }{
		{"", "https://api.openai.com/v1/chat/completions"},
		{"https://example.invalid", "https://example.invalid/v1/chat/completions"},
		{"https://example.invalid/api/v3", "https://example.invalid/api/v3/chat/completions"},
		{"https://example.invalid/openai/", "https://example.invalid/openai/chat/completions"},
		{"https://example.invalid/api/v1/", "https://example.invalid/api/v1/chat/completions"},
		{"https://example.invalid/custom/chat/completions/", "https://example.invalid/custom/chat/completions"},
	} {
		t.Run(tc.base, func(t *testing.T) {
			messages := []agent.Message{agent.Text("user", "Inspect"), {Role: "assistant", Sequence: 9, StopReason: "tool_use", Usage: &agent.Usage{InputTokens: 99}, Content: []agent.Block{
				{Type: "thinking", Thinking: "private reasoning", Signature: "signature"},
				{Type: "tool_use", ID: "old", Name: "inspect", Input: json.RawMessage(`{"path":"a"}`)},
			}}, {Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "old", Content: json.RawMessage(`"failed fixture"`), IsError: true}, {Type: "text", Text: "Continue"}}}}
			tools := []agent.Definition{{Name: "inspect", Description: "Inspect a path", Schema: json.RawMessage(`{"type":"object"}`)}}
			original, _ := json.Marshal(messages)
			calls, requestSize := 0, 0
			p := openAIProvider(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.String() != tc.endpoint || req.Header.Get("Authorization") != "Bearer test-token" || req.Header.Get("x-api-key") != "" || req.Header.Get("anthropic-version") != "" {
					t.Fatal("OpenAI request used incorrect endpoint or authentication format")
				}
				raw, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				var payload struct {
					Model         string          `json:"model"`
					Messages      []openAIMessage `json:"messages"`
					Tools         []openAITool    `json:"tools"`
					MaxTokens     int             `json:"max_completion_tokens"`
					Stream        bool            `json:"stream"`
					StreamOptions struct {
						IncludeUsage bool `json:"include_usage"`
					} `json:"stream_options"`
				}
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Fatal(err)
				}
				for _, forbidden := range []string{`"thinking"`, `"output_config"`, `"reasoning_effort"`, `"max_tokens"`, `"sequence"`, `"stop_reason"`, `"usage"`, `private reasoning`} {
					if strings.Contains(string(raw), forbidden) {
						t.Fatalf("unexpected request field %s", forbidden)
					}
				}
				if payload.Model != "test-model" || !payload.Stream || !payload.StreamOptions.IncludeUsage || len(payload.Messages) != 5 || payload.Messages[0].Role != "system" || *payload.Messages[0].Content != System {
					t.Fatalf("incorrect OpenAI payload: %+v", payload)
				}
				call, result := payload.Messages[2], payload.Messages[3]
				if call.Role != "assistant" || len(call.ToolCalls) != 1 || call.ToolCalls[0].Function.Arguments != `{"path":"a"}` || result.Role != "tool" || result.ToolID != "old" || *result.Content != "Tool error: failed fixture" || payload.Messages[4].Role != "user" {
					t.Fatalf("tool history did not round trip: %+v", payload.Messages)
				}
				if calls == 1 {
					requestSize = len(raw)
					if payload.MaxTokens != 2048 || len(payload.Tools) != 1 || payload.Tools[0].Type != "function" || payload.Tools[0].Function.Name != "inspect" || string(payload.Tools[0].Function.Parameters) != `{"type":"object"}` {
						t.Fatal("incorrect tool or output controls")
					}
				} else if payload.MaxTokens != 512 || len(payload.Tools) != 0 {
					t.Fatal("summary inherited tool or normal output controls")
				}
				return testResponse(req, http.StatusOK, "application/json", openAITextResponse), nil
			})
			p.BaseURL, p.MaxTokens, p.ReasoningEffort = tc.base, 2048, "max"
			message, err := p.Generate(context.Background(), messages, tools, nil)
			if err != nil || message.Text() != "done" || message.StopReason != "end_turn" || message.Usage == nil || *message.Usage != (agent.Usage{InputTokens: 20, CacheReadTokens: 11, OutputTokens: 7}) || message.Usage.Input() != 31 {
				t.Fatalf("response=%+v err=%v", message, err)
			}
			if size, err := p.InputBytes(messages, tools); err != nil || size != requestSize {
				t.Fatalf("size=%d wire=%d err=%v", size, requestSize, err)
			}
			if _, err := p.GenerateSummary(context.Background(), messages, 512, nil); err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(messages)
			if calls != 2 || p.MaxTokens != 2048 || string(original) != string(after) {
				t.Fatal("request changed caller state")
			}
		})
	}
}

func openAIEvents(events ...string) string {
	return "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
}

func TestOpenAIStreamedParallelToolCallsRoundTrip(t *testing.T) {
	stream := openAIEvents(
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Checking"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-a","type":"function","function":{"name":"record","arguments":"{\"value\":"}},{"index":1,"id":"call-b","type":"function","function":{"name":"record","arguments":"{\"value\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}},{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":31,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":11}}}`,
		`[DONE]`,
	)
	requests := 0
	var executed []string
	var events []agent.Event
	p := openAIProvider(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return testResponse(req, http.StatusOK, "text/event-stream", stream), nil
		}
		var payload struct {
			Messages []openAIMessage `json:"messages"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Messages) != 5 || len(payload.Messages[2].ToolCalls) != 2 || payload.Messages[3].Role != "tool" || payload.Messages[3].ToolID != "call-a" || payload.Messages[4].ToolID != "call-b" {
			t.Fatalf("invalid tool round trip: %+v", payload.Messages)
		}
		return testResponse(req, http.StatusOK, "application/json", openAITextResponse), nil
	})
	loop := &agent.Loop{Provider: p, Emit: func(event agent.Event) { events = append(events, event) }, Tools: []agent.Tool{{Definition: agent.Definition{Name: "record", Schema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}`)}, Execute: func(_ context.Context, input json.RawMessage) (string, error) {
		executed = append(executed, string(input))
		return "recorded", nil
	}}}}
	result, err := loop.Run(context.Background(), "Record both")
	if err != nil || result != "done" || requests != 2 || !reflect.DeepEqual(executed, []string{`{"value":1}`, `{"value":2}`}) {
		t.Fatalf("result=%q calls=%d executed=%v err=%v", result, requests, executed, err)
	}
	if loop.History[1].Usage == nil || loop.History[1].Usage.Input() != 31 || loop.History[1].StopReason != "tool_use" {
		t.Fatal("stream usage or stop reason lost")
	}
	textDeltas, toolDeltas := 0, 0
	for _, event := range events {
		if event.Type == "text_delta" {
			textDeltas++
		}
		if event.Type == "tool_delta" {
			toolDeltas++
		}
	}
	if textDeltas != 1 || toolDeltas != 4 {
		t.Fatalf("text deltas=%d tool deltas=%d", textDeltas, toolDeltas)
	}
}

func TestOpenAIRejectsInvalidStreamsAndPreservesUsage(t *testing.T) {
	const text = `{"choices":[{"index":0,"delta":{"content":"partial"}}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`
	const stop = `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	for _, tc := range []struct {
		name, body string
		kind       agent.ErrorKind
	}{
		{"missing done", openAIEvents(text, stop), agent.ErrorTransport},
		{"missing finish", openAIEvents(text, `[DONE]`), agent.ErrorTransport},
		{"malformed event", openAIEvents(text, `{`), agent.ErrorProvider},
		{"wrong choice", openAIEvents(text, `{"choices":[{"index":1,"delta":{"content":"wrong"}}]}`), agent.ErrorProvider},
		{"after finish", openAIEvents(text, stop, text, `[DONE]`), agent.ErrorProvider},
		{"content filter", openAIEvents(text, `{"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`, `[DONE]`), agent.ErrorProvider},
		{"context error", openAIEvents(text, `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Too many tokens"}}`), agent.ErrorContextOverflow},
		{"server error type", openAIEvents(text, `{"error":{"type":"server_error","message":"Try again"}}`), agent.ErrorUnavailable},
		{"server error code", openAIEvents(text, `{"error":{"type":"api_error","code":"server_error","message":"Try again"}}`), agent.ErrorUnavailable},
		{"rate limit type", openAIEvents(text, `{"error":{"type":"rate_limit_exceeded","message":"Try later"}}`), agent.ErrorRateLimit},
		{"rate limit code", openAIEvents(text, `{"error":{"type":"tokens","code":"rate_limit_exceeded","message":"Try later"}}`), agent.ErrorRateLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := openAIProvider(func(req *http.Request) (*http.Response, error) {
				return testResponse(req, http.StatusOK, "text/event-stream", tc.body), nil
			})
			message, err := p.Generate(context.Background(), nil, nil, nil)
			var modelErr *agent.ModelError
			if !errors.As(err, &modelErr) || modelErr.Kind != tc.kind || message.Usage == nil || message.Usage.Input() != 9 || message.Text() != "partial" {
				t.Fatalf("message=%+v err=%v", message, err)
			}
		})
	}
}

func TestOpenAIToolValidationAndTruncation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, input := range []string{"", `null`, `[]`, `"bad"`, `{"value":`} {
			for _, finish := range []string{"tool_calls", "length"} {
				t.Run(input+"/"+finish+map[bool]string{true: "/stream", false: "/json"}[stream], func(t *testing.T) {
					arguments, _ := json.Marshal(input)
					body := `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"record","arguments":` + string(arguments) + `}}]},"finish_reason":"` + finish + `"}],"usage":{"prompt_tokens":5}}`
					contentType := "application/json"
					if stream {
						body = openAIEvents(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call","type":"function","function":{"name":"record","arguments":`+string(arguments)+`}}]},"finish_reason":"`+finish+`"}],"usage":{"prompt_tokens":5}}`, `[DONE]`)
						contentType = "text/event-stream"
					}
					p := openAIProvider(func(req *http.Request) (*http.Response, error) {
						return testResponse(req, http.StatusOK, contentType, body), nil
					})
					message, err := p.Generate(context.Background(), nil, nil, nil)
					if message.Usage == nil || message.Usage.Input() != 5 {
						t.Fatal("rejected response lost usage")
					}
					if _, marshalErr := json.Marshal(message); marshalErr != nil {
						t.Fatalf("failed response cannot cross the model bridge: %v", marshalErr)
					}
					if finish == "length" {
						if err != nil || message.StopReason != "max_tokens" || len(message.Content) != 1 || string(message.Content[0].Input) != `{}` {
							t.Fatalf("truncation lost call identity or safe arguments: %+v err=%v", message, err)
						}
					} else {
						var modelErr *agent.ModelError
						if !errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorToolArguments {
							t.Fatalf("invalid tool arguments accepted: %+v err=%v", message, err)
						}
					}
				})
			}
		}
	}
}

func TestOpenAIEmbeddedErrorRetainsSameChunkUsage(t *testing.T) {
	stream := openAIEvents(
		`{"choices":[{"index":0,"delta":{"content":"partial"}}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`,
		`{"error":{"type":"server_error"},"usage":{"prompt_tokens":31,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":11}}}`,
	)
	p := openAIProvider(func(req *http.Request) (*http.Response, error) {
		return testResponse(req, http.StatusOK, "text/event-stream", stream), nil
	})
	message, err := p.Generate(context.Background(), nil, nil, nil)
	var modelErr *agent.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorUnavailable || message.Text() != "partial" || message.Usage == nil || *message.Usage != (agent.Usage{InputTokens: 20, OutputTokens: 7, CacheReadTokens: 11}) {
		t.Fatalf("error chunk lost final usage: message=%+v err=%v", message, err)
	}
}

func TestOpenAIRejectsMalformedJSONAndUnconfiguredModel(t *testing.T) {
	for _, body := range []string{openAITextResponse + `{}`, openAITextResponse + `garbage`, strings.Replace(openAITextResponse, `"finish_reason":"stop"`, `"finish_reason":""`, 1)} {
		p := openAIProvider(func(req *http.Request) (*http.Response, error) {
			return testResponse(req, http.StatusOK, "application/json", body), nil
		})
		message, err := p.Generate(context.Background(), nil, nil, nil)
		if err == nil || message.Usage == nil || message.Usage.Input() != 31 || len(message.Content) != 0 {
			t.Fatalf("malformed response accepted or usage lost: %+v err=%v", message, err)
		}
	}
	p := openAIProvider(func(req *http.Request) (*http.Response, error) {
		t.Fatal("unconfigured model made a network request")
		return nil, nil
	})
	p.Model = ""
	if _, err := p.Generate(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("OpenAI inherited the Anthropic default model")
	}
	if _, err := p.InputBytes(nil, nil); err == nil {
		t.Fatal("sizing accepted an invalid model")
	}
}

func TestOpenAIRejectsInvalidToolIdentitiesAndFinishReasons(t *testing.T) {
	const call = `{"index":0,"id":"call","type":"function","function":{"name":"record","arguments":"{}"}}`
	for _, tc := range []struct{ name, calls, finish string }{
		{"missing id", strings.Replace(call, `"id":"call",`, "", 1), "tool_calls"},
		{"missing name", strings.Replace(call, `"name":"record",`, "", 1), "tool_calls"},
		{"duplicate id", call + "," + strings.Replace(call, `"index":0`, `"index":1`, 1), "tool_calls"},
		{"unsupported type", strings.Replace(call, `"type":"function"`, `"type":"custom"`, 1), "tool_calls"},
		{"wrong stop", call, "stop"},
		{"empty calls", "", "tool_calls"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{true: "/stream", false: "/json"}[stream], func(t *testing.T) {
				body := `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[` + tc.calls + `]},"finish_reason":"` + tc.finish + `"}]}`
				contentType := "application/json"
				if stream {
					body = openAIEvents(`{"choices":[{"index":0,"delta":{"tool_calls":[`+tc.calls+`]},"finish_reason":"`+tc.finish+`"}]}`, `[DONE]`)
					contentType = "text/event-stream"
				}
				p := openAIProvider(func(req *http.Request) (*http.Response, error) {
					return testResponse(req, http.StatusOK, contentType, body), nil
				})
				_, err := p.Generate(context.Background(), nil, nil, nil)
				var modelErr *agent.ModelError
				if !errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorProvider {
					t.Fatalf("invalid tool response accepted: %v", err)
				}
			})
		}
	}
}
