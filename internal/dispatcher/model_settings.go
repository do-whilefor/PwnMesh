package dispatcher

import (
	"pwnmesh/internal/config"
	"pwnmesh/internal/modelconfig"
)

// Called only by the scheduler loop. Existing tasks own their earlier Env map;
// new assignments see the replacement map at the next scheduling tick.
func (s *Scheduler) refreshModelSettings() error {
	settings, found, err := modelconfig.Read(s.Config.ModelSettingsPath)
	if err != nil {
		return err
	}
	if !found || settings == s.modelSettings {
		return nil
	}
	for n := range s.Config.Workers {
		s.Config.Workers[n].Env = settings.Overlay(s.Config.Workers[n].Env)
	}
	s.modelSettings = settings
	clear(s.incompatible)
	clear(s.unhealthy)
	clear(s.rejected)
	return nil
}

// A completion from an older launch must not poison or clear the new model's
// readiness gate. Tokens participate here even though execution IDs omit them.
func (s *Scheduler) currentModelSettings(w config.Worker) bool {
	if s.Config.ModelSettingsPath == "" {
		return true
	}
	for _, current := range s.Config.Workers {
		if current.Name != w.Name {
			continue
		}
		for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "PWNMESH_REASONING_EFFORT", "PWNMESH_MAX_OUTPUT_TOKENS", "PWNMESH_REQUEST_TIMEOUT", "PWNMESH_CONNECTION_MODE", "PWNMESH_PROXY_URL"} {
			if current.Env[key] != w.Env[key] {
				return false
			}
		}
		return true
	}
	return false
}
