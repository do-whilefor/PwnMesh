package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"

	"pwnmesh/internal/provider"
)

// Getenv reads only this worker's environment. It never inherits dispatcher
// process settings or resolves another setting name.
func (w Worker) Getenv(key string) string {
	return w.Env[key]
}

// ToolEnvironment is the configured Docker exec environment, excluding model
// secrets and launch-only values. Identity and delivery share this policy.
// Missing values inherit the image; explicit empty values override it.
func (w Worker) ToolEnvironment() (map[string]string, error) {
	env := map[string]string{}
	mode := w.Env["PWNMESH_CONNECTION_MODE"]
	for key, value := range w.Env {
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "ANTHROPIC_") || strings.HasPrefix(upper, "OPENAI_") || upper == "PWNMESH_MODEL_PROTOCOL" || key == "PWNMESH_MODEL_BRIDGE" || key == "PWNMESH_LAUNCH_TOKEN" {
			continue
		}
		if mode != "" {
			switch strings.ToUpper(key) {
			case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
				continue
			}
		}
		if key == "" || strings.Contains(key, "=") || strings.ContainsRune(key+value, '\x00') {
			return nil, errors.New("invalid worker environment")
		}
		env[key] = value
	}
	if mode == "" {
		return env, nil
	}
	proxy := ""
	if mode == "proxy" {
		var err error
		proxy, err = provider.ProxyURL(w.Env["PWNMESH_PROXY_URL"], true)
		if err != nil {
			return nil, err
		}
	} else if mode != "direct" {
		return nil, errors.New("connection_mode must be direct or proxy")
	}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		env[key] = proxy
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		env[key] = "localhost,127.0.0.1,::1,server"
	}
	return env, nil
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
			if !ok && !strings.HasPrefix(name, "ANTHROPIC_") && !strings.HasPrefix(name, "OPENAI_") && name != "PWNMESH_MODEL_PROTOCOL" {
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
