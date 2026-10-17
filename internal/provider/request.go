package provider

import (
	"encoding/json"
	"errors"

	"pwnmesh/internal/agent"
)

// RequestFormat describes serialized model requests without transport or credentials.
type RequestFormat struct {
	Protocol        string
	Model           string
	MaxTokens       int
	ReasoningEffort string
	Adaptive        bool
}

func (p *Client) RequestFormat() RequestFormat {
	return RequestFormat{Protocol: p.Protocol, Model: p.Model, MaxTokens: p.MaxTokens, ReasoningEffort: p.ReasoningEffort, Adaptive: p.AdaptiveThinking()}
}

func (p RequestFormat) InputBytes(messages []agent.Message, tools []agent.Definition) (int, error) {
	raw, err := p.payload(messages, tools, p.MaxTokens)
	return len(raw), err
}

func (p RequestFormat) payload(messages []agent.Message, tools []agent.Definition, limit int) ([]byte, error) {
	if p.Protocol == "openai" {
		return p.openAIPayload(messages, tools, limit)
	}
	if p.Protocol != "" && p.Protocol != "anthropic" {
		return nil, errors.New("unsupported model protocol")
	}
	model := p.Model
	if model == "" {
		model = DefaultModel
	}
	if limit <= 0 {
		limit = DefaultMaxTokens
	}
	effort := p.ReasoningEffort
	if effort == "" {
		effort = DefaultReasoningEffort
	}
	switch effort {
	case "low", "high", "max":
	default:
		return nil, errors.New("reasoning effort must be low, high, or max")
	}
	payload := struct {
		Model     string             `json:"model"`
		MaxTokens int                `json:"max_tokens"`
		System    string             `json:"system"`
		Messages  []agent.Message    `json:"messages"`
		Tools     []agent.Definition `json:"tools,omitempty"`
		Stream    bool               `json:"stream"`
		Thinking  struct {
			Type string `json:"type"`
		} `json:"thinking"`
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}{Model: model, MaxTokens: limit, System: System, Messages: agent.WireHistory(messages), Tools: tools, Stream: true}
	// DeepSeek and GLM use enabled with effort. Kimi Coding and OpenRouter
	// require adaptive to express effort without a separate thinking budget.
	payload.Thinking.Type = "enabled"
	if p.Adaptive {
		payload.Thinking.Type = "adaptive"
	}
	payload.OutputConfig.Effort = effort
	return json.Marshal(payload)
}
