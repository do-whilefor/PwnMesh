package server

import (
	"context"
	"errors"
	"time"

	b "pwnmesh/internal/board"
	"pwnmesh/internal/kimi"
	"pwnmesh/internal/modelwire"
	"pwnmesh/internal/provider"
)

type modelProbe struct {
	OK             bool   `json:"ok"`
	LatencyMS      int64  `json:"latency_ms"`
	Message        string `json:"message"`
	Model          string `json:"model"`
	ConnectionMode string `json:"connection_mode"`
}

// probe uses a private configuration draft and never persists credentials.
func (m *modelSettings) probe(ctx context.Context, raw []byte) (modelProbe, error) {
	current, err := m.store.Load()
	if err != nil {
		return modelProbe{}, b.Err(503, err.Error())
	}
	previous := current
	current.Token = ""
	loginID, err := decodeModelPatch(raw, &current)
	if err != nil {
		return modelProbe{}, b.Err(422, err.Error())
	}
	if err = current.ReuseToken(previous); err != nil {
		return modelProbe{}, b.Err(422, err.Error())
	}
	var draft *kimiLogin
	if loginID != "" && loginID != current.CredentialID {
		draft, err = m.logins.authorized(loginID, current)
		if err != nil {
			return modelProbe{}, b.Err(422, err.Error())
		}
		current.CredentialID = loginID
	}
	if err = current.Validate(true); err != nil {
		return modelProbe{}, b.Err(422, err.Error())
	}
	// Credential reads can wait for a refresh in another process. Include that
	// wait in the same budget as the upstream model request.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(min(current.RequestTimeout, 45))*time.Second)
	defer cancel()
	if draft != nil {
		stop := context.AfterFunc(draft.ctx, cancel)
		defer stop()
		if draft.ctx.Err() != nil {
			cancel()
		}
	}
	started := time.Now()
	env := current.Overlay(nil)
	if draft != nil {
		// Draft credentials remain in memory until an explicit save.
		env["ANTHROPIC_AUTH_MODE"], env["ANTHROPIC_AUTH_TOKEN"] = "", draft.credential.AccessToken
	}
	p, err := provider.FromEnvironment(ctx, func(key string) string { return env[key] }, "")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return modelProbe{LatencyMS: time.Since(started).Milliseconds(), Message: "Model connection test timed out or was cancelled", Model: current.Model, ConnectionMode: current.ConnectionMode}, nil
		}
		message := "invalid model connection settings"
		if errors.Is(err, kimi.ErrLoginRequired) {
			message = "Kimi Code sign-in is required; sign in again"
		}
		return modelProbe{}, b.Err(422, message)
	}
	if draft != nil {
		p.AuthMode, p.Client = "kimi_oauth", draft.client.HTTP
	}
	if p.Client != nil {
		defer p.Client.CloseIdleConnections()
	}
	p.MaxTokens = min(p.MaxTokens, provider.ProbeMaxTokens)
	err = provider.Probe(ctx, p)
	message := "Anthropic Messages and tool round trip verified"
	if current.Protocol == "openai" {
		message = "OpenAI Chat Completions and tool round trip verified"
	}
	if err != nil {
		message = "Model did not complete the tool round trip; check endpoint, protocol and model support"
		var status *modelwire.HTTPError
		if errors.As(err, &status) {
			message = status.Error()
		}
		if errors.Is(err, kimi.ErrLoginRequired) {
			message = "Kimi Code sign-in is required; sign in again"
		}
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			message = "Model connection test timed out or was cancelled"
		}
	}
	return modelProbe{OK: err == nil, LatencyMS: time.Since(started).Milliseconds(), Message: message, Model: current.Model, ConnectionMode: current.ConnectionMode}, nil
}
