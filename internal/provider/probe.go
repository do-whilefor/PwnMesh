package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"

	"pwnmesh/internal/agent"
)

const ProbeMaxTokens = 1024

// Probe verifies one tool round trip without executing project tools. Callers
// bound output and time, and retain their own provider-error classification.
func Probe(ctx context.Context, p agent.Provider) error {
	tool := agent.Definition{Name: "readiness_check", Description: "Call once with ready:true, then return exactly the JSON object supplied in its result.", Schema: json.RawMessage(`{"type":"object","properties":{"ready":{"type":"boolean","enum":[true]}},"required":["ready"],"additionalProperties":false}`)}
	history := []agent.Message{agent.Text("user", "Verify tool support. Call readiness_check exactly once with {\"ready\":true}. After receiving its result, return exactly that JSON object, without commentary or further calls.")}
	first, err := p.Generate(ctx, history, []agent.Definition{tool}, nil)
	if err != nil {
		return err
	}
	if first.Role != "assistant" || first.StopReason != "tool_use" {
		return errors.New("probe did not return a complete assistant tool call")
	}
	var calls []agent.Block
	for _, block := range first.Content {
		if block.Type == "tool_use" {
			calls = append(calls, block)
		}
	}
	if len(calls) != 1 || calls[0].ID == "" || calls[0].Name != tool.Name || agent.ValidateArguments(tool.Schema, calls[0].Input) != nil {
		return errors.New("probe requires exactly one valid readiness_check call")
	}
	// Reveal the random challenge only in the tool result, so a canned reply
	// cannot pass without consuming tool-result history.
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	result, _ := json.Marshal(map[string]string{"ready": hex.EncodeToString(nonce[:])})
	content, _ := json.Marshal(string(result))
	history = append(history, first, agent.Message{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: calls[0].ID, Content: content}}})
	last, err := p.Generate(ctx, history, nil, nil)
	if err != nil {
		return err
	}
	if last.Role != "assistant" || last.StopReason != "end_turn" {
		return errors.New("probe did not return a complete assistant receipt")
	}
	for _, block := range last.Content {
		if block.Type == "tool_use" {
			return errors.New("probe returned another tool call instead of its receipt")
		}
	}
	// Permit JSON whitespace, but reject duplicate keys, extra fields and prose.
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(last.Text())) != nil || !bytes.Equal(compact.Bytes(), result) {
		return errors.New("probe did not return the exact tool receipt")
	}
	return nil
}
