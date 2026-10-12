//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pwnmesh/internal/modelconfig"
)

func TestNativeModelSettingsPathIsSharedAndPreservesLegacyFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(modelconfig.PathEnv, "")
	want, err := modelconfig.ResolvePath()
	if err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(t.TempDir(), "custom.db")
	if got, err := serveModelSettingsPath(database); err != nil || got != want {
		t.Fatalf("custom database changed shared model settings: %q %v", got, err)
	}
	legacy := filepath.Join(filepath.Dir(database), "model-settings.json")
	if err := os.WriteFile(legacy, []byte("retained-legacy-settings"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := serveModelSettingsPath(database); err == nil || !strings.Contains(err.Error(), modelconfig.PathEnv) {
		t.Fatal("server silently ignored existing database-adjacent model settings")
	}
	t.Setenv(modelconfig.PathEnv, legacy)
	if got, err := serveModelSettingsPath(database); err != nil || got != legacy {
		t.Fatalf("explicit path was not preserved: %q %v", got, err)
	}
	if raw, err := os.ReadFile(legacy); err != nil || string(raw) != "retained-legacy-settings" {
		t.Fatal("legacy settings were changed")
	}
}

func TestServeTrustedHostFlags(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, args := range [][]string{
		nil,
		{"--host", ""},
		{"--host", "0.0.0.0"},
		{"--host", "::1"},
		{"--host", "pwn.example.com"},
		{"--allow-host", "server", "--allow-host", "pwn.example.com, other.example.com"},
	} {
		if err := serve(ctx, args, io.Discard); !errors.Is(err, context.Canceled) {
			t.Errorf("valid flags %v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"--allow-host", ""},
		{"--allow-host", "server,"},
		{"--allow-host", "server:8000"},
		{"--allow-host", "*.example.com"},
		{"--allow-host", "https://pwn.example.com"},
		{"--host", "localhost:8000"},
	} {
		if err := serve(ctx, args, io.Discard); err == nil || !strings.Contains(err.Error(), "invalid trusted host") {
			t.Errorf("invalid flags %v: %v", args, err)
		}
	}
}
