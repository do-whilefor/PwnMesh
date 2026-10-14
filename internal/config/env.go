package config

import (
	"fmt"
	"maps"
	"os"
	"strings"
)

const (
	envPrefix       = "PWNMESH_"
	legacyEnvPrefix = "XLOOM_"
)

// Getenv prefers PwnMesh settings and accepts their legacy environment names.
// An explicitly empty setting disables the legacy fallback.
func Getenv(key string) string {
	return lookupEnv(key, os.LookupEnv)
}

// Getenv reads only this worker's environment, using the same pre-PwnMesh
// aliases as process settings. It never inherits dispatcher process settings.
func (w Worker) Getenv(key string) string {
	return lookupEnv(key, func(name string) (string, bool) {
		value, ok := w.Env[name]
		return value, ok
	})
}

func lookupEnv(key string, lookup func(string) (string, bool)) string {
	if value, ok := lookup(key); ok {
		return value
	}
	if suffix, ok := strings.CutPrefix(key, envPrefix); ok {
		value, _ := lookup(legacyEnvPrefix + suffix)
		return value
	}
	return ""
}

// resolveEnv keeps the input maps intact and applies worker overrides before
// expansion. Normalize aliases last so execution and environment identity agree.
// An explicitly empty canonical setting still overrides its legacy alias.
func (w Worker) resolveEnv(common map[string]string) (map[string]string, error) {
	env := make(map[string]string, len(common)+len(w.Env))
	maps.Copy(env, common)
	maps.Copy(env, w.Env)
	for key, value := range env {
		var missing string
		env[key] = os.Expand(value, func(name string) string {
			value, ok := os.LookupEnv(name)
			if !ok && !strings.HasPrefix(name, "ANTHROPIC_") {
				missing = name
			}
			return value
		})
		if missing != "" {
			return nil, fmt.Errorf("environment variable %s is required for worker %q", missing, w.Name)
		}
	}
	for key, value := range env {
		if suffix, legacy := strings.CutPrefix(key, legacyEnvPrefix); legacy {
			canonical := envPrefix + suffix
			if _, exists := env[canonical]; !exists {
				env[canonical] = value
			}
		}
	}
	if env["ANTHROPIC_MODEL"] == "" {
		env["ANTHROPIC_MODEL"] = env["ANTHROPIC_DEFAULT_FABLE_MODEL"]
	}
	return env, nil
}
