package config

import (
	"path/filepath"
	"testing"

	"pwnmesh/internal/modelconfig"
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
	}
}
