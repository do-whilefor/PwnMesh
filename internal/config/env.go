package config

import (
	"fmt"
	"maps"
	"os"
	"strings"
)

// Getenv prefers PwnMesh settings and accepts their legacy environment names.
// An explicitly empty setting disables the legacy fallback.
func Getenv(key string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	if suffix, ok := strings.CutPrefix(key, "PWNMESH_"); ok {
		return os.Getenv("XLOOM_" + suffix)
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
		if suffix, legacy := strings.CutPrefix(key, "XLOOM_"); legacy {
			canonical := "PWNMESH_" + suffix
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
