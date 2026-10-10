package modelconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
