package config

import (
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
	"pwnmesh/internal/board/model"
	"pwnmesh/internal/modelconfig"
)

type Runtime struct {
	Interval          int    `yaml:"interval"`
	MaxWorkers        int    `yaml:"max_workers"`
	MaxProjects       int    `yaml:"max_running_projects"`
	MaxProjectWorkers int    `yaml:"max_project_workers"`
	HealthTimeout     int    `yaml:"healthcheck_timeout"`
	HealthMode        string `yaml:"worker_healthcheck"`
	Execution         string `yaml:"execution"`
	PromptGroup       string `yaml:"prompt_group"`
}
type Task = model.TaskBudget
type Tasks struct {
	Reason  Task `yaml:"reason"`
	Curate  Task `yaml:"curate"`
	Explore Task `yaml:"explore"`
}
type Container struct {
	Image           string   `yaml:"image"`
	Network         string   `yaml:"network_mode"`
	CompletedAction string   `yaml:"completed_action"`
	CapAdd          []string `yaml:"cap_add"`
	Socket          string   `yaml:"socket"`
	Namespace       string   `yaml:"namespace"`
}
type Worker struct {
	Name       string            `yaml:"name"`
	Type       string            `yaml:"type"`
	TaskTypes  []string          `yaml:"task_types"`
	MaxRunning int               `yaml:"max_running"`
	Priority   int               `yaml:"priority"`
	Env        map[string]string `yaml:"env"`
	baseEnv    map[string]string
}

// WithModelSettings preserves the environment configured before a saved model
// overlay. Removing that file restores this baseline without changing live jobs.
func (w Worker) WithModelSettings(settings *modelconfig.Settings) Worker {
	if w.baseEnv == nil {
		w.baseEnv = make(map[string]string, len(w.Env))
		maps.Copy(w.baseEnv, w.Env)
	}
	if settings == nil {
		w.Env = maps.Clone(w.baseEnv)
	} else {
		w.Env = settings.Overlay(w.baseEnv)
	}
	return w
}

type Config struct {
	ModelSettingsPath string            `yaml:"-"`
	Server            string            `yaml:"server"`
	Runtime           Runtime           `yaml:"runtime"`
	Tasks             Tasks             `yaml:"tasks"`
	Container         Container         `yaml:"container"`
	CommonEnv         map[string]string `yaml:"common_env"`
	Workers           []Worker          `yaml:"workers"`
}

func Load(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("configuration must contain exactly one YAML document")
	}
	return c, c.Validate()
}
func (c *Config) Validate() error {
	if c.ModelSettingsPath == "" {
		var err error
		c.ModelSettingsPath, err = modelconfig.ResolvePath()
		if err != nil {
			return err
		}
	}
	modelSettings, hasModelSettings, modelSettingsErr := modelconfig.Read(c.ModelSettingsPath)
	if modelSettingsErr != nil {
		return modelSettingsErr
	}
	if c.Runtime.Execution == "" {
		c.Runtime.Execution = "container"
	}
	if c.Runtime.HealthMode == "" {
		c.Runtime.HealthMode = "startup_only"
	}
	if c.Runtime.PromptGroup == "" {
		c.Runtime.PromptGroup = "default"
	}
	if c.Tasks.Reason.MaxIntents == 0 {
		c.Tasks.Reason.MaxIntents = 3
	}
	if c.Container.Socket == "" {
		c.Container.Socket = "/var/run/docker.sock"
	}
	if c.Container.Namespace == "" {
		c.Container.Namespace = "pwnmesh"
	}
	u, err := url.Parse(c.Server)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("server must be an HTTP URL")
	}
	if c.Runtime.Execution != "container" {
		return fmt.Errorf("only container execution is supported")
	}
	if c.Runtime.PromptGroup != "default" {
		return fmt.Errorf("only the default prompt group is supported")
	}
	if !slices.Contains([]string{"disabled", "startup_only", "startup_and_task"}, c.Runtime.HealthMode) {
		return fmt.Errorf("unknown worker_healthcheck mode")
	}
	if c.Runtime.Interval <= 0 || c.Runtime.MaxWorkers <= 0 || c.Runtime.MaxProjects <= 0 || c.Runtime.MaxProjectWorkers <= 0 || c.Runtime.HealthTimeout <= 0 {
		return fmt.Errorf("invalid runtime limits")
	}
	for _, t := range []Task{c.Tasks.Reason, c.Tasks.Curate, c.Tasks.Explore} {
		if t.Timeout < 0 {
			return fmt.Errorf("task timeout must be nonnegative (0 disables the execution deadline)")
		}
		if !slices.Contains([]string{"", "low", "high", "max"}, t.ReasoningEffort) {
			return fmt.Errorf("task reasoning_effort must be low, high or max, or omitted to inherit the backend setting")
		}
	}
	if c.Tasks.Explore.ConcludeTimeout <= 0 || c.Tasks.Reason.MaxIntents <= 0 {
		return fmt.Errorf("explore conclude_timeout and reason max_intents must be positive")
	}
	if c.Container.Image == "" || c.Container.Network == "" || !slices.Contains([]string{"stop", "remove"}, c.Container.CompletedAction) {
		return fmt.Errorf("invalid container configuration")
	}
	for _, ch := range c.Container.Namespace {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
			return fmt.Errorf("container namespace must contain lowercase letters, digits or hyphens")
		}
	}
	if len(c.Workers) == 0 {
		return fmt.Errorf("at least one worker is required")
	}
	seen := map[string]bool{}
	capabilities := map[string]bool{}
	for n := range c.Workers {
		w := &c.Workers[n]
		if strings.TrimSpace(w.Name) == "" || seen[w.Name] || w.MaxRunning <= 0 || w.Priority < 0 {
			return fmt.Errorf("invalid or duplicate worker %q", w.Name)
		}
		seen[w.Name] = true
		if w.Type == "" {
			w.Type = "go"
		}
		if w.Type != "go" {
			return fmt.Errorf("worker %q must use go", w.Name)
		}
		if len(w.TaskTypes) == 0 {
			return fmt.Errorf("worker %q has no task types", w.Name)
		}
		types := map[string]bool{}
		for _, typ := range w.TaskTypes {
			if !slices.Contains([]string{"reason", "curate", "explore"}, typ) || types[typ] {
				return fmt.Errorf("invalid task types for %q", w.Name)
			}
			types[typ] = true
			capabilities[typ] = true
		}
		env, err := w.resolveEnv(c.CommonEnv)
		if err != nil {
			return err
		}
		w.Env = env
		if hasModelSettings {
			*w = w.WithModelSettings(&modelSettings)
		}
	}
	for _, role := range []struct{ kind, name string }{{"reason", "Decide"}, {"curate", "Curate"}, {"explore", "Execute"}} {
		if !capabilities[role.kind] {
			return fmt.Errorf("at least one worker must support %s (%s) to run projects", role.kind, role.name)
		}
	}
	return nil
}
func (c Config) Task(kind string) Task {
	switch kind {
	case "reason":
		return c.Tasks.Reason
	case "curate":
		return c.Tasks.Curate
	default:
		return c.Tasks.Explore
	}
}
