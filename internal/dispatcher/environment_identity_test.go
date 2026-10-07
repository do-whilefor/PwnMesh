package dispatcher

import (
	"testing"

	"pwnmesh/internal/config"
)

func TestEnvironmentIdentityTracksLegacySettings(t *testing.T) {
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
	} {
		t.Run(tc.suffix, func(t *testing.T) {
			legacy, canonical := "XLOOM_"+tc.suffix, "PWNMESH_"+tc.suffix
			oldID := identity(map[string]string{legacy: tc.first})
			if oldID != identity(map[string]string{canonical: tc.first}) {
				t.Fatal("equivalent old and new settings have different identities")
			}
			if oldID == identity(map[string]string{legacy: tc.second}) {
				t.Fatal("changed legacy setting did not change execution identity")
			}
			if oldID != identity(map[string]string{canonical: tc.first, legacy: tc.second}) {
				t.Fatal("overridden legacy setting changed execution identity")
			}
			if identity(map[string]string{canonical: "", legacy: tc.first}) != identity(map[string]string{canonical: ""}) {
				t.Fatal("explicitly empty canonical setting did not suppress legacy identity")
			}
		})
	}
}
