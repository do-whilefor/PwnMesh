package config

import (
	"fmt"
	"maps"
	"os"
	"strings"
)

// Getenv reads an exact process setting without aliases.
func Getenv(key string) string {
	return os.Getenv(key)
}

// Getenv reads only this worker's environment. It never inherits dispatcher
// process settings or resolves another setting name.
func (w Worker) Getenv(key string) string {
	return w.Env[key]
}

// resolveEnv keeps the input maps intact and applies worker overrides before
// expansion. An explicitly empty worker setting overrides the common setting.
func (w Worker) resolveEnv(common map[string]string) (map[string]string, error) {
	env := make(map[string]string, len(common)+len(w.Env))
	maps.Copy(env, common)
	maps.Copy(env, w.Env)
	for key, value := range env {
		if suffix, obsolete := strings.CutPrefix(key, "XLOOM_"); obsolete {
			return nil, fmt.Errorf("environment variable %s is unsupported; use PWNMESH_%s for worker %q", key, suffix, w.Name)
		}
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
	if env["ANTHROPIC_MODEL"] == "" {
		env["ANTHROPIC_MODEL"] = env["ANTHROPIC_DEFAULT_FABLE_MODEL"]
	}
	return env, nil
}
