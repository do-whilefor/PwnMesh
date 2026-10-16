package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestConfigRejectsTimeoutDurationOverflow(t *testing.T) {
	const maxSeconds = int64((1<<63 - 1) / time.Second)
	for _, tc := range []struct {
		name string
		set  func(*Config, int)
		max  int64
	}{
		{"interval", func(c *Config, n int) { c.Runtime.Interval = n }, maxSeconds / 2},
		{"health", func(c *Config, n int) { c.Runtime.HealthTimeout = n }, maxSeconds},
		{"reason", func(c *Config, n int) { c.Tasks.Reason.Timeout = n }, maxSeconds},
		{"curate", func(c *Config, n int) { c.Tasks.Curate.Timeout = n }, maxSeconds},
		{"execute", func(c *Config, n int) { c.Tasks.Explore.Timeout = n }, maxSeconds},
		{"conclusion", func(c *Config, n int) { c.Tasks.Explore.ConcludeTimeout = n }, maxSeconds},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := coldStartConfig()
			c.ModelSettingsPath = filepath.Join(t.TempDir(), "absent.json")
			tc.set(&c, int(tc.max+1))
			if err := c.Validate(); err == nil {
				t.Errorf("accepted overflowing %s seconds: %d", tc.name, tc.max+1)
			}
			tc.set(&c, int(tc.max))
			if err := c.Validate(); err != nil {
				t.Fatalf("rejected representable %s duration: %v", tc.name, err)
			}
		})
	}
}

func TestRoleReasoningPoliciesValidateAndSurviveJobSerialization(t *testing.T) {
	c := coldStartConfig()
	c.Tasks.Reason.ReasoningEffort = "high"
	c.Tasks.Curate.ReasoningEffort = "low"
	c.Tasks.Explore.ReasoningEffort = "max"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]string{"reason": "high", "curate": "low", "explore": "max"} {
		raw, err := json.Marshal(c.Task(kind))
		if err != nil {
			t.Fatal(err)
		}
		var recovered Task
		if err := json.Unmarshal(raw, &recovered); err != nil || recovered.ReasoningEffort != want {
			t.Fatalf("%s lost persisted reasoning policy: %s, %v", kind, raw, err)
		}
	}
	for _, bad := range []string{"medium", "HIGH", " max ", "none"} {
		for _, field := range []*Task{&c.Tasks.Reason, &c.Tasks.Curate, &c.Tasks.Explore} {
			previous := field.ReasoningEffort
			field.ReasoningEffort = bad
			if err := c.Validate(); err == nil {
				t.Fatalf("accepted unsupported reasoning policy %q", bad)
			}
			field.ReasoningEffort = previous
		}
	}
}

func TestRoleReasoningYAMLAndLegacyBudgetIdentity(t *testing.T) {
	var task Task
	d := yaml.NewDecoder(strings.NewReader("timeout: 300\nreasoning_effort: high\n"))
	d.KnownFields(true)
	if err := d.Decode(&task); err != nil || task.ReasoningEffort != "high" {
		t.Fatalf("role policy is not configurable: %+v %v", task, err)
	}
	raw, err := json.Marshal(Task{Timeout: 300})
	if err != nil || string(raw) != `{"timeout":300,"conclude_timeout":0,"max_intents":0}` {
		t.Fatalf("empty new field changed an existing job's budget identity: %s %v", raw, err)
	}
}
