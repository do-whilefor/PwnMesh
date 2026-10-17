package modelconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/modelwire"
	"pwnmesh/internal/provider"
)

func TestDefaultsMatchProvider(t *testing.T) {
	settings := Defaults(func(string) string { return "" })
	if settings.BaseURL != provider.DefaultBaseURL || settings.Model != modelwire.DefaultModel {
		t.Fatalf("saved settings and provider defaults differ: base_url=%q model=%q", settings.BaseURL, settings.Model)
	}
	if settings.RequestTimeout != int(provider.DefaultRequestTimeout/time.Second) || settings.MaxTokens != modelwire.DefaultMaxTokens || settings.ReasoningEffort != modelwire.DefaultReasoningEffort {
		t.Fatalf("saved request defaults differ from provider: timeout=%d max_tokens=%d effort=%q", settings.RequestTimeout, settings.MaxTokens, settings.ReasoningEffort)
	}
}

func TestSettingsPersistAndRetainSecretWithoutExposingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "models.json")
	s := &Store{Path: path, Defaults: Defaults(func(string) string { return "" })}
	if _, err := s.Update(func(c *Settings) error { c.Token = "fixture-private-token"; c.Model = "model-a"; return nil }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("settings permissions: %v %v", info, err)
	}
	reopened := &Store{Path: path}
	settings, err := reopened.Update(func(c *Settings) error { c.Token = ""; c.Model = "model-b"; return nil })
	if err != nil || settings.Token != "fixture-private-token" || settings.Model != "model-b" {
		t.Fatalf("secret not retained: model=%s err=%v", settings.Model, err)
	}
	raw, _ := json.Marshal(settings.Public())
	if strings.Contains(string(raw), "fixture-private-token") || strings.Contains(string(raw), `"token":`) || !settings.Public().HasToken {
		t.Fatal("public settings exposed or lost secret")
	}
	if _, err := reopened.Update(func(c *Settings) error { c.ConnectionMode = "invalid"; return nil }); err == nil {
		t.Fatal("invalid settings accepted")
	}
	current, err := reopened.Load()
	if err != nil || current != settings {
		t.Fatal("failed save replaced settings")
	}
}

func TestPublicSettingsHaveAnExplicitCredentialFreeContract(t *testing.T) {
	settings := Defaults(func(string) string { return "" })
	settings.AuthMode, settings.Token = "api_key", "fixture-private-token"
	settings.CredentialID, settings.CredentialDir = testCredentialID, "/private/auth"
	settings.ConnectionMode, settings.ProxyURL = "proxy", "http://localhost:7897"
	raw, err := json.Marshal(settings.Public())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{"auth_mode", "protocol", "base_url", "model", "connection_mode", "proxy_url", "max_tokens", "request_timeout", "reasoning_effort", "has_token"}
	if len(fields) != len(want) {
		t.Fatalf("unexpected public fields: %s", raw)
	}
	for _, key := range want {
		if _, ok := fields[key]; !ok {
			t.Errorf("public settings omitted %s", key)
		}
	}
	if settings.Public().ProxyURL != settings.ProxyURL || !settings.Public().HasToken {
		t.Fatal("public settings changed the configured connection")
	}
}

func TestOverlayPreservesExistingTaskEnvironment(t *testing.T) {
	env := map[string]string{"ANTHROPIC_AUTH_TOKEN": "before", "CUSTOM_TOOL": "kept"}
	settings := Defaults(func(string) string { return "" })
	settings.Token = "after"
	settings.ConnectionMode = "proxy"
	next := settings.Overlay(env)
	if env["ANTHROPIC_AUTH_TOKEN"] != "before" || next["ANTHROPIC_AUTH_TOKEN"] != "after" || next["CUSTOM_TOOL"] != "kept" || next["PWNMESH_CONNECTION_MODE"] != "proxy" {
		t.Fatal("environment overlay was not isolated")
	}
}

func TestOpenAISettingsPersistAndSelectOnlyActiveCredentials(t *testing.T) {
	env := map[string]string{"PWNMESH_MODEL_PROTOCOL": "openai", "OPENAI_MODEL": "fixture-model", "OPENAI_API_KEY": "openai-fixture", "ANTHROPIC_AUTH_TOKEN": "old-fixture", "TOOL_SETTING": "kept"}
	settings := Defaults(func(key string) string { return env[key] })
	if settings.Protocol != "openai" || settings.BaseURL != provider.DefaultOpenAIBaseURL || settings.Model != "fixture-model" || settings.Token != "openai-fixture" {
		t.Fatal("OpenAI defaults inherited inactive provider settings")
	}
	store := Store{Path: filepath.Join(t.TempDir(), "models.json"), Defaults: settings}
	if _, err := store.Update(func(s *Settings) error { s.Token = "openai-fixture"; return nil }); err != nil {
		t.Fatal(err)
	}
	saved, found, err := Read(store.Path)
	if err != nil || !found || saved != settings {
		t.Fatal("OpenAI settings did not round trip", err)
	}
	next := saved.Overlay(env)
	if next["PWNMESH_MODEL_PROTOCOL"] != "openai" || next["OPENAI_API_KEY"] != "openai-fixture" || next["ANTHROPIC_AUTH_TOKEN"] != "" || next["TOOL_SETTING"] != "kept" || env["ANTHROPIC_AUTH_TOKEN"] != "old-fixture" {
		t.Fatal("OpenAI overlay retained stale credentials or mutated a running task")
	}
	saved.Protocol = "anthropic"
	next = saved.Overlay(next)
	if next["PWNMESH_MODEL_PROTOCOL"] != "anthropic" || next["OPENAI_API_KEY"] != "" || next["ANTHROPIC_AUTH_TOKEN"] != saved.Token {
		t.Fatal("switching back to Anthropic retained OpenAI credentials")
	}
}
