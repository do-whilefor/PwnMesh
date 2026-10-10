package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"pwnmesh/internal/agent"
	b "pwnmesh/internal/board"
	"pwnmesh/internal/modelconfig"
	"pwnmesh/internal/provider"
)

func registerModelSettings(m *http.ServeMux, path string) {
	getenv := func(string) string { return "" }
	if path != "" {
		getenv = os.Getenv
	}
	settings := &modelconfig.Store{Path: path, Defaults: modelconfig.Defaults(getenv)}
	m.HandleFunc("GET /model-settings", func(w http.ResponseWriter, r *http.Request) {
		current, err := settings.Load()
		if err != nil {
			writeError(w, r, b.Err(503, err.Error()))
			return
		}
		modelJSON(w, current.Public())
	})
	patch := func(w http.ResponseWriter, r *http.Request) ([]byte, error) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<10))
		if err != nil || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
			return nil, errors.New("expected a JSON object (maximum 32 KiB)")
		}
		return raw, nil
	}
	decode := func(raw []byte, current *modelconfig.Settings) error {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(current); err != nil {
			return errors.New("invalid model settings fields")
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return errors.New("expected one JSON object")
		}
		return nil
	}
	m.HandleFunc("PUT /model-settings", func(w http.ResponseWriter, r *http.Request) {
		raw, err := patch(w, r)
		if err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		current, err := settings.Update(func(s *modelconfig.Settings) error { return decode(raw, s) })
		if err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		modelJSON(w, current.Public())
	})
	m.HandleFunc("POST /model-settings/test", func(w http.ResponseWriter, r *http.Request) {
		raw, err := patch(w, r)
		if err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		current, err := settings.Load()
		if err != nil {
			writeError(w, r, b.Err(503, err.Error()))
			return
		}
		previous := current
		current.Token = ""
		if err = decode(raw, &current); err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		if err = current.ReuseToken(previous); err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		if err = current.Validate(true); err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		env := current.Overlay(nil)
		p, err := provider.FromEnvironment(func(key string) string { return env[key] }, "")
		if err != nil {
			writeError(w, r, b.Err(422, "invalid model connection settings"))
			return
		}
		if p.Client != nil {
			defer p.Client.CloseIdleConnections()
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(min(current.RequestTimeout, 45))*time.Second)
		defer cancel()
		started := time.Now()
		err = probeModel(ctx, p)
		message := "Anthropic Messages and tool round trip verified"
		if err != nil {
			message = "Model did not complete the Messages tool round trip; check endpoint, protocol and model support"
			var status *provider.HTTPError
			if errors.As(err, &status) {
				message = status.Error()
			}
			if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				message = "Model connection test timed out or was cancelled"
			}
		}
		modelJSON(w, map[string]any{"ok": err == nil, "latency_ms": time.Since(started).Milliseconds(), "message": message, "model": current.Model, "connection_mode": current.ConnectionMode})
	})
}

func modelJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(body)
}

// The simulation has no real tools or project side effects. It exercises the
// exact streaming provider used by workers, including one tool-result exchange.
func probeModel(ctx context.Context, p *provider.Anthropic) error {
	p.MaxTokens = min(p.MaxTokens, 1024)
	definition := agent.Definition{Name: "connection_check", Description: "Call once with ready:true, then repeat exactly the receipt in its result.", Schema: json.RawMessage(`{"type":"object","properties":{"ready":{"type":"boolean","enum":[true]}},"required":["ready"],"additionalProperties":false}`)}
	history := []agent.Message{agent.Text("user", "Connection test only. Call connection_check once with {\"ready\":true}. After the tool result, reply exactly with its receipt and no other text.")}
	first, err := p.Generate(ctx, history, []agent.Definition{definition}, nil)
	if err != nil {
		return err
	}
	id := ""
	for _, block := range first.Content {
		if block.Type != "tool_use" {
			continue
		}
		if id != "" || block.Name != definition.Name || block.ID == "" || agent.ValidateArguments(definition.Schema, block.Input) != nil {
			return errors.New("invalid simulation tool call")
		}
		id = block.ID
	}
	if id == "" || first.Role != "assistant" || first.StopReason != "tool_use" {
		return errors.New("model did not call simulation tool")
	}
	// Only the tool result reveals this receipt. A canned reply must not pass a
	// test intended to prove that the model consumes tool-result history.
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	receipt := "PWNMESH_OK_" + hex.EncodeToString(nonce[:])
	content, _ := json.Marshal(receipt)
	history = append(history, first, agent.Message{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: id, Content: content}}})
	second, err := p.Generate(ctx, history, nil, nil)
	if err != nil {
		return err
	}
	var answer strings.Builder
	for _, block := range second.Content {
		if block.Type == "tool_use" {
			return errors.New("unexpected simulation tool call")
		}
		if block.Type == "text" {
			answer.WriteString(block.Text)
		}
	}
	if strings.TrimSpace(answer.String()) != receipt || second.Role != "assistant" || second.StopReason != "end_turn" {
		return errors.New("model did not return simulation receipt")
	}
	return nil
}
