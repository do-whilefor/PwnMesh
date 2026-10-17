package modelwire

import (
	"bytes"
	"encoding/json"
	"testing"

	"pwnmesh/internal/agent"
)

func TestRequestFormatPreservesProtocolBytes(t *testing.T) {
	messages := []agent.Message{agent.Text("user", "Inspect <target>")}
	tools := []agent.Definition{{Name: "inspect", Schema: json.RawMessage(`{"type":"object"}`)}}
	for _, tc := range []struct {
		name   string
		format RequestFormat
		want   string
	}{
		{"Anthropic defaults", RequestFormat{}, `{"model":"deepseek-flash","max_tokens":32768,"system":"Work toward the assigned task using available tools. Distinguish confirmed facts from guesses. Follow the current request's output contract.","messages":[{"role":"user","content":[{"type":"text","text":"Inspect \u003ctarget\u003e"}]}],"tools":[{"name":"inspect","description":"","input_schema":{"type":"object"}}],"stream":true,"thinking":{"type":"enabled"},"output_config":{"effort":"max"}}`},
		{"Anthropic adaptive", RequestFormat{Protocol: "anthropic", Model: "kimi-for-coding", MaxTokens: 8192, ReasoningEffort: "high", Adaptive: true}, `{"model":"kimi-for-coding","max_tokens":8192,"system":"Work toward the assigned task using available tools. Distinguish confirmed facts from guesses. Follow the current request's output contract.","messages":[{"role":"user","content":[{"type":"text","text":"Inspect \u003ctarget\u003e"}]}],"tools":[{"name":"inspect","description":"","input_schema":{"type":"object"}}],"stream":true,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`},
		{"OpenAI effort", RequestFormat{Protocol: "openai", Model: "fixture", MaxTokens: 8192, ReasoningEffort: "high"}, `{"model":"fixture","messages":[{"role":"system","content":"Work toward the assigned task using available tools. Distinguish confirmed facts from guesses. Follow the current request's output contract."},{"role":"user","content":"Inspect \u003ctarget\u003e"}],"tools":[{"type":"function","function":{"name":"inspect","parameters":{"type":"object"}}}],"max_completion_tokens":8192,"reasoning_effort":"medium","stream":true,"stream_options":{"include_usage":true}}`},
		{"OpenAI defaults", RequestFormat{Protocol: "openai", Model: "fixture"}, `{"model":"fixture","messages":[{"role":"system","content":"Work toward the assigned task using available tools. Distinguish confirmed facts from guesses. Follow the current request's output contract."},{"role":"user","content":"Inspect \u003ctarget\u003e"}],"tools":[{"type":"function","function":{"name":"inspect","parameters":{"type":"object"}}}],"max_completion_tokens":32768,"stream":true,"stream_options":{"include_usage":true}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := tc.format.Payload(messages, tools, tc.format.MaxTokens)
			if err != nil || string(raw) != tc.want {
				t.Fatalf("request changed: %s, error=%v", raw, err)
			}
			size, err := tc.format.InputBytes(messages, tools)
			if err != nil || size != len(tc.want) {
				t.Fatalf("request size=%d, want %d: %v", size, len(tc.want), err)
			}
			summary, err := tc.format.Payload(messages, nil, 512)
			if err != nil || !bytes.Contains(summary, []byte(`:512`)) || bytes.Contains(summary, []byte(`"tools"`)) {
				t.Fatalf("summary lost its output limit or retained tools: %s, %v", summary, err)
			}
		})
	}
}

func TestRequestFormatRejectsInvalidProtocolPolicy(t *testing.T) {
	for _, format := range []RequestFormat{
		{Protocol: "unknown"}, {Protocol: "openai"}, {ReasoningEffort: "medium"},
		{Protocol: "openai", Model: "fixture", ReasoningEffort: "medium"},
	} {
		if _, err := format.InputBytes(nil, nil); err == nil {
			t.Fatalf("invalid model request policy accepted: %+v", format)
		}
	}
}
