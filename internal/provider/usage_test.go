package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
			p := Anthropic{BaseURL: server.URL, Token: "test-token"}
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
