package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	b "pwnmesh/internal/board"
	"pwnmesh/internal/kimi"
	"pwnmesh/internal/modelauth"
	"pwnmesh/internal/modelconfig"
	"pwnmesh/internal/provider"
)

func registerModelSettings(m *http.ServeMux, path string) {
	registerModelSettingsWithKimi(m, path, func(mode, proxy string) (*kimi.Client, error) {
		client, err := provider.ConnectionClient(mode, proxy)
		return &kimi.Client{HTTP: client}, err
	})
}

func registerModelSettingsWithKimi(m *http.ServeMux, path string, factory kimiClientFactory) *modelSettings {
	getenv := func(string) string { return "" }
	if path != "" {
		getenv = os.Getenv
	}
	settings := &modelconfig.Store{Path: path, Defaults: modelconfig.Defaults(getenv)}
	logins := &kimiLogins{factory: factory, sessions: make(map[string]*kimiLogin)}
	models := &modelSettings{store: settings, logins: logins, credentials: &kimi.Store{Dir: modelconfig.AuthDir(path)}}
	logins.register(m, path != "")
	m.HandleFunc("GET /model-settings", func(w http.ResponseWriter, r *http.Request) {
		current, err := settings.Load()
		if err != nil {
			writeError(w, r, b.Err(503, err.Error()))
			return
		}
		modelJSON(w, models.public(r.Context(), current))
	})
	m.HandleFunc("PUT /model-settings", func(w http.ResponseWriter, r *http.Request) {
		raw, err := modelPatch(w, r)
		if err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		current, err := models.save(r.Context(), raw)
		if err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		modelJSON(w, models.public(r.Context(), current))
	})
	m.HandleFunc("POST /model-settings/kimi/logout", func(w http.ResponseWriter, r *http.Request) {
		current, err := models.logout()
		if err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		modelJSON(w, current.Public())
	})
	m.HandleFunc("POST /model-settings/test", func(w http.ResponseWriter, r *http.Request) {
		raw, err := modelPatch(w, r)
		if err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		result, err := models.probe(r.Context(), raw)
		if err != nil {
			writeError(w, r, err)
			return
		}
		modelJSON(w, result)
	})
	return models
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
	if patch.LoginID != "" && (current.AuthMode != "kimi_oauth" || !modelauth.ValidCredentialID(patch.LoginID)) {
		return "", errors.New("invalid Kimi login reference")
	}
	return patch.LoginID, nil
}

func modelJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(body)
}
