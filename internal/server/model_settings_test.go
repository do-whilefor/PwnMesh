package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/modelconfig"
)

func modelSettingsRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader(body)))
	return response
}

func TestModelCredentialsCannotFollowChangedOriginWithoutExplicitToken(t *testing.T) {
	var oldCalls, newCalls atomic.Int32
	oldOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { oldCalls.Add(1); w.WriteHeader(401) }))
	defer oldOrigin.Close()
	newOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newCalls.Add(1)
		if r.Header.Get("x-api-key") != "explicit-new-token" {
			t.Error("previous provider credential leaked to another origin")
		}
		w.WriteHeader(401)
	}))
	defer newOrigin.Close()
	path := filepath.Join(t.TempDir(), "settings.json")
	handler := NewWithModelSettings(nil, path)
	initial, _ := json.Marshal(map[string]any{"base_url": oldOrigin.URL, "token": "old-origin-token", "connection_mode": "direct"})
	if response := modelSettingsRequest(t, handler, "PUT", "/model-settings", string(initial)); response.Code != 200 {
		t.Fatalf("setup failed: %d", response.Code)
	}
	for _, method := range []string{"PUT", "POST"} {
		route := "/model-settings"
		if method == "POST" {
			route += "/test"
		}
		for _, token := range []any{nil, json.RawMessage("null"), "", "   "} {
			body := map[string]any{"base_url": newOrigin.URL}
			// Null and omitted token fields both mean no explicitly supplied key.
			if token != nil {
				body["token"] = token
			}
			raw, _ := json.Marshal(body)
			response := modelSettingsRequest(t, handler, method, route, string(raw))
			if response.Code != 422 {
				t.Fatalf("%s changed origin accepted empty token: %d", method, response.Code)
			}
			if newCalls.Load() != 0 || oldCalls.Load() != 0 {
				t.Fatal("rejected configuration still sent a model request")
			}
			saved, _, err := modelconfig.Read(path)
			if err != nil || saved.BaseURL != oldOrigin.URL || saved.Token != "old-origin-token" {
				t.Fatal("rejected origin change changed saved credentials")
			}
		}
	}
	explicit, _ := json.Marshal(map[string]any{"base_url": newOrigin.URL, "token": "explicit-new-token"})
	response := modelSettingsRequest(t, handler, "POST", "/model-settings/test", string(explicit))
	if response.Code != 200 || newCalls.Load() != 1 || oldCalls.Load() != 0 {
		t.Fatalf("explicit replacement token was not used: status=%d new=%d old=%d", response.Code, newCalls.Load(), oldCalls.Load())
	}
}

func TestModelCredentialsReuseSameOriginPathAndEffectivePort(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("x-api-key") != "same-origin-token" || r.URL.Path != "/new/v1/messages" {
			t.Error("same-origin token or path was not retained")
		}
		w.WriteHeader(401)
	}))
	defer origin.Close()
	path := filepath.Join(t.TempDir(), "settings.json")
	handler := NewWithModelSettings(nil, path)
	initial, _ := json.Marshal(map[string]any{"base_url": origin.URL + "/old", "token": "same-origin-token", "connection_mode": "direct"})
	if response := modelSettingsRequest(t, handler, "PUT", "/model-settings", string(initial)); response.Code != 200 {
		t.Fatalf("setup failed: %d", response.Code)
	}
	for _, method := range []string{"PUT", "POST"} {
		route := "/model-settings"
		if method == "POST" {
			route += "/test"
		}
		raw, _ := json.Marshal(map[string]any{"base_url": origin.URL + "/new", "token": "", "model": "changed-model", "proxy_url": "http://localhost:7897"})
		response := modelSettingsRequest(t, handler, method, route, string(raw))
		if response.Code != 200 {
			t.Fatalf("same-origin %s rejected token reuse: %d %s", method, response.Code, response.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("same-origin simulation calls=%d", calls.Load())
	}
	for _, tc := range []struct {
		before, after string
		allowed       bool
	}{
		{"https://MODEL.example/old", "https://model.example:443/new", true},
		{"http://model.example/old", "http://model.example:80/new", true},
		{"https://model.example", "http://model.example:443", false},
		{"https://model.example", "https://model.example:8443", false},
		{"https://model.example", "https://other.example", false},
	} {
		previous := modelconfig.Settings{BaseURL: tc.before, Token: "same-origin-token"}
		next := modelconfig.Settings{BaseURL: tc.after}
		err := next.ReuseToken(previous)
		if (err == nil) != tc.allowed || tc.allowed && next.Token != previous.Token {
			t.Errorf("origin reuse %s -> %s: %v", tc.before, tc.after, err)
		}
	}
}

func TestModelSettingsAPISecretPersistenceAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	handler := NewWithModelSettings(nil, path)
	put := modelSettingsRequest(t, handler, "PUT", "/model-settings", `{"token":"secret-fixture-only","model":"fixture","connection_mode":"proxy","proxy_url":"http://localhost:7897"}`)
	if put.Code != 200 || strings.Contains(put.Body.String(), "secret-fixture-only") {
		t.Fatalf("save failed or leaked key: %d", put.Code)
	}
	get := modelSettingsRequest(t, NewWithModelSettings(nil, path), "GET", "/model-settings", "")
	var public modelconfig.Public
	if err := json.Unmarshal(get.Body.Bytes(), &public); err != nil {
		t.Fatal(err)
	}
	if !public.HasToken || public.Token != "" || public.Model != "fixture" || public.EffectiveProxyURL != "http://host.docker.internal:7897" {
		t.Fatal("invalid public model settings")
	}
	put = modelSettingsRequest(t, handler, "PUT", "/model-settings", `{"token":"","model":"changed"}`)
	if put.Code != 200 {
		t.Fatalf("empty token save failed: %d", put.Code)
	}
	saved, _, err := modelconfig.Read(path)
	if err != nil || saved.Token != "secret-fixture-only" {
		t.Fatal("empty token destroyed saved secret")
	}
	for _, body := range []string{`{"protocol":"openai"}`, `{"connection_mode":"vpn"}`, `{"unknown":true}`, `null`, `{} {}`, `{"base_url":"https://user:secret@invalid"}`, `{"request_timeout":0}`, `{"base_url":"https://models.invalid:0","token":"replacement"}`, `{"base_url":"https://models.invalid:65536","token":"replacement"}`, `{"proxy_url":"http://proxy.invalid:0"}`, `{"proxy_url":"socks5h://proxy.invalid:65536"}`} {
		for method, route := range map[string]string{"PUT": "/model-settings", "POST": "/model-settings/test"} {
			response := modelSettingsRequest(t, handler, method, route, body)
			if response.Code != 422 {
				t.Errorf("invalid %s settings accepted (%s): %d", method, body, response.Code)
			}
		}
	}
	if current, _, err := modelconfig.Read(path); err != nil || current != saved {
		t.Fatal("invalid save or test changed the persisted configuration")
	}
	request := httptest.NewRequest("PUT", "/model-settings", strings.NewReader(`{"model":"cross-origin"}`))
	request.Header.Set("Origin", "https://attacker.invalid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatal("cross-origin settings mutation accepted")
	}
}

func TestModelSimulationUsesMessagesToolRoundTripWithoutSavingDraft(t *testing.T) {
	for _, mode := range []string{"direct", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.IsAbs() != (mode == "proxy") || mode == "proxy" && r.URL.Host != "models.invalid" {
					t.Error("simulation did not follow the configured connection mode")
				}
				if r.URL.Path != "/anthropic/v1/messages" || r.Header.Get("x-api-key") != "draft-secret" || r.Header.Get("anthropic-version") != "2023-06-01" {
					t.Error("simulation used wrong protocol or credentials")
				}
				var payload struct {
					Messages []agent.Message    `json:"messages"`
					Tools    []agent.Definition `json:"tools"`
					Stream   bool               `json:"stream"`
					Model    string             `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if !payload.Stream || payload.Model != "draft-model" {
					t.Error("simulation bypassed worker provider request")
				}
				w.Header().Set("Content-Type", "application/json")
				if calls == 1 {
					if len(payload.Tools) != 1 || payload.Tools[0].Name != "connection_check" {
						t.Error("simulation omitted its protocol tool")
					}
					_, _ = w.Write([]byte(`{"role":"assistant","content":[{"type":"tool_use","id":"check-1","name":"connection_check","input":{"ready":true}}],"stop_reason":"tool_use"}`))
					return
				}
				if len(payload.Messages) != 3 || payload.Messages[2].Content[0].ToolUseID != "check-1" {
					t.Error("simulation omitted matching tool receipt")
					return
				}
				var receipt string
				if json.Unmarshal(payload.Messages[2].Content[0].Content, &receipt) != nil || !strings.HasPrefix(receipt, "PWNMESH_OK_") || strings.Contains(payload.Messages[0].Text(), receipt) || len(payload.Tools) != 0 {
					t.Error("simulation did not challenge tool-result consumption without more tools")
				}
				_ = json.NewEncoder(w).Encode(agent.Message{Role: "assistant", Content: []agent.Block{{Type: "text", Text: receipt}}, StopReason: "end_turn"})
			}))
			// A wildcard listener stays in this test's network namespace; unlike a
			// loopback proxy, it must not be rewritten to the Docker host by the provider.
			listener, err := net.Listen("tcp4", "0.0.0.0:0")
			if err != nil {
				t.Fatal(err)
			}
			_ = upstream.Listener.Close()
			upstream.Listener = listener
			upstream.Start()
			defer upstream.Close()
			t.Setenv("HTTP_PROXY", "http://ambient.invalid:7897")
			t.Setenv("HTTPS_PROXY", "http://ambient.invalid:7897")
			path := filepath.Join(t.TempDir(), "settings.json")
			handler := NewWithModelSettings(nil, path)
			base := upstream.URL + "/anthropic"
			if mode == "proxy" {
				base = "http://models.invalid/anthropic"
			}
			body, _ := json.Marshal(map[string]any{"token": "draft-secret", "base_url": base, "model": "draft-model", "connection_mode": mode, "proxy_url": upstream.URL})
			response := modelSettingsRequest(t, handler, "POST", "/model-settings/test", string(body))
			if response.Code != 200 || !strings.Contains(response.Body.String(), `"ok":true`) || calls != 2 {
				t.Fatalf("simulation failed: %d %s, calls=%d", response.Code, response.Body.String(), calls)
			}
			if _, found, err := modelconfig.Read(path); err != nil || found {
				t.Fatal("test persisted unsaved draft")
			}
		})
	}
}

func TestModelSimulationRejectsFalseSuccessAndRedactsUpstreamError(t *testing.T) {
	for _, status := range []int{200, 401} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"role":"assistant","content":[{"type":"text","text":"secret-should-not-be-echoed"}],"stop_reason":"end_turn"}`))
		}))
		body, _ := json.Marshal(map[string]any{"token": "secret-should-not-be-echoed", "base_url": upstream.URL, "connection_mode": "direct"})
		response := modelSettingsRequest(t, New(nil), "POST", "/model-settings/test", string(body))
		upstream.Close()
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"ok":false`) || strings.Contains(response.Body.String(), "secret-should-not-be-echoed") {
			t.Fatalf("false success or leaked error: %d %s", response.Code, response.Body.String())
		}
	}
	for _, scenario := range []string{"canned_receipt", "extra_argument", "wrong_role"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				response := agent.Message{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "check-1", Name: "connection_check", Input: json.RawMessage(`{"ready":true}`)}}, StopReason: "tool_use"}
				if calls == 1 {
					switch scenario {
					case "extra_argument":
						response.Content[0].Input = json.RawMessage(`{"ready":true,"ignored":true}`)
					case "wrong_role":
						response.Role = "user"
					}
				} else {
					response.Content = []agent.Block{{Type: "text", Text: "PWNMESH_OK"}}
					response.StopReason = "end_turn"
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer upstream.Close()
			body, _ := json.Marshal(map[string]any{"token": "fixture-secret", "base_url": upstream.URL, "connection_mode": "direct"})
			response := modelSettingsRequest(t, New(nil), "POST", "/model-settings/test", string(body))
			wantCalls := 1
			if scenario == "canned_receipt" {
				wantCalls = 2
			}
			if response.Code != 200 || !strings.Contains(response.Body.String(), `"ok":false`) || calls != wantCalls {
				t.Fatalf("invalid probe passed or kept calling: %d %s, calls=%d", response.Code, response.Body.String(), calls)
			}
		})
	}
}
