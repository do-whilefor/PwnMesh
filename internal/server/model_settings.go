package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

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
		p.MaxTokens = min(p.MaxTokens, provider.ProbeMaxTokens)
		err = provider.Probe(ctx, p)
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
