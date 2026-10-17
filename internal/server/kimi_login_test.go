package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/kimi"
	"pwnmesh/internal/modelconfig"
)

type kimiRoundTrip func(*http.Request) (*http.Response, error)

func (f kimiRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type kimiHarness struct {
	t          *testing.T
	mux        *http.ServeMux
	logins     *kimiLogins
	settings   *modelSettings
	path       string
	client     *kimi.Client
	pollBody   string
	pollStatus int
	polls      atomic.Int32
	models     atomic.Int32
}

func kimiResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func newKimiHarness(t *testing.T) *kimiHarness {
	t.Helper()
	t.Setenv("PWNMESH_AUTH_DIR", "")
	t.Setenv("PWNMESH_MODEL_PROTOCOL", "anthropic")
	h := &kimiHarness{t: t, mux: http.NewServeMux(), path: filepath.Join(t.TempDir(), "models.json"), pollStatus: 200, pollBody: `{"access_token":"oauth-access-secret","refresh_token":"oauth-refresh-secret","expires_in":3600}`}
	h.client = &kimi.Client{HTTP: &http.Client{Transport: kimiRoundTrip(h.roundTrip)}}
	h.settings = registerModelSettingsWithKimi(h.mux, h.path, func(mode, proxy string) (*kimi.Client, error) { return h.client, nil })
	h.logins = h.settings.logins
	t.Cleanup(func() {
		h.logins.mu.Lock()
		defer h.logins.mu.Unlock()
		for id := range h.logins.sessions {
			h.logins.remove(id)
		}
	})
	return h
}

func (h *kimiHarness) roundTrip(r *http.Request) (*http.Response, error) {
	switch r.URL.String() {
	case "https://auth.kimi.com/api/oauth/device_authorization":
		return kimiResponse(r, 200, `{"device_code":"device-secret","user_code":"CODE","verification_uri":"https://kimi.com/verify","verification_uri_complete":"https://kimi.com/verify?code=CODE","interval":5,"expires_in":900}`), nil
	case "https://auth.kimi.com/api/oauth/token":
		h.polls.Add(1)
		if r.ParseForm() != nil || r.Form.Get("device_code") != "device-secret" {
			h.t.Error("device grant did not use the server-held code")
		}
		return kimiResponse(r, h.pollStatus, h.pollBody), nil
	case "https://api.kimi.com/coding/v1/messages":
		call := h.models.Add(1)
		if r.Header.Get("Authorization") != "Bearer oauth-access-secret" || r.Header.Get("x-api-key") != "" {
			h.t.Error("Kimi probe did not use Bearer-only subscription authentication")
		}
		var payload struct {
			Messages []agent.Message    `json:"messages"`
			Tools    []agent.Definition `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			h.t.Error(err)
		}
		if call%2 == 1 {
			if len(payload.Tools) != 1 || payload.Tools[0].Name != "readiness_check" {
				h.t.Error("Kimi probe omitted readiness tool")
			}
			return kimiResponse(r, 200, `{"role":"assistant","content":[{"type":"tool_use","id":"check","name":"readiness_check","input":{"ready":true}}],"stop_reason":"tool_use"}`), nil
		}
		if len(payload.Messages) != 3 || len(payload.Messages[2].Content) != 1 || payload.Messages[2].Content[0].ToolUseID != "check" || len(payload.Tools) != 0 {
			h.t.Error("Kimi probe lost matching tool result")
			return kimiResponse(r, 400, `{}`), nil
		}
		var receipt string
		if err := json.Unmarshal(payload.Messages[2].Content[0].Content, &receipt); err != nil {
			h.t.Error(err)
		}
		body, _ := json.Marshal(agent.Message{Role: "assistant", Content: []agent.Block{{Type: "text", Text: receipt}}, StopReason: "end_turn"})
		return kimiResponse(r, 200, string(body)), nil
	default:
		h.t.Error("request escaped the pinned Kimi endpoints")
		return kimiResponse(r, 400, `{}`), nil
	}
}

func (h *kimiHarness) request(method, route, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	r := modelSettingsRequest(h.t, h.mux, method, route, body)
	for _, secret := range []string{"device-secret", "oauth-access-secret", "oauth-refresh-secret", "rotated-access-secret", "rotated-refresh-secret", "general-api-secret", "coding-plan-secret", `"credential_id"`, `"credential_dir"`} {
		if strings.Contains(r.Body.String(), secret) {
			h.t.Fatal("model settings response leaked credentials or internal references")
		}
	}
	return r
}

func (h *kimiHarness) start() string {
	h.t.Helper()
	r := h.request("POST", "/model-settings/kimi/login", `{"connection_mode":"direct","proxy_url":"http://host.docker.internal:7897"}`)
	var response struct {
		LoginID  string `json:"login_id"`
		UserCode string `json:"user_code"`
		URI      string `json:"verification_uri"`
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &response) != nil || !modelconfig.ValidCredentialID(response.LoginID) || response.UserCode != "CODE" || response.URI != "https://kimi.com/verify?code=CODE" || r.Header().Get("Cache-Control") != "no-store" {
		h.t.Fatalf("cannot start Kimi login: %d %s", r.Code, r.Body.String())
	}
	return response.LoginID
}

func (h *kimiHarness) ready(id string) {
	h.t.Helper()
	h.logins.mu.Lock()
	defer h.logins.mu.Unlock()
	if session := h.logins.sessions[id]; session != nil {
		session.next = time.Now().Add(-time.Second)
	} else {
		h.t.Fatal("login session is missing")
	}
}

func (h *kimiHarness) authorized() string {
	h.t.Helper()
	id := h.start()
	h.ready(id)
	r := h.request("POST", "/model-settings/kimi/login/"+id, "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"authorized"`) {
		h.t.Fatalf("cannot authorize login: %d %s", r.Code, r.Body.String())
	}
	return id
}

func kimiDraft(id string) map[string]any {
	return map[string]any{"auth_mode": "kimi_oauth", "protocol": "anthropic", "base_url": "https://api.kimi.com/coding", "model": "kimi-for-coding", "connection_mode": "direct", "proxy_url": "http://host.docker.internal:7897", "login_id": id}
}

func encodeKimiDraft(body map[string]any) string { raw, _ := json.Marshal(body); return string(raw) }

func TestAPISettingsSaveDoesNotWaitForKimiSessionLock(t *testing.T) {
	h := newKimiHarness(t)
	h.logins.mu.Lock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- modelSettingsRequest(t, h.mux, "PUT", "/model-settings", `{"auth_mode":"api_key","token":"general-api-secret","model":"independent-save"}`)
	}()
	var response *httptest.ResponseRecorder
	select {
	case response = <-done:
		h.logins.mu.Unlock()
	case <-time.After(time.Second):
		h.logins.mu.Unlock()
		<-done
		t.Fatal("ordinary API settings depend on the Kimi session lock")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", response.Code, response.Body.String())
	}
	saved, found, err := modelconfig.Read(h.path)
	if err != nil || !found || saved.Model != "independent-save" || saved.Token != "general-api-secret" {
		t.Fatal("API settings were not persisted independently")
	}
}

func TestKimiLoginPollingThrottlesAndHonorsSlowDown(t *testing.T) {
	h := newKimiHarness(t)
	id := h.start()
	r := h.request("POST", "/model-settings/kimi/login/"+id, "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"pending"`) || h.polls.Load() != 0 {
		t.Fatal("initial poll bypassed the OAuth interval")
	}
	h.pollStatus, h.pollBody = 400, `{"error":"authorization_pending"}`
	h.ready(id)
	r = h.request("POST", "/model-settings/kimi/login/"+id, "")
	if !strings.Contains(r.Body.String(), `"status":"pending"`) || h.polls.Load() != 1 {
		t.Fatal("pending authorization did not poll once")
	}
	h.pollStatus, h.pollBody = 429, `{"error":"slow_down","interval":20}`
	h.ready(id)
	r = h.request("POST", "/model-settings/kimi/login/"+id, "")
	if !strings.Contains(r.Body.String(), `"status":"slow_down"`) || !strings.Contains(r.Body.String(), `"interval":20`) || h.polls.Load() != 2 {
		t.Fatal("slow_down did not extend polling interval")
	}
	r = h.request("POST", "/model-settings/kimi/login/"+id, "")
	if !strings.Contains(r.Body.String(), `"status":"pending"`) || h.polls.Load() != 2 {
		t.Fatal("repeated browser polling bypassed slow_down")
	}
}

func TestKimiLoginDeniedExpiredAndCancelledSessions(t *testing.T) {
	for _, outcome := range []string{"denied", "expired", "local expiry", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			h := newKimiHarness(t)
			id := h.start()
			h.ready(id)
			want := outcome
			switch outcome {
			case "denied":
				h.pollStatus, h.pollBody = 400, `{"error":"access_denied"}`
			case "expired":
				h.pollStatus, h.pollBody = 400, `{"error":"expired_token"}`
			case "local expiry":
				h.logins.mu.Lock()
				h.logins.sessions[id].expires = time.Now().Add(-time.Second)
				h.logins.mu.Unlock()
				want = "expired"
			case "cancelled":
				if r := h.request("DELETE", "/model-settings/kimi/login/"+id, ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"cancelled"`) {
					t.Fatal("cancellation failed")
				}
				want = "expired"
			}
			r := h.request("POST", "/model-settings/kimi/login/"+id, "")
			if r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"`+want+`"`) {
				t.Fatalf("unexpected terminal state: %d %s", r.Code, r.Body.String())
			}
			h.logins.mu.Lock()
			remains := h.logins.sessions[id] != nil
			h.logins.mu.Unlock()
			if remains {
				t.Fatal("terminal login retained server session")
			}
			if r := h.request("PUT", "/model-settings", encodeKimiDraft(kimiDraft(id))); r.Code != 422 {
				t.Fatal("terminal login remained saveable")
			}
		})
	}
}

func TestKimiLoginCancellationAbortsInflightPoll(t *testing.T) {
	h := newKimiHarness(t)
	id := h.start()
	h.ready(id)
	started := make(chan struct{})
	h.client.HTTP.Transport = kimiRoundTrip(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- modelSettingsRequest(t, h.mux, "POST", "/model-settings/kimi/login/"+id, "") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	if r := h.request("DELETE", "/model-settings/kimi/login/"+id, ""); r.Code != 200 {
		t.Fatal("could not cancel inflight authorization")
	}
	select {
	case r := <-done:
		if r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"expired"`) {
			t.Fatal("cancelled poll did not discard its result")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not abort the upstream request")
	}
}

func TestKimiAuthorizedDraftTestsSavesReloadsAndLogsOut(t *testing.T) {
	h := newKimiHarness(t)
	id := h.authorized()
	if r := h.request("POST", "/model-settings/kimi/login/"+id, ""); r.Code != 200 || h.polls.Load() != 1 {
		t.Fatal("authorized draft polled for another access token")
	}
	r := h.request("POST", "/model-settings/test", encodeKimiDraft(kimiDraft(id)))
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"ok":true`) || h.models.Load() != 2 {
		t.Fatalf("authorized tool probe failed: %d %s", r.Code, r.Body.String())
	}
	if _, found, err := modelconfig.Read(h.path); err != nil || found {
		t.Fatal("testing a draft persisted settings")
	}
	credentialPath := filepath.Join(modelconfig.AuthDir(h.path), id+".json")
	if _, err := os.Stat(credentialPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("testing persisted unsaved OAuth credentials")
	}
	r = h.request("PUT", "/model-settings", encodeKimiDraft(kimiDraft(id)))
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"has_token":true`) || strings.Contains(r.Body.String(), id) {
		t.Fatalf("save failed or exposed reference: %d %s", r.Code, r.Body.String())
	}
	saved, found, err := modelconfig.Read(h.path)
	if err != nil || !found || saved.CredentialID != id || saved.Token != "" || saved.AuthMode != "kimi_oauth" {
		t.Fatalf("OAuth settings were not persisted separately: %v", err)
	}
	credentials := &kimi.Store{Dir: modelconfig.AuthDir(h.path)}
	credential, err := credentials.Resolve(context.Background(), id)
	if err != nil || credential.AccessToken != "oauth-access-secret" || credential.RefreshToken != "oauth-refresh-secret" {
		t.Fatalf("saved credential source failed: %v", err)
	}
	raw, err := os.ReadFile(h.path)
	if err != nil || strings.Contains(string(raw), "oauth-access-secret") || strings.Contains(string(raw), "oauth-refresh-secret") {
		t.Fatal("settings persisted OAuth tokens inline")
	}
	reloaded := http.NewServeMux()
	registerModelSettingsWithKimi(reloaded, h.path, func(mode, proxy string) (*kimi.Client, error) { return h.client, nil })
	r = modelSettingsRequest(t, reloaded, "GET", "/model-settings", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"has_token":true`) || strings.Contains(r.Body.String(), id) || strings.Contains(r.Body.String(), "secret") {
		t.Fatal("reload lost subscription state or exposed credentials")
	}
	r = modelSettingsRequest(t, reloaded, "POST", "/model-settings/kimi/logout", `{}`)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"has_token":false`) {
		t.Fatalf("logout failed: %d %s", r.Code, r.Body.String())
	}
	if _, err := credentials.Read(id); !errors.Is(err, kimi.ErrLoginRequired) {
		t.Fatal("logout retained OAuth credentials")
	}
	saved, _, err = modelconfig.Read(h.path)
	if err != nil || saved.CredentialID != "" || saved.Token != "" {
		t.Fatal("logout retained settings credentials")
	}
}

func TestKimiInvalidDraftsCannotPersistOrRedirectCredentials(t *testing.T) {
	h := newKimiHarness(t)
	id := h.authorized()
	for _, tc := range []struct {
		name  string
		patch func(map[string]any)
	}{
		{"injected reference", func(body map[string]any) { body["credential_id"] = id }},
		{"cased reference", func(body map[string]any) { body["CREDENTIAL_ID"] = id }},
		{"injected location", func(body map[string]any) { body["credential_dir"] = "/tmp/attacker" }},
		{"offsite endpoint", func(body map[string]any) { body["base_url"] = "https://attacker.invalid/" }},
		{"wrong protocol", func(body map[string]any) { body["protocol"] = "openai" }},
		{"changed proxy", func(body map[string]any) { body["proxy_url"] = "http://attacker.invalid:7897" }},
		{"manual token", func(body map[string]any) { body["token"] = "general-api-secret" }},
		{"invalid allowance", func(body map[string]any) { body["max_tokens"] = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := kimiDraft(id)
			tc.patch(body)
			for method, route := range map[string]string{"PUT": "/model-settings", "POST": "/model-settings/test"} {
				if r := h.request(method, route, encodeKimiDraft(body)); r.Code != 422 {
					t.Fatalf("invalid subscription draft accepted: %d %s", r.Code, r.Body.String())
				}
			}
			if h.models.Load() != 0 {
				t.Fatal("invalid draft sent a model request")
			}
			if _, found, err := modelconfig.Read(h.path); err != nil || found {
				t.Fatal("invalid draft changed settings")
			}
			if _, err := os.Stat(filepath.Join(modelconfig.AuthDir(h.path), id+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid draft persisted credentials")
			}
		})
	}
}

func TestKimiFailedSettingsWriteRollsBackNewCredentials(t *testing.T) {
	h := newKimiHarness(t)
	id := h.authorized()
	h.settings.store.Path = filepath.Join(t.TempDir(), "directory-instead-of-settings") + string(os.PathSeparator)
	h.settings.credentials = &kimi.Store{Dir: modelconfig.AuthDir(h.settings.store.Path)}
	r := h.request("PUT", "/model-settings", encodeKimiDraft(kimiDraft(id)))
	if r.Code != 422 {
		t.Fatalf("expected failed settings write, got %d", r.Code)
	}
	if _, err := os.Stat(filepath.Join(modelconfig.AuthDir(h.settings.store.Path), id+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed settings save retained credentials")
	}
	h.logins.mu.Lock()
	session := h.logins.sessions[id]
	h.logins.mu.Unlock()
	if session == nil || session.credential.AccessToken == "" {
		t.Fatal("failed settings save destroyed the retryable login draft")
	}
}

type failedCredentialSync struct{ *kimi.Store }

func (s failedCredentialSync) Save(id string, credential kimi.Credential) error {
	if err := s.Store.Save(id, credential); err != nil {
		return err
	}
	return errors.New("credential directory sync failed after rename")
}

func TestKimiPartialCredentialWriteRollsBackAndKeepsRetryableDraft(t *testing.T) {
	h := newKimiHarness(t)
	id := h.authorized()
	credentials := &kimi.Store{Dir: modelconfig.AuthDir(h.path)}
	h.settings.credentials = failedCredentialSync{credentials}
	body := encodeKimiDraft(kimiDraft(id))
	if r := h.request("PUT", "/model-settings", body); r.Code != 422 {
		t.Fatal("credential sync failure was accepted")
	}
	if _, err := credentials.Read(id); !errors.Is(err, kimi.ErrLoginRequired) {
		t.Fatal("failed credential save left an unreferenced credential")
	}
	if _, found, err := modelconfig.Read(h.path); err != nil || found {
		t.Fatal("failed credential save persisted settings")
	}
	h.settings.credentials = credentials
	if r := h.request("PUT", "/model-settings", body); r.Code != 200 {
		t.Fatal("credential sync failure destroyed the retryable login draft")
	}
}

func TestKimiLogoutRemainsRevokedAfterSettingsWriteFailure(t *testing.T) {
	h := newKimiHarness(t)
	id := h.authorized()
	if r := h.request("PUT", "/model-settings", encodeKimiDraft(kimiDraft(id))); r.Code != 200 {
		t.Fatal("subscription setup failed")
	}
	// A trailing slash lets Load fall back to defaults but prevents the rename.
	saved, _, err := modelconfig.Read(h.path)
	if err != nil {
		t.Fatal(err)
	}
	h.settings.store.Defaults = saved
	h.settings.store.Path = filepath.Join(t.TempDir(), "unwritable-settings") + string(os.PathSeparator)
	if r := h.request("POST", "/model-settings/kimi/logout", ""); r.Code != 422 {
		t.Fatal("logout did not report settings persistence failure")
	}
	if _, err := h.settings.credentials.Read(id); !errors.Is(err, kimi.ErrLoginRequired) {
		t.Fatal("failed settings write restored revoked credentials")
	}
	h.settings.store.Path = h.path
	if r := h.request("GET", "/model-settings", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"has_token":false`) {
		t.Fatal("stale settings reported a revoked credential as connected")
	}
}

func TestKimiAndCodingPlanNeverReuseAnotherAuthenticationModeKey(t *testing.T) {
	h := newKimiHarness(t)
	setup := `{"auth_mode":"api_key","base_url":"https://open.bigmodel.cn/api/anthropic","token":"general-api-secret","connection_mode":"direct"}`
	if r := h.request("PUT", "/model-settings", setup); r.Code != 200 {
		t.Fatal("API key fixture setup failed")
	}
	for method, route := range map[string]string{"PUT": "/model-settings", "POST": "/model-settings/test"} {
		if r := h.request(method, route, `{"auth_mode":"glm_coding_plan"}`); r.Code != 422 {
			t.Fatal("Coding Plan reused a general API key")
		}
		if r := h.request(method, route, `{"auth_mode":"kimi_oauth","base_url":"https://api.kimi.com/coding","token":"general-api-secret"}`); r.Code != 422 {
			t.Fatal("Kimi subscription accepted an API key")
		}
	}
	if h.models.Load() != 0 || h.polls.Load() != 0 {
		t.Fatal("rejected authentication mode contacted an upstream")
	}
	if r := h.request("PUT", "/model-settings", `{"auth_mode":"glm_coding_plan","token":"coding-plan-secret"}`); r.Code != 200 {
		t.Fatalf("explicit Coding Plan key rejected: %d %s", r.Code, r.Body.String())
	}
	saved, _, err := modelconfig.Read(h.path)
	if err != nil || saved.AuthMode != "glm_coding_plan" || saved.Token != "coding-plan-secret" || saved.CredentialID != "" {
		t.Fatal("Coding Plan save retained another auth mode credential")
	}
}

func TestKimiRepeatedSavePreservesRotatedCredentials(t *testing.T) {
	h := newKimiHarness(t)
	id := h.authorized()
	body := encodeKimiDraft(kimiDraft(id))
	if r := h.request("PUT", "/model-settings", body); r.Code != 200 {
		t.Fatal("initial subscription save failed")
	}
	credentials := &kimi.Store{Dir: modelconfig.AuthDir(h.path)}
	rotated := kimi.Credential{AccessToken: "rotated-access-secret", RefreshToken: "rotated-refresh-secret", ExpiresAt: time.Now().Add(time.Hour)}
	if err := credentials.Save(id, rotated); err != nil {
		t.Fatal(err)
	}
	if r := h.request("PUT", "/model-settings", body); r.Code != 200 || !strings.Contains(r.Body.String(), `"has_token":true`) {
		t.Fatalf("lost-response save retry failed: %d %s", r.Code, r.Body.String())
	}
	after, err := credentials.Read(id)
	if err != nil || after.AccessToken != rotated.AccessToken || after.RefreshToken != rotated.RefreshToken || !after.ExpiresAt.Equal(rotated.ExpiresAt) {
		t.Fatal("save retry overwrote rotated credentials")
	}
	if h.polls.Load() != 1 || h.models.Load() != 0 {
		t.Fatal("save retry replayed authorization or model requests")
	}
}

func TestKimiMissingSavedCredentialRequiresSignInWithoutNetwork(t *testing.T) {
	h := newKimiHarness(t)
	id := h.authorized()
	body := encodeKimiDraft(kimiDraft(id))
	if r := h.request("PUT", "/model-settings", body); r.Code != 200 {
		t.Fatal("initial subscription save failed")
	}
	credentials := &kimi.Store{Dir: modelconfig.AuthDir(h.path)}
	if err := credentials.Delete(id); err != nil {
		t.Fatal(err)
	}
	r := h.request("GET", "/model-settings", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"has_token":false`) || strings.Contains(r.Body.String(), id) {
		t.Fatal("missing credential is still reported as authenticated")
	}
	r = h.request("POST", "/model-settings/test", body)
	if r.Code != 422 || !strings.Contains(r.Body.String(), "sign-in is required") {
		t.Fatalf("missing credential did not prompt sign-in: %d %s", r.Code, r.Body.String())
	}
	if h.polls.Load() != 1 || h.models.Load() != 0 {
		t.Fatal("missing saved credential contacted an upstream")
	}
	if r := h.request("PUT", "/model-settings", body); r.Code != 422 {
		t.Fatal("save retry recreated a revoked credential")
	}
}

func TestKimiLoginCancellationAbortsInflightDraftProbe(t *testing.T) {
	h := newKimiHarness(t)
	id := h.authorized()
	started := make(chan struct{})
	h.client.HTTP.Transport = kimiRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.kimi.com/coding/v1/messages" {
			t.Error("draft probe contacted an unexpected endpoint")
		}
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- modelSettingsRequest(t, h.mux, "POST", "/model-settings/test", encodeKimiDraft(kimiDraft(id)))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("draft probe did not start")
	}
	if r := h.request("DELETE", "/model-settings/kimi/login/"+id, ""); r.Code != 200 {
		t.Fatal("could not cancel login during probe")
	}
	select {
	case r := <-done:
		if r.Code != 200 || !strings.Contains(r.Body.String(), `"ok":false`) || !strings.Contains(r.Body.String(), "cancelled") {
			t.Fatalf("cancelled probe reported incorrect result: %d %s", r.Code, r.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("login cancellation did not abort the draft probe")
	}
}
