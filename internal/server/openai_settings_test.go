package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"pwnmesh/internal/modelconfig"
)

func TestOpenAISettingsProbeSaveAndReload(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer openai-fixture" || r.Header.Get("x-api-key") != "" || r.Header.Get("anthropic-version") != "" {
			t.Error("probe used the wrong protocol or credentials")
		}
		var input struct {
			Model    string `json:"model"`
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		if input.Model != "openai-model" {
			t.Error("probe lost its selected model")
		}
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			if len(input.Tools) != 1 || input.Tools[0].Function.Name != "readiness_check" {
				t.Error("probe omitted OpenAI function schema")
			}
			_, _ = w.Write([]byte(`{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"check-1","type":"function","function":{"name":"readiness_check","arguments":"{\"ready\":true}"}}]}}]}`))
			return
		}
		if len(input.Messages) != 4 || input.Messages[3].Role != "tool" || input.Messages[3].ToolCallID != "check-1" || len(input.Tools) != 0 {
			t.Error("probe omitted matching OpenAI tool receipt")
			return
		}
		var challenge map[string]string
		receipt := input.Messages[3].Content
		if json.Unmarshal([]byte(receipt), &challenge) != nil || len(challenge["ready"]) != 32 {
			t.Error("probe did not challenge tool-result consumption")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]string{"role": "assistant", "content": receipt}}}})
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "settings.json")
	handler := NewWithModelSettings(nil, path)
	body, _ := json.Marshal(map[string]any{"protocol": "openai", "base_url": upstream.URL + "/v1", "model": "openai-model", "token": "openai-fixture", "connection_mode": "direct"})
	response := modelSettingsRequest(t, handler, "POST", "/model-settings/test", string(body))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"ok":true`) || !strings.Contains(response.Body.String(), "OpenAI Chat Completions") || calls != 2 {
		t.Fatalf("OpenAI probe failed: %d %s, calls=%d", response.Code, response.Body.String(), calls)
	}
	if _, found, err := modelconfig.Read(path); err != nil || found {
		t.Fatal("probe saved the draft")
	}
	response = modelSettingsRequest(t, handler, "PUT", "/model-settings", string(body))
	if response.Code != 200 || strings.Contains(response.Body.String(), "openai-fixture") {
		t.Fatalf("OpenAI save failed or exposed token: %d", response.Code)
	}
	response = modelSettingsRequest(t, NewWithModelSettings(nil, path), "GET", "/model-settings", "")
	var saved modelconfig.Public
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &saved) != nil || saved.Protocol != "openai" || !saved.HasToken || saved.Token != "" {
		t.Fatal("OpenAI settings did not survive server restart")
	}
}
