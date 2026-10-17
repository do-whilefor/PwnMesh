package dispatcher

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	board "pwnmesh/internal/board/model"
	"pwnmesh/internal/config"
	"pwnmesh/internal/protocol"
)

func TestEnvironmentIdentityTracksToolConfiguration(t *testing.T) {
	s := Scheduler{}
	identity := func(env map[string]string) string {
		return s.environmentID(config.Worker{Type: "go", Env: env})
	}
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "NO_PROXY", "PATH", "LANG", "TOOL_PROFILE"} {
		t.Run(key, func(t *testing.T) {
			first := map[string]string{key: "first"}
			second := map[string]string{key: "second"}
			if identity(first) == identity(second) || identity(first) == identity(nil) {
				t.Fatal("changing or removing a tool setting retained recovery identity")
			}
			if identity(map[string]string{key: ""}) == identity(nil) {
				t.Fatal("explicitly clearing an image default is indistinguishable from inheritance")
			}
		})
	}
	for _, mode := range []string{"direct", "proxy"} {
		env := map[string]string{"PWNMESH_CONNECTION_MODE": mode, "PWNMESH_PROXY_URL": "http://localhost:7897"}
		changed := maps.Clone(env)
		changed["HTTPS_PROXY"], changed["no_proxy"] = "ignored", "ignored"
		if identity(env) != identity(changed) {
			t.Fatal("overridden legacy proxy changed the effective tool identity")
		}
	}
	env := map[string]string{"LANG": "C", "ANTHROPIC_AUTH_TOKEN": "first-credential"}
	changed := maps.Clone(env)
	changed["ANTHROPIC_AUTH_TOKEN"] = "rotated-credential"
	changed["PWNMESH_LAUNCH_TOKEN"] = "launch-only"
	changed["PWNMESH_MODEL_BRIDGE"] = "ignored-override"
	if identity(env) != identity(changed) {
		t.Fatal("provider credentials or reserved launch values changed recovery identity")
	}
}

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
		if env["PWNMESH_CONNECTION_MODE"] == "proxy" {
			c.CommonEnv["PWNMESH_PROXY_URL"] = "http://proxy:7897"
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
			if identity(map[string]string{key: ""}) == identity(nil) {
				t.Fatal("empty tool setting did not record its image override")
			}
		})
	}
}

func TestEnvironmentIdentityTracksActiveModelProtocol(t *testing.T) {
	s := Scheduler{}
	identity := func(env map[string]string) string {
		return s.environmentID(config.Worker{Type: "go", Env: env})
	}
	anthropic := map[string]string{"ANTHROPIC_BASE_URL": "https://model.invalid", "ANTHROPIC_MODEL": "model"}
	explicit := maps.Clone(anthropic)
	explicit["PWNMESH_MODEL_PROTOCOL"] = "anthropic"
	if identity(anthropic) != identity(explicit) {
		t.Fatal("explicit default protocol invalidated the existing Anthropic identity")
	}
	openai := map[string]string{"PWNMESH_MODEL_PROTOCOL": "openai", "OPENAI_BASE_URL": "https://model.invalid", "OPENAI_MODEL": "model", "OPENAI_API_KEY": "first-credential"}
	if identity(anthropic) == identity(openai) || identity(openai) == "" {
		t.Fatal("different model protocols share an identity or OpenAI is unrunnable")
	}
	for _, key := range []string{"OPENAI_BASE_URL", "OPENAI_MODEL"} {
		changed := maps.Clone(openai)
		changed[key] = "replacement"
		if identity(openai) == identity(changed) {
			t.Fatalf("changing %s retained recovery identity", key)
		}
	}
	rotated := maps.Clone(openai)
	rotated["OPENAI_API_KEY"] = "rotated-credential"
	rotated["ANTHROPIC_BASE_URL"], rotated["ANTHROPIC_MODEL"] = "unused-base", "unused-model"
	rotated["ANTHROPIC_AUTH_TOKEN"] = "unused-credential"
	if identity(openai) != identity(rotated) {
		t.Fatal("credentials or inactive provider metadata changed OpenAI identity")
	}
	unused := maps.Clone(anthropic)
	unused["OPENAI_BASE_URL"], unused["OPENAI_MODEL"], unused["OPENAI_API_KEY"] = "unused-base", "unused-model", "unused-credential"
	if identity(anthropic) != identity(unused) {
		t.Fatal("inactive OpenAI metadata changed Anthropic identity")
	}
	for _, invalid := range []string{"unknown", "OpenAI", "openai "} {
		if identity(map[string]string{"PWNMESH_MODEL_PROTOCOL": invalid}) != "" {
			t.Fatal("invalid model protocol received a runnable identity")
		}
	}
}

func legacyDefaultEnvironmentID(s *Scheduler, w config.Worker) string {
	env := map[string]string{}
	for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "PWNMESH_REASONING_EFFORT", "PWNMESH_MAX_OUTPUT_TOKENS", "PWNMESH_CONTEXT_BYTES", "PWNMESH_REQUEST_TIMEOUT"} {
		env[key] = w.Env[key]
	}
	return digest(struct {
		Type    string
		Image   string
		Network string
		Caps    []string
		Env     map[string]string
	}{w.Type, s.Config.Container.Image, s.Config.Container.Network, s.Config.Container.CapAdd, env})
}

func TestEnvironmentIdentityRejectsLegacyEncodingAndInvalidEnvironment(t *testing.T) {
	s := Scheduler{}
	w := config.Worker{Type: "go"}
	if s.environmentID(w) == legacyDefaultEnvironmentID(&s, w) {
		t.Fatal("legacy identity cannot prove which tool settings were used or removed")
	}
	for _, env := range []map[string]string{{"BAD=KEY": "value"}, {"LANG": "C\x00"}, {"PWNMESH_CONNECTION_MODE": "invalid"}, {"PWNMESH_CONNECTION_MODE": "proxy", "PWNMESH_PROXY_URL": "invalid"}} {
		if s.environmentID(config.Worker{Env: env}) != "" {
			t.Fatal("invalid tool environment received a runnable identity")
		}
	}
}

func TestRecoveryRejectsChangedToolEnvironmentAndLegacyIdentity(t *testing.T) {
	for _, change := range []string{"changed", "removed", "legacy", "legacy_result"} {
		t.Run(change, func(t *testing.T) {
			s, runner, store, graph := staleCuratorFixture(t)
			backend := s.Config.Workers[0]
			if change == "changed" || change == "removed" {
				backend.Env = map[string]string{"TOOL_PROFILE": "original"}
			}
			environment := s.environmentID(backend)
			if change == "legacy" || change == "legacy_result" {
				environment = legacyDefaultEnvironmentID(s, backend)
			}
			run := &task{Job: protocol.Job{RunID: "retained-environment", Kind: "reason", Graph: graph, GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", EnvironmentID: environment, Budget: s.Config.Task("reason"), DecisionTrigger: "initial"}, Worker: backend, Lease: Lease{Run: backend.Name + "@retained-environment", Kind: "reason"}}
			ctx := context.Background()
			if err := s.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/reason/claim", map[string]string{"worker": run.Lease.Run, "trigger": "initial"}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := s.register(ctx, run); err != nil {
				t.Fatal(err)
			}
			if change == "changed" {
				s.Config.Workers[0].Env = map[string]string{"TOOL_PROFILE": "replacement"}
			}
			if change == "legacy_result" {
				result := protocol.Result{Status: "success", Text: `{"accepted":false,"reason":"No authorized work"}`, StateVersion: run.Job.Decision.StateVersion}
				if err := s.status(ctx, run, "result_pending", result); err != nil {
					t.Fatal(err)
				}
			}
			recoverPendingResult(t, s, graph.Project)
			saved := curationExecution(t, store, run)
			if runner.calls.Load() != 0 || saved.Resumes != 0 {
				t.Fatal("recovery restarted a worker with an unproven environment")
			}
			if change == "legacy_result" {
				if saved.Status != "rejected" {
					t.Fatalf("legacy saved result did not settle: %s %s", saved.Status, saved.Result)
				}
			} else {
				var failure protocol.Result
				if json.Unmarshal(saved.Result, &failure) != nil || saved.Status != "failed" || failure.FailureKind != "configuration" {
					t.Fatalf("unproven environment was not fenced: %s %s", saved.Status, saved.Result)
				}
			}
		})
	}
}

func TestInvalidToolEnvironmentDoesNotClaimLease(t *testing.T) {
	s, runner, _, graph := staleCuratorFixture(t)
	s.Config.Workers[0].Env = map[string]string{"PWNMESH_CONNECTION_MODE": "invalid"}
	launched, err := s.launch(context.Background(), graph, "reason", nil, "initial", board.ExecutionCheck{})
	if err == nil || launched || runner.calls.Load() != 0 {
		t.Fatalf("invalid environment entered execution: launched=%v error=%v", launched, err)
	}
	if err := s.Client.Do(context.Background(), "GET", projectPath(graph.Project.ID), nil, &graph, nil); err != nil {
		t.Fatal(err)
	}
	if graph.Project.Reason != nil {
		t.Fatal("invalid environment claimed a lease before validation")
	}
}
