package provider

import (
	"testing"
	"time"
)

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
