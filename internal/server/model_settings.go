package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	b "pwnmesh/internal/board"
	"pwnmesh/internal/kimi"
	"pwnmesh/internal/modelconfig"
	"pwnmesh/internal/provider"
)

func registerModelSettings(m *http.ServeMux, path string) {
	registerModelSettingsWithKimi(m, path, func(mode, proxy string) (*kimi.Client, error) {
		client, err := provider.ConnectionClient(mode, proxy)
		return &kimi.Client{HTTP: client}, err
	})
}

func registerModelSettingsWithKimi(m *http.ServeMux, path string, factory kimiClientFactory) *kimiLogins {
	getenv := func(string) string { return "" }
	if path != "" {
		getenv = os.Getenv
	}
	settings := &modelconfig.Store{Path: path, Defaults: modelconfig.Defaults(getenv)}
	logins := &kimiLogins{settings: settings, factory: factory, sessions: make(map[string]*kimiLogin)}
	logins.register(m)
	m.HandleFunc("GET /model-settings", func(w http.ResponseWriter, r *http.Request) {
		current, err := settings.Load()
		if err != nil {
			writeError(w, r, b.Err(503, err.Error()))
			return
		}
		modelJSON(w, logins.public(current))
	})
	m.HandleFunc("PUT /model-settings", func(w http.ResponseWriter, r *http.Request) {
		raw, err := modelPatch(w, r)
		if err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		current, err := logins.save(raw)
		if err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		modelJSON(w, logins.public(current))
	})
	m.HandleFunc("POST /model-settings/test", func(w http.ResponseWriter, r *http.Request) {
		raw, err := modelPatch(w, r)
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
		loginID, err := decodeModelPatch(raw, &current)
		if err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		if err = current.ReuseToken(previous); err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		var draft *kimiLogin
		if loginID != "" && loginID != current.CredentialID {
			draft, err = logins.authorized(loginID, current)
			if err != nil {
				writeError(w, r, b.Err(422, err.Error()))
				return
			}
			current.CredentialID = loginID
		}
		if err = current.Validate(true); err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		env := current.Overlay(nil)
		if draft != nil {
			// Draft credentials remain in memory until an explicit save.
			env["ANTHROPIC_AUTH_MODE"], env["ANTHROPIC_AUTH_TOKEN"] = "", draft.credential.AccessToken
		}
		p, err := provider.FromEnvironment(func(key string) string { return env[key] }, "")
		if err != nil {
			message := "invalid model connection settings"
			if errors.Is(err, kimi.ErrLoginRequired) {
				message = "Kimi Code sign-in is required; sign in again"
			}
			writeError(w, r, b.Err(422, message))
			return
		}
		if draft != nil {
			p.AuthMode, p.Client = "kimi_oauth", draft.client.HTTP
		}
		if p.Client != nil {
			defer p.Client.CloseIdleConnections()
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(min(current.RequestTimeout, 45))*time.Second)
		defer cancel()
		if draft != nil {
			stop := context.AfterFunc(draft.ctx, cancel)
			defer stop()
			if draft.ctx.Err() != nil {
				cancel()
			}
		}
		started := time.Now()
		p.MaxTokens = min(p.MaxTokens, provider.ProbeMaxTokens)
		err = provider.Probe(ctx, p)
		message := "Anthropic Messages and tool round trip verified"
		if current.Protocol == "openai" {
			message = "OpenAI Chat Completions and tool round trip verified"
		}
		if err != nil {
			message = "Model did not complete the tool round trip; check endpoint, protocol and model support"
			var status *provider.HTTPError
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
		modelJSON(w, map[string]any{"ok": err == nil, "latency_ms": time.Since(started).Milliseconds(), "message": message, "model": current.Model, "connection_mode": current.ConnectionMode})
	})
	return logins
}

func modelPatch(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<10))
	if err != nil || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return nil, errors.New("expected a JSON object (maximum 32 KiB)")
	}
	return raw, nil
}

func decodeModelPatch(raw []byte, current *modelconfig.Settings) (string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return "", errors.New("expected one JSON object")
	}
	for key := range fields {
		if strings.EqualFold(key, "credential_id") {
			return "", errors.New("credential_id is managed by the server")
		}
	}
	previousMode := current.AuthMode
	patch := struct {
		*modelconfig.Settings
		LoginID string `json:"login_id"`
	}{Settings: current}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil {
		return "", errors.New("invalid model settings fields")
	}
	if current.AuthMode != "kimi_oauth" || previousMode != "kimi_oauth" {
		current.CredentialID, current.CredentialDir = "", ""
	}
	if patch.LoginID != "" && (current.AuthMode != "kimi_oauth" || !modelconfig.ValidCredentialID(patch.LoginID)) {
		return "", errors.New("invalid Kimi login reference")
	}
	return patch.LoginID, nil
}

func modelJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(body)
}
