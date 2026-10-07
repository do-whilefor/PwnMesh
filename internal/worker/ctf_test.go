//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func TestCTFSubmissionCapabilityRequiresConfiguredChannel(t *testing.T) {
	for _, tc := range []struct {
		name, host, token string
		available         bool
	}{
		{name: "unconfigured"},
		{name: "host only", host: "contest.invalid"},
		{name: "token only", token: "fixture-token"},
		{name: "blank host", host: " \t\n", token: "fixture-token"},
		{name: "blank token", host: "contest.invalid", token: " \t\n"},
		{name: "configured", host: "contest.invalid", token: "fixture-token", available: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"TSEC_SERVER_HOST": tc.host, "TSEC_AGENT_TOKEN": tc.token}
			if tsecSubmissionAvailable(func(key string) string { return env[key] }) != tc.available {
				t.Fatal("incorrect submission capability")
			}
			// Restore process environment after each case; no parallel test mutates it.
			t.Setenv("TSEC_SERVER_HOST", tc.host)
			t.Setenv("TSEC_AGENT_TOKEN", tc.token)
			for _, kind := range []string{"reason", "explore"} {
				job := scenarioJob(t, "ctf", kind)
				prompt, err := Prompt(job, false, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(prompt, ctfPolicy) != tc.available {
					t.Fatalf("%s prompt disagrees with the configured submission capability", kind)
				}
				options := Options{RunDir: t.TempDir()}
				if err := ConfigureRuntimeTools(job, &options); err != nil {
					t.Fatal(err)
				}
				var recipe bool
				availableTools := map[string]bool{}
				for _, tool := range options.Tools {
					availableTools[tool.Name] = true
					recipe = recipe || strings.Contains(tool.Description, ctfExecution)
					if !tc.available && strings.Contains(tool.Description, "TSEC_") {
						t.Fatalf("%s advertises an unconfigured contest service", tool.Name)
					}
				}
				if recipe != (tc.available && kind == "explore") {
					t.Fatalf("%s execution recipe disagrees with submission capability", kind)
				}
				if kind == "explore" && (!availableTools["bash"] || !availableTools["read"] || !availableTools["write"]) {
					t.Fatalf("missing ordinary execution tools: %v", availableTools)
				}
			}
		})
	}
}

func TestUnconfiguredCTFKeepsUserInstructionsWithoutSubmissionPolicy(t *testing.T) {
	t.Setenv("TSEC_SERVER_HOST", "")
	t.Setenv("TSEC_AGENT_TOKEN", "")
	for _, kind := range []string{"reason", "explore"} {
		t.Run(kind, func(t *testing.T) {
			job := scenarioJob(t, "ctf", kind)
			const custom = "Find two local fixture flags. A user-specified service is https://custom.invalid/finish."
			job.Graph.Facts[0].Description = custom
			if job.State != nil {
				// scenarioJob's reason view is intentionally empty; retain the user's
				// fixture in the same immutable decision view used by production.
				job.State.Graph = job.Graph
				job.Decision.StateVersion = board.DecisionStateVersion(*job.State)
				job.Decision.View, _ = json.Marshal(map[string]string{"origin": custom})
			}
			calls := 0
			provider := scenarioProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				input := history[0].Text()
				if !strings.Contains(input, custom) {
					t.Fatal("scenario capability filtering removed user task instructions")
				}
				for _, unexpected := range []string{"CTF submission evidence:", "TSEC_", "Submission requires an Execute action"} {
					if strings.Contains(input, unexpected) {
						t.Fatalf("model received an unconfigured submission policy: %q", unexpected)
					}
					for _, definition := range definitions {
						if strings.Contains(definition.Description, unexpected) {
							t.Fatalf("model received an unconfigured submission recipe: %q", unexpected)
						}
					}
				}
				return agent.Text("assistant", `{"accepted":false,"reason":"Synthetic capability check only."}`), nil
			})
			result, err := runTestWorker(context.Background(), job, Options{RunDir: t.TempDir(), Provider: provider})
			if err != nil || result.Status != "success" || calls != 1 {
				t.Fatalf("run: %+v, %v; calls=%d", result, err, calls)
			}
		})
	}
}

func TestCTFSubmissionRulesReachModelAndPersistAcrossPhases(t *testing.T) {
	t.Setenv("TSEC_SERVER_HOST", "contest.invalid")
	t.Setenv("TSEC_AGENT_TOKEN", "fixture-token")
	for _, phase := range []string{"execute", "conclude", "repair"} {
		t.Run(phase, func(t *testing.T) {
			job := scenarioJob(t, "ctf", "explore")
			job.ResultContractVersion = 2
			runDir := t.TempDir()
			var stop chan struct{}
			if phase == "conclude" {
				stop = make(chan struct{})
				close(stop)
			}
			calls := 0
			provider := scenarioProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				var input string
				for _, message := range history {
					input += message.Text() + "\n"
				}
				for _, rule := range []string{"high confidence", "Never use the submission API to brute force", "API response explicitly confirms acceptance"} {
					if !strings.Contains(input, rule) {
						t.Fatalf("model input lost submission rule %q in %s", rule, phase)
					}
				}
				restricted := phase == "conclude" || (phase == "repair" && calls == 2)
				if restricted != (len(definitions) == 0) {
					t.Fatalf("wrong phase capabilities: restricted=%v definitions=%v", restricted, definitions)
				}
				if strings.Count(input, ctfPolicy) != 1 || strings.Count(input, "<task_graph>") != 1 || strings.Contains(input, ctfExecution) {
					t.Fatal("task policy was duplicated or execution instructions leaked into phase history")
				}
				submission := false
				for _, definition := range definitions {
					if definition.Name == "bash" {
						submission = strings.Contains(definition.Description, "${TSEC_SERVER_HOST}/api/submit") && strings.Contains(definition.Description, "${TSEC_AGENT_TOKEN}")
					}
				}
				if submission == restricted {
					t.Fatal("CTF submission recipe must exist only in executable tool definitions")
				}
				if phase == "repair" && calls == 1 {
					return agent.Text("assistant", "A candidate flag is not enough to claim submission success."), nil
				}
				return agent.Text("assistant", `{"accepted":false,"reason":"Synthetic fixture: no high-confidence flag or confirmed submission response."}`), nil
			})
			result, err := runTestWorker(context.Background(), job, Options{RunDir: runDir, Provider: provider, SoftStop: stop})
			wantCalls := 1
			if phase == "repair" {
				wantCalls = 2
			}
			if err != nil || result.Status != "success" || calls != wantCalls {
				t.Fatalf("run: %+v, %v; model calls=%d", result, err, calls)
			}
			raw, err := os.ReadFile(filepath.Join(runDir, "session.json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved session
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(saved.TaskPrompt, ctfPolicy) || strings.Contains(saved.ConclusionPrompt+saved.RepairPrompt, ctfPolicy) {
				t.Fatal("durable phase prompts did not retain exactly one shared CTF policy")
			}
		})
	}
}
