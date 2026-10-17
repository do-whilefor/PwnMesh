package config

import (
	"maps"
	"os"
	"strings"
	"testing"
)

func TestToolEnvironmentPreservesOverridesAndOwnsItsValues(t *testing.T) {
	input := map[string]string{"LANG": "", "PATH": "/tools", "HTTPS_PROXY": "http://legacy:8080", "ANTHROPIC_AUTH_TOKEN": "credential", "anthropic_custom": "private", "OPENAI_API_KEY": "credential", "openai_custom": "private", "OPENAI_BASE_URL": "https://model.invalid", "OPENAI_MODEL": "model", "PWNMESH_MODEL_PROTOCOL": "openai", "PWNMESH_LAUNCH_TOKEN": "launch", "PWNMESH_MODEL_BRIDGE": "override"}
	before := maps.Clone(input)
	env, err := (Worker{Env: input}).ToolEnvironment()
	if err != nil || len(env) != 3 || env["PATH"] != "/tools" || env["HTTPS_PROXY"] != "http://legacy:8080" {
		t.Fatalf("tool environment lost configured values or leaked reserved values: %v", err)
	}
	if value, exists := env["LANG"]; !exists || value != "" {
		t.Fatal("empty override was discarded")
	}
	env["PATH"] = "changed"
	if !maps.Equal(input, before) {
		t.Fatal("derived tool environment changed the resolved worker settings")
	}
	for _, input := range []map[string]string{{"": "value"}, {"BAD=KEY": "value"}, {"LANG": "C\x00"}, {"PWNMESH_CONNECTION_MODE": "unknown"}, {"PWNMESH_CONNECTION_MODE": "proxy", "PWNMESH_PROXY_URL": "invalid"}} {
		if _, err := (Worker{Env: input}).ToolEnvironment(); err == nil {
			t.Fatal("invalid tool environment was accepted")
		}
	}
}

func TestWorkerGetenvUsesExactSettingNames(t *testing.T) {
	for _, tc := range []struct {
		name, key, legacyKey, value, legacyValue, want string
		setValue, setLegacy                            bool
	}{
		{name: "missing", key: "PWNMESH_TEST_SETTING", legacyKey: "XLOOM_TEST_SETTING"},
		{name: "obsolete name is not a fallback", key: "PWNMESH_TEST_SETTING", legacyKey: "XLOOM_TEST_SETTING", setLegacy: true, legacyValue: "old"},
		{name: "canonical", key: "PWNMESH_TEST_SETTING", legacyKey: "XLOOM_TEST_SETTING", setValue: true, value: "new", want: "new"},
		{name: "canonical wins", key: "PWNMESH_TEST_SETTING", legacyKey: "XLOOM_TEST_SETTING", setValue: true, value: "new", setLegacy: true, legacyValue: "old", want: "new"},
		{name: "empty canonical wins", key: "PWNMESH_TEST_SETTING", legacyKey: "XLOOM_TEST_SETTING", setValue: true, setLegacy: true, legacyValue: "old"},
		{name: "unrelated value", key: "CONFIG_TEST_SETTING", legacyKey: "XLOOM_CONFIG_TEST_SETTING", setValue: true, value: "direct", setLegacy: true, legacyValue: "old", want: "direct"},
		{name: "unrelated missing", key: "CONFIG_TEST_SETTING", legacyKey: "XLOOM_CONFIG_TEST_SETTING", setLegacy: true, legacyValue: "old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			if tc.setValue {
				env[tc.key] = tc.value
			}
			if tc.setLegacy {
				env[tc.legacyKey] = tc.legacyValue
			}
			before := maps.Clone(env)
			if got := (Worker{Env: env}).Getenv(tc.key); got != tc.want {
				t.Errorf("worker Getenv returned %q, want %q", got, tc.want)
			}
			if !maps.Equal(env, before) {
				t.Fatal("worker lookup changed its configured environment")
			}
		})
	}
}

func TestWorkerGetenvDoesNotInheritProcessSettings(t *testing.T) {
	for _, key := range []string{"PWNMESH_CONTEXT_BYTES", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "PWNMESH_MODEL_PROTOCOL"} {
		t.Setenv(key, "process-only")
		if got := (Worker{}).Getenv(key); got != "" {
			t.Fatalf("worker inherited process setting %s", key)
		}
	}
	worker := Worker{Env: map[string]string{"PWNMESH_CONTEXT_BYTES": "worker-only"}}
	if got := worker.Getenv("PWNMESH_CONTEXT_BYTES"); got != "worker-only" {
		t.Fatalf("worker setting = %q, want its own value", got)
	}
}

func TestConfigRejectsMissingEnvironmentAlongsideOptionalModelVariables(t *testing.T) {
	const required = "PWNMESH_CONFIG_TEST_REQUIRED"
	optionalKeys := []string{"ANTHROPIC_CONFIG_TEST_OPTIONAL", "OPENAI_CONFIG_TEST_OPTIONAL", "PWNMESH_MODEL_PROTOCOL"}
	for _, key := range append([]string{required}, optionalKeys...) {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	for _, optional := range optionalKeys {
		for _, value := range []string{
			"${" + required + "}${" + optional + "}",
			"${" + optional + "}${" + required + "}",
		} {
			t.Run(value, func(t *testing.T) {
				c := coldStartConfig()
				c.CommonEnv["CONFIG_TEST_SETTING"] = value
				if err := c.Validate(); err == nil || !strings.Contains(err.Error(), required) {
					t.Fatalf("missing required variable was masked by an optional model variable: %v", err)
				}
			})
		}
		if env, err := (Worker{}).resolveEnv(map[string]string{optional: "${" + optional + "}"}); err != nil || env[optional] != "" {
			t.Fatalf("missing optional provider setting %s did not resolve empty: %v", optional, err)
		}
	}
}

func TestConfigResolvesWorkerEnvironment(t *testing.T) {
	const canonical = "PWNMESH_CONTEXT_BYTES"
	for _, tc := range []struct {
		name           string
		common, worker map[string]string
		want           string
	}{
		{name: "common", common: map[string]string{canonical: "100"}, want: "100"},
		{name: "worker overrides common", common: map[string]string{canonical: "100"}, worker: map[string]string{canonical: "200"}, want: "200"},
		{name: "empty worker overrides common", common: map[string]string{canonical: "100"}, worker: map[string]string{canonical: ""}},
		{name: "expanded value", common: map[string]string{canonical: "${PWNMESH_CONFIG_TEST_VALUE}"}, want: "300"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PWNMESH_CONFIG_TEST_VALUE", "300")
			c := coldStartConfig()
			for key, value := range tc.common {
				c.CommonEnv[key] = value
			}
			c.Workers[0].Env = tc.worker
			commonBefore, workerBefore := maps.Clone(c.CommonEnv), maps.Clone(tc.worker)
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			if got, exists := c.Workers[0].Env[canonical]; !exists || got != tc.want {
				t.Fatalf("canonical setting = %q, exists = %v; want %q", got, exists, tc.want)
			}
			if !maps.Equal(c.CommonEnv, commonBefore) || !maps.Equal(tc.worker, workerBefore) {
				t.Fatal("resolution mutated an input environment")
			}
		})
	}
}

func TestConfigRejectsObsoleteEnvironmentNames(t *testing.T) {
	for _, target := range []string{"common", "worker"} {
		t.Run(target, func(t *testing.T) {
			c := coldStartConfig()
			if target == "common" {
				c.CommonEnv["XLOOM_CONTEXT_BYTES"] = "100"
			} else {
				c.Workers[0].Env = map[string]string{"XLOOM_CONTEXT_BYTES": "100"}
			}
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "use PWNMESH_CONTEXT_BYTES") {
				t.Fatalf("obsolete setting was not rejected with its replacement: %v", err)
			}
		})
	}
}

func TestConfigKeepsProviderModelFallback(t *testing.T) {
	for _, model := range []string{"", "explicit"} {
		t.Run(model, func(t *testing.T) {
			env, err := (Worker{Env: map[string]string{"ANTHROPIC_MODEL": model}}).resolveEnv(map[string]string{"ANTHROPIC_DEFAULT_FABLE_MODEL": "provider-default"})
			if err != nil {
				t.Fatal(err)
			}
			want := model
			if want == "" {
				want = "provider-default"
			}
			if env["ANTHROPIC_MODEL"] != want {
				t.Fatalf("resolved model = %q, want %q", env["ANTHROPIC_MODEL"], want)
			}
		})
	}
}

func TestConfigIsolatesResolvedWorkerEnvironments(t *testing.T) {
	t.Setenv("PWNMESH_CONFIG_TEST_VALUE", "expanded")
	c := coldStartConfig()
	c.CommonEnv["SHARED"] = "${PWNMESH_CONFIG_TEST_VALUE}"
	c.CommonEnv["OVERRIDE"] = "common"
	c.Workers[0].Env = map[string]string{"OVERRIDE": "${PWNMESH_CONFIG_TEST_VALUE}", "LOCAL": "first"}
	second := c.Workers[0]
	second.Name, second.Env = "second", nil
	c.Workers = append(c.Workers, second)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	firstEnv, secondEnv := c.Workers[0].Env, c.Workers[1].Env
	if firstEnv["SHARED"] != "expanded" || secondEnv["SHARED"] != "expanded" ||
		firstEnv["OVERRIDE"] != "expanded" || secondEnv["OVERRIDE"] != "common" || secondEnv["LOCAL"] != "" {
		t.Fatal("worker environment precedence or expansion changed")
	}
	firstEnv["SHARED"] = "changed"
	if secondEnv["SHARED"] != "expanded" || c.CommonEnv["SHARED"] != "${PWNMESH_CONFIG_TEST_VALUE}" {
		t.Fatal("resolved worker environments share mutable state")
	}
}
