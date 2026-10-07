package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func coldStartConfig() Config {
	return Config{
		Server:    "http://server:8000",
		Runtime:   Runtime{Interval: 2, MaxWorkers: 4, MaxProjects: 2, MaxProjectWorkers: 2, HealthTimeout: 30},
		Tasks:     Tasks{Reason: Task{Timeout: 300, MaxIntents: 3}, Explore: Task{ConcludeTimeout: 60}},
		Container: Container{Image: "fixture", Network: "bridge", CompletedAction: "stop"},
		CommonEnv: map[string]string{"ANTHROPIC_BASE_URL": "http://unused.invalid", "ANTHROPIC_AUTH_TOKEN": "fixture", "ANTHROPIC_MODEL": "fixture"},
		Workers:   []Worker{{Name: "fixture", Type: "go", TaskTypes: []string{"reason", "explore"}, MaxRunning: 4}},
	}
}

func TestDecideExecuteConfigDoesNotRequireBootstrapBudget(t *testing.T) {
	c := coldStartConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("reason + explore configuration: %v", err)
	}
}

func TestConfigRequiresDecideCapability(t *testing.T) {
	c := coldStartConfig()
	c.Workers[0].TaskTypes = []string{"explore"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "reason (Decide)") {
		t.Fatalf("missing Decide capability: %v", err)
	}
}

func TestConfigRejectsRetiredBootstrapAndMock(t *testing.T) {
	c := coldStartConfig()
	c.Workers[0].TaskTypes = append(c.Workers[0].TaskTypes, "bootstrap")
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "invalid task types") {
		t.Fatalf("accepted retired bootstrap capability: %v", err)
	}
	c = coldStartConfig()
	c.Workers[0].Type = "mock"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "must use go") {
		t.Fatalf("accepted retired mock backend: %v", err)
	}
	decoder := yaml.NewDecoder(strings.NewReader("bootstrap: {timeout: 60, conclude_timeout: 5}"))
	decoder.KnownFields(true)
	if err := decoder.Decode(&Tasks{}); err == nil {
		t.Fatal("accepted retired bootstrap budget")
	}
}

func TestDecideCapabilityCanBeProvidedBySeparateWorker(t *testing.T) {
	c := coldStartConfig()
	c.Workers[0].TaskTypes = []string{"explore"}
	c.Workers = append(c.Workers, Worker{Name: "planner", Type: "go", TaskTypes: []string{"reason"}, MaxRunning: 1})
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
