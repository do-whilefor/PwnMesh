package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/modelwire"
)

const DefaultOpenAIBaseURL = "https://api.openai.com/v1"

func (p *Client) openAIEndpoint() string {
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = DefaultOpenAIBaseURL
	}
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	if parsed, err := url.Parse(base); err == nil && parsed.Path != "" {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

type openAIUsage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Details    struct {
		Cached int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *openAIUsage) usage() *agent.Usage {
	if u == nil {
		return nil
	}
	// OpenAI includes cache hits in prompt_tokens; agent.Usage.Input adds them.
	cached := max(0, min(u.Details.Cached, u.Prompt))
	return &agent.Usage{InputTokens: max(0, u.Prompt-cached), OutputTokens: u.Completion, CacheReadTokens: cached}
}

func finishOpenAI(message *agent.Message, reason string) error {
	switch reason {
	case "stop":
		message.StopReason = "end_turn"
	case "tool_calls":
		message.StopReason = "tool_use"
	case "length":
		message.StopReason = "max_tokens"
	case "content_filter":
		return errors.New("model response blocked by content filter")
	default:
		return errors.New("invalid OpenAI finish reason")
	}
	seen := map[string]bool{}
	for i := range message.Content {
		block := &message.Content[i]
		if block.Type != "tool_use" {
			continue
		}
		if block.ID == "" || block.Name == "" || seen[block.ID] {
			return errors.New("invalid or duplicate OpenAI tool call identity")
		}
		seen[block.ID] = true
		input := strings.TrimSpace(string(block.Input))
		if !strings.HasPrefix(input, "{") || !json.Valid(block.Input) {
			if reason != "length" {
				return &agent.ModelError{Kind: agent.ErrorToolArguments, Err: errors.New("invalid OpenAI tool arguments")}
			}
			block.Input = json.RawMessage(`{}`)
		}
	}
	if (reason == "tool_calls" && len(seen) == 0) || (reason == "stop" && len(seen) != 0) {
		return errors.New("OpenAI finish reason does not match tool calls")
	}
	if len(message.Content) == 0 {
		if reason != "length" {
			return errors.New("empty OpenAI model response")
		}
		message.Content = []agent.Block{{Type: "text", Text: ""}}
	}
	return nil
}

func consumeOpenAIJSON(reader io.Reader) (agent.Message, error) {
	var response struct {
		Choices []struct {
			Index   int                     `json:"index"`
			Message modelwire.OpenAIMessage `json:"message"`
			Finish  string                  `json:"finish_reason"`
		} `json:"choices"`
		Usage *openAIUsage `json:"usage"`
	}
	body := &io.LimitedReader{R: reader, N: (32 << 20) + 1}
	decoder := json.NewDecoder(body)
	err := decoder.Decode(&response)
	observed := agent.Message{Usage: response.Usage.usage()}
	if err == nil {
		var extra json.RawMessage
		err = decoder.Decode(&extra)
		if err == io.EOF {
			err = nil
		} else if err == nil {
			err = errors.New("multiple model response values")
		}
	}
	if body.N == 0 {
		err = errors.New("model response body exceeds 32 MiB")
	}
	if err != nil {
		kind := agent.ErrorProvider
		if retryableTransport(err) {
			kind = agent.ErrorTransport
		}
		return observed, &agent.ModelError{Kind: kind, Err: fmt.Errorf("invalid or incomplete model response body: %w", err)}
	}
	if len(response.Choices) != 1 || response.Choices[0].Index != 0 || response.Choices[0].Message.Role != "assistant" {
		return observed, &agent.ModelError{Kind: agent.ErrorProvider, Err: errors.New("invalid OpenAI model response")}
	}
	choice := response.Choices[0]
	message := agent.Message{Role: "assistant", Usage: observed.Usage}
	if choice.Message.Content != nil {
		message.Content = append(message.Content, agent.Block{Type: "text", Text: *choice.Message.Content})
	}
	if choice.Message.Refusal != "" {
		message.Content = append(message.Content, agent.Block{Type: "text", Text: choice.Message.Refusal})
	}
	for _, call := range choice.Message.ToolCalls {
		if call.Type != "function" {
			return observed, &agent.ModelError{Kind: agent.ErrorProvider, Err: errors.New("unsupported OpenAI tool call type")}
		}
		message.Content = append(message.Content, agent.Block{Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: json.RawMessage(call.Function.Arguments)})
	}
	if err := finishOpenAI(&message, choice.Finish); err != nil {
		var modelErr *agent.ModelError
		if !errors.As(err, &modelErr) {
			err = &agent.ModelError{Kind: agent.ErrorProvider, Err: err}
		}
		return observed, err
	}
	return message, nil
}

func consumeOpenAISSE(ctx context.Context, reader io.Reader, emit agent.Emit) (agent.Message, error) {
	message := agent.Message{Role: "assistant"}
	var content strings.Builder
	type toolParts struct {
		block               int
		id, name, arguments strings.Builder
	}
	parts := map[int]*toolParts{}
	textIndex := -1
	finish := ""
	done := false
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 64<<10), 8<<20)
	var lines []string
	consume := func() error {
		if len(lines) == 0 {
			return nil
		}
		data := strings.Join(lines, "\n")
		lines = nil
		if data == "[DONE]" {
			done = true
			return nil
		}
		var event struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Role      string  `json:"role"`
					Content   *string `json:"content"`
					Refusal   *string `json:"refusal"`
					ToolCalls []struct {
						Index    *int                     `json:"index"`
						ID       string                   `json:"id"`
						Type     string                   `json:"type"`
						Function modelwire.OpenAIFunction `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				Finish string `json:"finish_reason"`
			} `json:"choices"`
			Usage *openAIUsage `json:"usage"`
			Error *struct {
				Type string `json:"type"`
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fmt.Errorf("invalid OpenAI stream event: %w", err)
		}
		if event.Usage != nil {
			message.Usage = event.Usage.usage()
		}
		if event.Error != nil {
			status := 400
			if event.Error.Type == "server_error" || event.Error.Type == "api_error" || event.Error.Code == "server_error" {
				status = 503
			}
			if event.Error.Type == "rate_limit_exceeded" || event.Error.Type == "rate_limit_error" || event.Error.Code == "rate_limit_exceeded" {
				status = 429
			}
			return classifyEndpointError(status, []byte(data))
		}
		if len(event.Choices) == 0 && event.Usage != nil {
			return nil
		}
		if len(event.Choices) != 1 || event.Choices[0].Index != 0 || finish != "" {
			return errors.New("invalid OpenAI stream choice")
		}
		choice := event.Choices[0]
		if choice.Delta.Role != "" && choice.Delta.Role != "assistant" {
			return errors.New("invalid OpenAI stream role")
		}
		for _, delta := range []*string{choice.Delta.Content, choice.Delta.Refusal} {
			if delta == nil {
				continue
			}
			if textIndex < 0 {
				textIndex = len(message.Content)
				message.Content = append(message.Content, agent.Block{Type: "text"})
			}
			content.WriteString(*delta)
			message.Content[textIndex].Text = content.String()
			if emit != nil && *delta != "" {
				emit(agent.Event{Type: "text_delta", Text: *delta})
			}
		}
		for _, call := range choice.Delta.ToolCalls {
			if call.Index == nil || *call.Index < 0 || *call.Index > 4096 || (call.Type != "" && call.Type != "function") {
				return errors.New("invalid OpenAI stream tool call")
			}
			part := parts[*call.Index]
			if part == nil {
				if *call.Index != len(parts) {
					return errors.New("invalid OpenAI stream tool index")
				}
				part = &toolParts{block: len(message.Content)}
				parts[*call.Index] = part
				message.Content = append(message.Content, agent.Block{Type: "tool_use"})
			}
			part.id.WriteString(call.ID)
			part.name.WriteString(call.Function.Name)
			part.arguments.WriteString(call.Function.Arguments)
			block := &message.Content[part.block]
			block.ID, block.Name = part.id.String(), part.name.String()
			if emit != nil && call.Function.Arguments != "" {
				emit(agent.Event{Type: "tool_delta", ToolID: block.ID, ToolName: block.Name, Text: call.Function.Arguments})
			}
		}
		finish = choice.Finish
		return nil
	}
	for scan.Scan() {
		if err := ctx.Err(); err != nil {
			return message, err
		}
		line := strings.TrimSuffix(scan.Text(), "\r")
		if line == "" {
			if err := consume(); err != nil {
				return message, err
			}
			if done {
				break
			}
		} else if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scan.Err(); err != nil {
		kind := agent.ErrorProvider
		if retryableTransport(err) {
			kind = agent.ErrorTransport
		}
		return message, &agent.ModelError{Kind: kind, Err: fmt.Errorf("model stream read failed: %w", err)}
	}
	if err := consume(); err != nil {
		return message, err
	}
	if !done || finish == "" {
		return message, &agent.ModelError{Kind: agent.ErrorTransport, Err: errors.New("OpenAI stream ended before finish and [DONE]")}
	}
	for _, part := range parts {
		// Invalid fragments remain omitted until finishOpenAI rejects or settles
		// them. Failed responses must still serialize across the model bridge.
		if raw := json.RawMessage(part.arguments.String()); json.Valid(raw) {
			message.Content[part.block].Input = raw
		}
	}
	if err := finishOpenAI(&message, finish); err != nil {
		return message, err
	}
	return message, nil
}
