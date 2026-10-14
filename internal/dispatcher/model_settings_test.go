package dispatcher

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pwnmesh/internal/config"
	"pwnmesh/internal/modelconfig"
	"pwnmesh/internal/protocol"
)

func TestSettingsReloadReleasesHealthGateAndKeepsRunningEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	store := &modelconfig.Store{Path: path, Defaults: modelconfig.Defaults(func(string) string { return "" })}
	old := map[string]string{"ANTHROPIC_MODEL": "old", "ANTHROPIC_AUTH_TOKEN": "old-secret", "CUSTOM_TOOL": "value"}
	s := &Scheduler{Config: config.Config{ModelSettingsPath: path, Workers: []config.Worker{{Name: "worker", Env: old}}}, incompatible: map[string]string{"worker": "configuration"}, unhealthy: map[string]time.Time{"worker": time.Now()}, rejected: map[string]time.Time{"worker": time.Now()}}
	if _, err := store.Update(func(c *modelconfig.Settings) error {
		c.Model = "replacement"
		c.Token = "replacement-secret"
		c.ConnectionMode = "proxy"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshModelSettings(); err != nil {
		t.Fatal(err)
	}
	if s.Config.Workers[0].Env["ANTHROPIC_MODEL"] != "replacement" || old["ANTHROPIC_MODEL"] != "old" || len(s.incompatible)+len(s.unhealthy)+len(s.rejected) != 0 {
		t.Fatal("new settings failed to recover backend or mutated running task")
	}
	s.incompatible["worker"] = "configuration"
	if err := s.refreshModelSettings(); err != nil {
		t.Fatal(err)
	}
	if len(s.incompatible) != 1 {
		t.Fatal("unchanged settings erased health rejection")
	}
	if _, err := store.Update(func(c *modelconfig.Settings) error { c.Token = "rotated-secret"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshModelSettings(); err != nil || len(s.incompatible) != 0 {
		t.Fatal("token rotation did not release stale health rejection")
	}
}

type settingsCleanupRunner struct {
	batchProtocolRunner
	project string
	cleaned chan string
}

func (r *settingsCleanupRunner) Projects(context.Context) ([]string, error) {
	return []string{r.project}, nil
}
func (r *settingsCleanupRunner) Cleanup(_ context.Context, _ string, state string) error {
	r.cleaned <- state
	return nil
}

func TestInvalidModelSettingsDoNotBlockProjectStopAndContainerCleanup(t *testing.T) {
	s, _, _, graph, _ := batchSchedulerFixture(t, 1)
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	s.Config.ModelSettingsPath = path
	runner := &settingsCleanupRunner{project: graph.Project.ID, cleaned: make(chan string, 1)}
	s.Runner = runner
	cancelled := false
	s.running["fixture"] = &task{Job: protocol.Job{Graph: graph}, Cancel: func() { cancelled = true }}
	if err := s.Client.Do(context.Background(), "PUT", projectPath(graph.Project.ID)+"/status", map[string]string{"status": "stopped"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Step(context.Background()); err == nil {
		t.Fatal("invalid model settings were ignored")
	}
	if !cancelled {
		t.Fatal("model configuration failure blocked cancellation")
	}
	select {
	case state := <-runner.cleaned:
		if state != "stopped" {
			t.Fatalf("wrong cleanup state: %s", state)
		}
	case <-time.After(time.Second):
		t.Fatal("model configuration failure blocked container cleanup")
	}
	s.wg.Wait()
}

func TestInitialReloadDoesNotEraseStartupReadinessFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	store := &modelconfig.Store{Path: path, Defaults: modelconfig.Defaults(func(string) string { return "" })}
	if _, err := store.Update(func(c *modelconfig.Settings) error { c.Token = "invalid-fixture"; return nil }); err != nil {
		t.Fatal(err)
	}
	s := New(config.Config{ModelSettingsPath: path, Runtime: config.Runtime{HealthMode: "startup_only"}, Workers: []config.Worker{{Name: "worker"}}}, nil)
	s.CheckHealth = func(context.Context, config.Worker) error {
		return &healthFailure{Kind: "configuration", Err: errors.New("rejected")}
	}
	if err := s.Health(context.Background(), false); err == nil {
		t.Fatal("expected failed startup health")
	}
	if err := s.refreshModelSettings(); err != nil {
		t.Fatal(err)
	}
	if s.incompatible["worker"] != "configuration" {
		t.Fatal("first scheduling tick erased startup readiness rejection")
	}
}

func TestOldLaunchHealthDoesNotRejectRotatedCredentials(t *testing.T) {
	s := New(config.Config{ModelSettingsPath: "configured", Runtime: config.Runtime{MaxWorkers: 1}, Workers: []config.Worker{{Name: "worker", Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "new"}}}}, nil)
	old := &task{Worker: config.Worker{Name: "worker", Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "old"}}}
	s.done <- finished{Task: old, Outcome: "unhealthy", Err: &healthFailure{Kind: "configuration", Err: errors.New("old credentials rejected")}}
	s.reap()
	if len(s.incompatible) != 0 {
		t.Fatal("late old launch rejected new credentials")
	}
	s.done <- finished{Task: &task{Worker: s.Config.Workers[0]}, Outcome: "unhealthy", Err: &healthFailure{Kind: "configuration", Err: errors.New("new credentials rejected")}}
	s.reap()
	if s.incompatible["worker"] != "configuration" {
		t.Fatal("current launch rejection was discarded")
	}
}

func TestSettingsRemovalRestoresConfiguredEnvironment(t *testing.T) {
	for _, scenario := range []string{"hot_reload", "saved_at_startup", "removed_before_first_refresh"} {
		t.Run(scenario, func(t *testing.T) {
			savedAtStartup := scenario != "hot_reload"
			path := filepath.Join(t.TempDir(), "settings.json")
			t.Setenv(modelconfig.PathEnv, path)
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "environment-token")
			t.Setenv("ANTHROPIC_BASE_URL", "https://environment.invalid")
			t.Setenv("ANTHROPIC_DEFAULT_FABLE_MODEL", "environment-model")
			store := modelconfig.Store{Path: path, Defaults: modelconfig.Defaults(func(string) string { return "" })}
			save := func() {
				t.Helper()
				if _, err := store.Update(func(settings *modelconfig.Settings) error {
					settings.Model, settings.Token, settings.ConnectionMode = "saved-model", "saved-token", "proxy"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if savedAtStartup {
				save()
			}
			cfg, err := config.Load(filepath.Join("..", "..", "dispatch.example.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			s := New(cfg, nil)
			if !savedAtStartup {
				save()
			}
			if scenario != "removed_before_first_refresh" {
				if err := s.refreshModelSettings(); err != nil {
					t.Fatal(err)
				}
			}
			running := s.Config.Workers[0]
			if running.Env["ANTHROPIC_MODEL"] != "saved-model" {
				t.Fatal("saved model was not applied")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			fallback, err := config.Load(filepath.Join("..", "..", "dispatch.example.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			s.incompatible = map[string]string{"general": "configuration"}
			if err := s.refreshModelSettings(); err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(s.Config.Workers[0].Env, fallback.Workers[0].Env) || len(s.incompatible) != 0 {
				t.Fatal("removed settings did not restore the configured environment and release its health gate")
			}
			if running.Env["ANTHROPIC_MODEL"] != "saved-model" || running.Env["ANTHROPIC_AUTH_TOKEN"] != "saved-token" || s.currentModelSettings(running) {
				t.Fatal("fallback changed an existing launch or accepted its stale health result")
			}
			s.incompatible["general"] = "configuration"
			if err := s.refreshModelSettings(); err != nil || len(s.incompatible) != 1 {
				t.Fatal("unchanged fallback erased a readiness rejection")
			}
			save()
			if err := s.refreshModelSettings(); err != nil || s.Config.Workers[0].Env["ANTHROPIC_MODEL"] != "saved-model" || len(s.incompatible) != 0 {
				t.Fatal("recreated settings were not applied")
			}
		})
	}
}
