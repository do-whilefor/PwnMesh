//go:build linux

package worker

import (
	"encoding/json"
	"os"
	"testing"

	"pwnmesh/internal/config"
)

func TestModelForJobUsesPersistedRoleEffortWithoutMutatingOtherRoles(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "fixture-token")
	t.Setenv("ANTHROPIC_MODEL", "fixture-model")
	t.Setenv("PWNMESH_REASONING_EFFORT", "max")
	j := Job{RunID: "saved-run", Kind: "reason", Budget: config.Task{ReasoningEffort: "high"}}
	raw, _ := json.Marshal(j)
	var recovered Job
	if err := json.Unmarshal(raw, &recovered); err != nil {
		t.Fatal(err)
	}
	p, err := modelForJob(recovered)
	if err != nil || p.ReasoningEffort != "high" || p.SessionID != j.RunID || p.Model != "fixture-model" {
		t.Fatalf("saved role policy did not reach provider: %+v %v", p, err)
	}
	other, err := modelForJob(Job{Kind: "explore"})
	if err != nil || other.ReasoningEffort != "max" || os.Getenv("PWNMESH_REASONING_EFFORT") != "max" {
		t.Fatalf("one role's override leaked to another: %+v %v", other, err)
	}
	t.Setenv("PWNMESH_REASONING_EFFORT", "low")
	p, err = modelForJob(recovered)
	if err != nil || p.ReasoningEffort != "high" {
		t.Fatal("same-run recovery changed persisted reasoning policy")
	}
}

func TestModelForJobLegacyFallbackAndInvalidPolicies(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "fixture-token")
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("ANTHROPIC_DEFAULT_FABLE_MODEL", "fixture-alias")
	t.Setenv("PWNMESH_REASONING_EFFORT", "")
	p, err := modelForJob(Job{})
	if err != nil || p.ReasoningEffort != "max" || p.Model != "fixture-alias" {
		t.Fatal("legacy backend defaults changed")
	}
	if _, err := modelForJob(Job{Budget: config.Task{ReasoningEffort: "unsupported"}}); err == nil {
		t.Fatal("invalid job policy reached the model")
	}
	t.Setenv("PWNMESH_REASONING_EFFORT", "unsupported")
	if _, err := modelForJob(Job{}); err == nil {
		t.Fatal("invalid inherited policy reached the model")
	}
}

func TestModelForJobLegacyBrandSettings(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "fixture-token")
	t.Setenv("ANTHROPIC_MODEL", "fixture-model")
	for key, value := range map[string]string{"REASONING_EFFORT": "low", "MAX_OUTPUT_TOKENS": "8192", "REQUEST_TIMEOUT": "45"} {
		t.Setenv("XLOOM_"+key, value)
		// Register cleanup before making the canonical key absent.
		t.Setenv("PWNMESH_"+key, "")
		if err := os.Unsetenv("PWNMESH_" + key); err != nil {
			t.Fatal(err)
		}
	}
	p, err := modelForJob(Job{})
	if err != nil || p.ReasoningEffort != "low" || p.MaxTokens != 8192 || p.Timeout.Seconds() != 45 {
		t.Fatalf("legacy worker settings were ignored: %+v %v", p, err)
	}
	t.Setenv("PWNMESH_REASONING_EFFORT", "high")
	p, err = modelForJob(Job{})
	if err != nil || p.ReasoningEffort != "high" {
		t.Fatalf("canonical setting did not take precedence: %+v %v", p, err)
	}
}
