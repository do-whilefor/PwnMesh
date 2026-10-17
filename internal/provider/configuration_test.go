package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"pwnmesh/internal/agent"
)

func TestFromEnvironmentUsesDeepSeekDefaults(t *testing.T) {
	p, err := FromEnvironment(func(key string) string {
		if key == "ANTHROPIC_AUTH_TOKEN" {
			return "fixture"
		}
		return ""
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.URL.String(); got != "https://api.deepseek.com/anthropic/v1/messages" {
			t.Errorf("default request endpoint = %q", got)
		}
		var payload struct{ Model string }
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "deepseek-flash" {
			t.Errorf("default request model = %q", payload.Model)
		}
		return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`), nil
	})}
	if _, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Reply OK")}, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestFromEnvironmentMatchesRoleAndModelSettings(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "fixture", "ANTHROPIC_BASE_URL": "https://openrouter.ai/api", "ANTHROPIC_DEFAULT_FABLE_MODEL": "fixture-model",
		"PWNMESH_REASONING_EFFORT": "max", "PWNMESH_MAX_OUTPUT_TOKENS": "384000", "PWNMESH_REQUEST_TIMEOUT": "45",
	}
	getenv := func(key string) string { return env[key] }
	p, err := FromEnvironment(getenv, "low")
	if err != nil || p.Model != "fixture-model" || p.BaseURL != env["ANTHROPIC_BASE_URL"] || p.ReasoningEffort != "low" || p.MaxTokens != 384000 || p.Timeout != 45*time.Second {
		t.Fatalf("role settings changed: provider=%+v err=%v", p, err)
	}
	if env["PWNMESH_REASONING_EFFORT"] != "max" {
		t.Fatal("role override changed other roles")
	}
	env["ANTHROPIC_MODEL"] = "preferred-model"
	p, err = FromEnvironment(getenv, "")
	if err != nil || p.Model != "preferred-model" || p.ReasoningEffort != "max" {
		t.Fatalf("backend inheritance changed: provider=%+v err=%v", p, err)
	}
	delete(env, "PWNMESH_REASONING_EFFORT")
	delete(env, "PWNMESH_MAX_OUTPUT_TOKENS")
	delete(env, "PWNMESH_REQUEST_TIMEOUT")
	p, err = FromEnvironment(getenv, "")
	if err != nil || p.ReasoningEffort != DefaultReasoningEffort || p.MaxTokens != DefaultMaxTokens || p.Timeout != 180*time.Second {
		t.Fatalf("defaults changed: provider=%+v err=%v", p, err)
	}
}

func TestFromEnvironmentRejectsInvalidExplicitSettings(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"PWNMESH_REASONING_EFFORT", "medium"}, {"PWNMESH_MAX_OUTPUT_TOKENS", "0"}, {"PWNMESH_MAX_OUTPUT_TOKENS", "garbage"},
		{"PWNMESH_REQUEST_TIMEOUT", "-1"}, {"PWNMESH_REQUEST_TIMEOUT", "9223372036854775807"}, {"ANTHROPIC_AUTH_TOKEN", " "},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			env := map[string]string{"ANTHROPIC_AUTH_TOKEN": "fixture", tc.key: tc.value}
			if _, err := FromEnvironment(func(key string) string { return env[key] }, ""); err == nil {
				t.Fatal("invalid explicit setting silently fell back")
			}
		})
	}
}

func TestFromEnvironmentSelectsOpenAIWithoutAnthropicFallback(t *testing.T) {
	env := map[string]string{
		"PWNMESH_MODEL_PROTOCOL": "openai", "OPENAI_API_KEY": "openai-fixture", "OPENAI_MODEL": "openai-model",
		"ANTHROPIC_AUTH_TOKEN": "unused-fixture", "ANTHROPIC_MODEL": "unused-model", "ANTHROPIC_BASE_URL": "https://unused.invalid",
		"ANTHROPIC_AUTH_MODE": "kimi_oauth", "ANTHROPIC_CREDENTIAL_ID": "unused-reference", "ANTHROPIC_CREDENTIAL_DIR": "/unused-store",
	}
	getenv := func(key string) string { return env[key] }
	p, err := FromEnvironment(getenv, "")
	if err != nil || p.Protocol != "openai" || p.AuthMode != "" || p.TokenSource != nil || p.BaseURL != DefaultOpenAIBaseURL || p.Token != env["OPENAI_API_KEY"] || p.Model != "openai-model" {
		t.Fatal("OpenAI did not use isolated protocol settings", err)
	}
	env["OPENAI_BASE_URL"] = "https://gateway.invalid/v1"
	p, err = FromEnvironment(getenv, "low")
	if err != nil || p.BaseURL != env["OPENAI_BASE_URL"] || p.ReasoningEffort != "low" {
		t.Fatal("OpenAI lost its endpoint or persisted policy", err)
	}
	for _, key := range []string{"OPENAI_MODEL", "OPENAI_API_KEY"} {
		value := env[key]
		delete(env, key)
		if _, err := FromEnvironment(getenv, ""); err == nil {
			t.Fatalf("missing %s fell back to Anthropic", key)
		}
		env[key] = value
	}
	env["PWNMESH_MODEL_PROTOCOL"] = "unknown"
	if _, err := FromEnvironment(getenv, ""); err == nil {
		t.Fatal("unknown model protocol accepted")
	}
}
