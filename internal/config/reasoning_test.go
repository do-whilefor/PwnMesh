package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

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
