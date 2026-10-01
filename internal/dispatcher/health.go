package dispatcher

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/config"
	"pwnmesh/internal/provider"
)

// Readiness verifies a small tool round trip, not task quality or the model's
// full context window. Each strategy gets two turns under one worker deadline;
// provider transport retries remain bounded by that same deadline.
const healthMaxTokens = 1024

type healthFailure struct {
	Kind string
	Err  error
}

func (e *healthFailure) Error() string { return "model readiness " + e.Kind + ": " + e.Err.Error() }
func (e *healthFailure) Unwrap() error { return e.Err }

// An explicit endpoint protocol/configuration rejection is not repaired by a five-second
// delay. Keep that worker out of new assignments until a successful explicit
// readiness check (or the next process startup). Unknown/transient failures
// retain the existing bounded cooldown and never claim incompatibility.
func (s *Scheduler) recordHealth(name string, err error) {
	if err == nil {
		delete(s.incompatible, name)
		delete(s.unhealthy, name)
		return
	}
	var failure *healthFailure
	if errors.As(err, &failure) && (failure.Kind == "incompatible" || failure.Kind == "configuration") {
		if s.incompatible == nil {
			s.incompatible = map[string]string{}
		}
		s.incompatible[name] = failure.Kind
		delete(s.unhealthy, name)
		return
	}
	if s.unhealthy == nil {
		s.unhealthy = map[string]time.Time{}
	}
	s.unhealthy[name] = time.Now().Add(5 * time.Second)
}

func (s *Scheduler) health(ctx context.Context, w config.Worker, budgets ...config.Task) error {
	if s.CheckHealth != nil {
		return s.CheckHealth(ctx, w)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.Runtime.HealthTimeout)*time.Second)
	defer cancel()
	if len(budgets) == 0 {
		for _, kind := range w.TaskTypes {
			budgets = append(budgets, s.Config.Task(kind))
		}
		if len(budgets) == 0 {
			budgets = []config.Task{{}}
		}
	}
	getenv := func(key string) string {
		if value, ok := w.Env[key]; ok {
			return value
		}
		if suffix, ok := strings.CutPrefix(key, "PWNMESH_"); ok {
			return w.Env["XLOOM_"+suffix]
		}
		return ""
	}
	seen := map[string]bool{}
	for _, budget := range budgets {
		p, err := provider.FromEnvironment(getenv, budget.ReasoningEffort)
		if err != nil {
			return &healthFailure{Kind: "configuration", Err: err}
		}
		if seen[p.ReasoningEffort] {
			continue
		}
		seen[p.ReasoningEffort] = true
		// Match role effort without asking an unverified model to consume the
		// much larger task output allowance. Truncation means unverified.
		p.MaxTokens = min(p.MaxTokens, healthMaxTokens)
		if err := probeHealth(ctx, p); err != nil {
			return fmt.Errorf("effort %s: %w", p.ReasoningEffort, err)
		}
	}
	return nil
}

func healthRequestFailure(err error) error {
	kind := "unverified"
	var model *agent.ModelError
	var status *provider.HTTPError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		kind = "unverified"
	case errors.As(err, &model) && (model.Kind == agent.ErrorContextOverflow || model.Kind == agent.ErrorBudget):
		kind = "unverified"
	case errors.As(err, &status) && (status.Status == 401 || status.Status == 403):
		kind = "configuration"
	case errors.As(err, &status) && (status.Status == 400 || status.Status == 404 || status.Status == 405 || status.Status == 415 || status.Status == 422):
		kind = "incompatible"
	case errors.As(err, &model):
		switch model.Kind {
		case agent.ErrorTransport, agent.ErrorRateLimit, agent.ErrorUnavailable:
			kind = "transient"
		case agent.ErrorContextOverflow, agent.ErrorBudget:
			kind = "unverified"
		}
	}
	return &healthFailure{Kind: kind, Err: err}
}

func probeHealth(ctx context.Context, p agent.Provider) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return &healthFailure{Kind: "unverified", Err: err}
	}
	tool := agent.Definition{Name: "readiness_check", Description: "A no-effect protocol check. Call once with ready:true, then return exactly the JSON object supplied in its result.", Schema: json.RawMessage(`{"type":"object","properties":{"ready":{"type":"boolean","enum":[true]}},"required":["ready"],"additionalProperties":false}`)}
	history := []agent.Message{agent.Text("user", "Verify tool support. Call readiness_check exactly once with {\"ready\":true}. After receiving its result, return exactly that JSON object, without commentary or further calls.")}
	first, err := p.Generate(ctx, history, []agent.Definition{tool}, nil)
	if err != nil {
		return healthRequestFailure(err)
	}
	check := func(m agent.Message) error {
		if m.StopReason == "max_tokens" || m.StopReason == "length" {
			return &healthFailure{Kind: "unverified", Err: errors.New("probe output allowance exhausted")}
		}
		if m.Role != "assistant" || m.StopReason == "" {
			return &healthFailure{Kind: "unverified", Err: errors.New("invalid assistant response")}
		}
		return nil
	}
	if err := check(first); err != nil {
		return err
	}
	var calls []agent.Block
	for _, block := range first.Content {
		if block.Type == "tool_use" {
			calls = append(calls, block)
		}
	}
	if len(calls) != 1 || calls[0].ID == "" || calls[0].Name != tool.Name || agent.ValidateArguments(tool.Schema, calls[0].Input) != nil {
		return &healthFailure{Kind: "unverified", Err: errors.New("probe requires exactly one valid readiness_check call")}
	}
	// The challenge only appears in the tool result, so a canned first-turn
	// answer cannot demonstrate that the model accepts tool-result history.
	expected := hex.EncodeToString(nonce[:])
	result, _ := json.Marshal(map[string]string{"ready": expected})
	content, _ := json.Marshal(string(result))
	history = append(history, first, agent.Message{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: calls[0].ID, Content: content}}})
	last, err := p.Generate(ctx, history, nil, nil)
	if err != nil {
		return healthRequestFailure(err)
	}
	if err := check(last); err != nil {
		return err
	}
	for _, block := range last.Content {
		if block.Type == "tool_use" {
			return &healthFailure{Kind: "unverified", Err: errors.New("probe returned another tool call instead of its receipt")}
		}
	}
	// Require the complete receipt, allowing JSON whitespace but no duplicate
	// keys, additional fields, prose or trailing values.
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(last.Text())) != nil || !bytes.Equal(compact.Bytes(), result) {
		return &healthFailure{Kind: "unverified", Err: errors.New("probe did not return the exact tool receipt")}
	}
	return nil
}
