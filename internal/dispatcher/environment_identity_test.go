package dispatcher

import (
	"testing"

	"pwnmesh/internal/config"
)

func TestEnvironmentIdentityTracksPwnMeshSettings(t *testing.T) {
	identity := func(env map[string]string) string {
		t.Helper()
		c := config.Config{
			Server:    "http://server:8000",
			Runtime:   config.Runtime{Interval: 2, MaxWorkers: 4, MaxProjects: 2, MaxProjectWorkers: 2, HealthTimeout: 30},
			Tasks:     config.Tasks{Explore: config.Task{ConcludeTimeout: 60}},
			Container: config.Container{Image: "fixture", Network: "bridge", CompletedAction: "stop"},
			CommonEnv: map[string]string{"ANTHROPIC_BASE_URL": "http://unused.invalid", "ANTHROPIC_AUTH_TOKEN": "fixture", "ANTHROPIC_MODEL": "fixture"},
			Workers:   []config.Worker{{Name: "fixture", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: 1, Env: env}},
		}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		s := Scheduler{Config: c}
		return s.environmentID(c.Workers[0])
	}
	for _, tc := range []struct{ suffix, first, second string }{
		{"REASONING_EFFORT", "high", "max"},
		{"MAX_OUTPUT_TOKENS", "100", "200"},
		{"CONTEXT_BYTES", "100", "200"},
		{"REQUEST_TIMEOUT", "100", "200"},
		{"CONTEXT_TOKENS", "100", "200"},
		{"CONTEXT_TARGET_TOKENS", "100", "200"},
		{"CONNECTION_MODE", "direct", "proxy"},
		{"PROXY_URL", "http://proxy:7897", "http://proxy:7898"},
	} {
		t.Run(tc.suffix, func(t *testing.T) {
			key := "PWNMESH_" + tc.suffix
			if identity(map[string]string{key: tc.first}) == identity(map[string]string{key: tc.second}) {
				t.Fatal("changed setting did not change execution identity")
			}
			if identity(map[string]string{key: ""}) != identity(nil) {
				t.Fatal("empty setting changed execution identity")
			}
		})
	}
}

func TestEnvironmentIdentityPinsDefaultSettingsEncoding(t *testing.T) {
	w := config.Worker{Type: "go", Env: map[string]string{"ANTHROPIC_BASE_URL": "https://model.invalid", "ANTHROPIC_MODEL": "fixture"}}
	s := Scheduler{Config: config.Config{Container: config.Container{Image: "fixture", Network: "bridge"}}}
	env := map[string]string{}
	for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "PWNMESH_REASONING_EFFORT", "PWNMESH_MAX_OUTPUT_TOKENS", "PWNMESH_CONTEXT_BYTES", "PWNMESH_REQUEST_TIMEOUT"} {
		env[key] = w.Env[key]
	}
	expected := digest(struct {
		Type    string
		Image   string
		Network string
		Caps    []string
		Env     map[string]string
	}{"go", "fixture", "bridge", nil, env})
	if s.environmentID(w) != expected {
		t.Fatal("default execution environment identity encoding changed")
	}
	w.Env["PWNMESH_CONNECTION_MODE"] = ""
	w.Env["PWNMESH_PROXY_URL"] = ""
	if s.environmentID(w) != expected {
		t.Fatal("empty proxy settings changed execution identity")
	}
}
