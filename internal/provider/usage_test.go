package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
)

func TestReportedUsageSurvivesStreamAndJSON(t *testing.T) {
	const start = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":11,\"output_tokens\":1,\"cache_creation_input_tokens\":37,\"cache_read_input_tokens\":19}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\n"
	const end = "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, tc := range []struct {
		name, contentType, body string
		failed                  bool
		output                  int
	}{
		{"stream", "text/event-stream", start + end, false, 7},
		{"partial stream", "text/event-stream", start, true, 1},
		{"JSON", "application/json", `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7,"cache_creation_input_tokens":37,"cache_read_input_tokens":19}}`, false, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			p := Client{BaseURL: server.URL, Token: "test-token"}
			message, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "fixture")}, nil, nil)
			if (err != nil) != tc.failed {
				t.Fatalf("unexpected error: %v", err)
			}
			want := agent.Usage{InputTokens: 11, OutputTokens: tc.output, CacheReadTokens: 19, CacheWriteTokens: 37}
			if message.Usage == nil || *message.Usage != want || message.Usage.Input() != 67 {
				t.Fatalf("reported usage changed: %+v", message.Usage)
			}
		})
	}
}

func TestStreamUsageDistinguishesZeroFromOmittedCounters(t *testing.T) {
	start := `data: {"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1,"cache_creation_input_tokens":37,"cache_read_input_tokens":19}}}` + "\n\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}` + "\n\n"
	for _, tc := range []struct {
		name, usage string
		want        agent.Usage
	}{
		{"omitted", `{"output_tokens":7}`, agent.Usage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 19, CacheWriteTokens: 37}},
		{"zero", `{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`, agent.Usage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := start + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + tc.usage + "}\n\n" + `data: {"type":"message_stop"}` + "\n\n"
			message, err := consumeSSE(context.Background(), strings.NewReader(body), nil)
			if err != nil || message.Usage == nil || *message.Usage != tc.want {
				t.Fatalf("reported counters changed: usage=%+v err=%v", message.Usage, err)
			}
		})
	}
}

func TestRejectedJSONRetainsDecodedUsage(t *testing.T) {
	const response = `{"role":"assistant","content":[{"type":"text","text":"unaccepted"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7,"cache_creation_input_tokens":37,"cache_read_input_tokens":19}}`
	for _, body := range []string{
		response + `{}`,
		response + `trailing garbage`,
		strings.Replace(response, `"stop_reason":"end_turn"`, `"stop_reason":""`, 1),
	} {
		p := testProvider(func(req *http.Request) (*http.Response, error) {
			return testResponse(req, http.StatusOK, "application/json", body), nil
		})
		message, err := p.Generate(context.Background(), nil, nil, nil)
		want := agent.Usage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 19, CacheWriteTokens: 37}
		if err == nil || len(message.Content) != 0 || message.Usage == nil || *message.Usage != want {
			t.Fatalf("rejected response lost observed usage or retained content: usage=%+v content=%+v err=%v", message.Usage, message.Content, err)
		}
	}
}
