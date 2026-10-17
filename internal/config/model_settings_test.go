package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"pwnmesh/internal/modelconfig"
	"pwnmesh/internal/provider"
)

func TestDispatcherCanStartBeforeWebModelSetupAndReadSavedSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv(modelconfig.PathEnv, path)
	for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		t.Setenv(key, "")
	}
	file := filepath.Join("..", "..", "dispatch.example.yaml")
	initial, err := Load(file)
	if err != nil || initial.ModelSettingsPath != path {
		t.Fatalf("unconfigured Web install cannot start: %v", err)
	}
	store := modelconfig.Store{Path: path, Defaults: modelconfig.Defaults(func(string) string { return "" })}
	if _, err := store.Update(func(c *modelconfig.Settings) error {
		c.Token = "saved-fixture"
		c.Model = "saved-model"
		c.ConnectionMode = "proxy"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range loaded.Workers {
		if w.Env["ANTHROPIC_AUTH_TOKEN"] != "saved-fixture" || w.Env["ANTHROPIC_MODEL"] != "saved-model" || w.Env["PWNMESH_CONNECTION_MODE"] != "proxy" {
			t.Fatal("persisted Web config not used by dispatcher")
		}
		fallback := w.WithModelSettings(nil)
		if fallback.Env["ANTHROPIC_AUTH_TOKEN"] != "" || fallback.Env["ANTHROPIC_MODEL"] != "" || fallback.Env["PWNMESH_CONNECTION_MODE"] != "" || w.Env["ANTHROPIC_MODEL"] != "saved-model" {
			t.Fatal("saved settings destroyed the unconfigured baseline or changed an existing worker")
		}
	}
}

func TestDispatcherLoadsSavedOpenAISettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv(modelconfig.PathEnv, path)
	store := modelconfig.Store{Path: path, Defaults: modelconfig.Defaults(func(string) string { return "" })}
	if _, err := store.Update(func(s *modelconfig.Settings) error {
		s.Protocol, s.BaseURL, s.Model, s.Token = "openai", "https://model.invalid/v1", "openai-model", "openai-fixture"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(filepath.Join("..", "..", "dispatch.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, worker := range loaded.Workers {
		p, err := provider.FromEnvironment(context.Background(), worker.Getenv, "")
		if err != nil || p.Protocol != "openai" || p.BaseURL != "https://model.invalid/v1" || p.Model != "openai-model" || p.Token != "openai-fixture" {
			t.Fatal("saved OpenAI configuration did not reach the dispatcher provider", err)
		}
	}
}

func TestDispatcherReadsDefaultNativeModelSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(modelconfig.PathEnv, "")
	path := filepath.Join(os.Getenv("HOME"), ".local", "share", "pwnmesh", "model-settings.json")
	store := modelconfig.Store{Path: path, Defaults: modelconfig.Defaults(func(string) string { return "" })}
	if _, err := store.Update(func(settings *modelconfig.Settings) error {
		settings.Token, settings.Model = "default-fixture", "default-saved-model"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(filepath.Join("..", "..", "dispatch.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ModelSettingsPath != path {
		t.Fatalf("model path = %q, want %q", loaded.ModelSettingsPath, path)
	}
	for _, worker := range loaded.Workers {
		if worker.Env["ANTHROPIC_AUTH_TOKEN"] != "default-fixture" || worker.Env["ANTHROPIC_MODEL"] != "default-saved-model" {
			t.Fatal("native dispatcher ignored the model settings saved by serve")
		}
	}
}
