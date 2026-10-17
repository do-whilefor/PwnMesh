package modelwire

import (
	"encoding/json"
	"errors"
	"strings"

	"pwnmesh/internal/agent"
)

type OpenAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

type OpenAIToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAITool struct {
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAIMessage struct {
	Role      string           `json:"role"`
	Content   *string          `json:"content,omitempty"`
	Refusal   string           `json:"refusal,omitempty"`
	ToolCalls []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolID    string           `json:"tool_call_id,omitempty"`
}

func (p RequestFormat) openAIPayload(messages []agent.Message, tools []agent.Definition, limit int) ([]byte, error) {
	if strings.TrimSpace(p.Model) == "" {
		return nil, errors.New("missing OpenAI model")
	}
	if limit <= 0 {
		limit = DefaultMaxTokens
	}
	// Map the project's three tiers to portable OpenAI values. Model support
	// remains the endpoint's decision; an unset direct client keeps its default.
	effort := p.ReasoningEffort
	switch effort {
	case "", "low":
	case "high":
		effort = "medium"
	case "max":
		effort = "high"
	default:
		return nil, errors.New("reasoning effort must be low, high, or max")
	}
	var wireTools []OpenAITool
	for _, tool := range tools {
		wireTools = append(wireTools, OpenAITool{"function", OpenAIFunction{Name: tool.Name, Description: tool.Description, Parameters: tool.Schema}})
	}
	system := System
	wire := []OpenAIMessage{{Role: "system", Content: &system}}
	for _, message := range agent.WireHistory(messages) {
		var text []string
		var calls []OpenAIToolCall
		for _, block := range message.Content {
			switch block.Type {
			case "text":
				text = append(text, block.Text)
			case "tool_use":
				calls = append(calls, OpenAIToolCall{ID: block.ID, Type: "function", Function: OpenAIFunction{Name: block.Name, Arguments: string(block.Input)}})
			case "tool_result":
				var content string
				if json.Unmarshal(block.Content, &content) != nil {
					content = string(block.Content)
				}
				if block.IsError {
					content = "Tool error: " + content
				}
				wire = append(wire, OpenAIMessage{Role: "tool", Content: &content, ToolID: block.ToolUseID})
			}
		}
		if len(text) > 0 || len(calls) > 0 {
			entry := OpenAIMessage{Role: message.Role, ToolCalls: calls}
			if len(text) > 0 {
				content := strings.Join(text, "\n")
				entry.Content = &content
			}
			wire = append(wire, entry)
		}
	}
	return json.Marshal(struct {
		Model           string          `json:"model"`
		Messages        []OpenAIMessage `json:"messages"`
		Tools           []OpenAITool    `json:"tools,omitempty"`
		MaxTokens       int             `json:"max_completion_tokens"`
		ReasoningEffort string          `json:"reasoning_effort,omitempty"`
		Stream          bool            `json:"stream"`
		StreamOptions   struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}{Model: p.Model, Messages: wire, Tools: wireTools, MaxTokens: limit, ReasoningEffort: effort, Stream: true, StreamOptions: struct {
		IncludeUsage bool `json:"include_usage"`
	}{true}})
}
