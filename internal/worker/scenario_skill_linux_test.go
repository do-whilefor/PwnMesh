//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
)

type scenarioSkillBundle struct {
	scenario, title, root, reference string
	files                            fs.ReadFileFS
	tool                             func() agent.Tool
}

func scenarioSkillBundles() []scenarioSkillBundle {
	return []scenarioSkillBundle{
		{"pentest", "Web-Vulnhunt", "skills/web-vulnhunt/", "references/testing.md", pentestSkillFiles, pentestSkillTool},
		{"audit", "Flounder", "skills/flounder/", "references/verification.md", auditSkillFiles, auditSkillTool},
	}
}

func TestScenarioSkillReadsBundledFilesOnly(t *testing.T) {
	for _, bundle := range scenarioSkillBundles() {
		t.Run(bundle.scenario, func(t *testing.T) {
			tool := bundle.tool()
			if !tool.Parallel {
				t.Fatal("read-only bundled guidance lost parallel execution")
			}
			index, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
			if err != nil || !strings.Contains(index, bundle.title) {
				t.Fatalf("missing default skill: %q, %v", index, err)
			}
			// A same-named workspace file must never replace the bundled guidance.
			path := filepath.Join(t.TempDir(), "SKILL.md")
			if err := os.WriteFile(path, []byte("untrusted workspace content"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{path, "../web-vulnhunt/SKILL.md", "../flounder/SKILL.md", "../prompts/pentest.md", "references/../../SKILL.md", "/SKILL.md", ".", "references", "missing.md", `references\testing.md`} {
				raw, _ := json.Marshal(map[string]string{"path": path})
				if text, err := tool.Execute(context.Background(), raw); err == nil || text != "" {
					t.Fatalf("read outside bundled files: %q: %q, %v", path, text, err)
				}
			}
			for _, raw := range []string{`null`, `[]`, `{"path":3}`, `{"path":"SKILL.md","command":"pwd"}`} {
				if err := agent.ValidateArguments(tool.Schema, json.RawMessage(raw)); err == nil {
					t.Fatalf("schema accepted invalid read: %s", raw)
				}
			}
			if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":3}`)); err == nil {
				t.Fatal("reader accepted a malformed path")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := tool.Execute(ctx, json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled read: %v", err)
			}
		})
	}
}

func TestPentestClueMethodologyLoadsOnDemand(t *testing.T) {
	guide, err := pentestSkillTool().Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, detail := range []string{"CORS", "Sourcemap", "Self-XSS", "Rate Limit", "Scanner results"} {
		if strings.Contains(pentestPolicy, detail) || !strings.Contains(guide, detail) {
			t.Errorf("clue methodology %q must live in the on-demand guide", detail)
		}
	}
	for _, boundary := range []string{"Before web/API testing, call read_pentest_skill", "reproducible security-boundary failure with actual impact", "all eight CVSS 3.1 Base metrics", "Not Scored", "Unverified does not mean non-existent"} {
		if !strings.Contains(pentestPolicy, boundary) {
			t.Errorf("fixed policy lost required evidence boundary %q", boundary)
		}
	}
}

func TestScenarioSkillReferencesResolveInBinary(t *testing.T) {
	for _, bundle := range scenarioSkillBundles() {
		t.Run(bundle.scenario, func(t *testing.T) {
			root := bundle.root
			references := regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)
			seen := map[string]bool{}
			err := fs.WalkDir(bundle.files, strings.TrimSuffix(root, "/"), func(filename string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				name := strings.TrimPrefix(filename, root)
				seen[name] = true
				input, _ := json.Marshal(map[string]string{"path": name})
				content, err := bundle.tool().Execute(context.Background(), input)
				if err != nil || strings.TrimSpace(content) == "" {
					t.Fatalf("unreadable bundled asset %s: %v", name, err)
				}
				for _, link := range references.FindAllStringSubmatch(content, -1) {
					ref := path.Join(path.Dir(name), link[1])
					if _, err := bundle.files.ReadFile(root + ref); err != nil {
						t.Errorf("broken reference in %s: %s: %v", name, ref, err)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"SKILL.md", "references/discovery.md", "references/verification.md", "references/reporting.md", bundle.reference} {
				if !seen[name] {
					t.Errorf("missing bundled asset %s", name)
				}
			}
		})
	}
}

func TestScenarioSkillDoesNotReachCurator(t *testing.T) {
	for _, scenario := range []string{"pentest", "audit"} {
		t.Run(scenario, func(t *testing.T) {
			j := curationJob(t)
			j.Graph.Project.Scenario = scenario
			j.State.Graph.Project.Scenario = scenario
			o := Options{RunDir: t.TempDir()}
			configureRoleTools(t, j, &o)
			for _, tool := range o.Tools {
				if tool.Name == "read_pentest_skill" || tool.Name == "read_audit_skill" || tool.Name == "cvss31" {
					t.Fatalf("scenario capability reached Curate: %s", tool.Name)
				}
			}
			prompt, err := Prompt(j, false)
			if err != nil || strings.Contains(prompt, "read_pentest_skill") || strings.Contains(prompt, "read_audit_skill") {
				t.Fatalf("scenario guidance reached Curate: %v", err)
			}
		})
	}
}

func TestScenarioWorkerLoadsSkillOnDemandAndRetainsItDuringRepair(t *testing.T) {
	for _, bundle := range scenarioSkillBundles() {
		for _, kind := range []string{"reason", "explore"} {
			t.Run(bundle.scenario+"/"+kind, func(t *testing.T) {
				job, dir := scenarioJob(t, bundle.scenario, kind), t.TempDir()
				toolName := bundle.tool().Name
				calls := 0
				provider := scenarioProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
					calls++
					skill, _ := bundle.tool().Execute(context.Background(), json.RawMessage(`{}`))
					if strings.Contains(history[0].Text(), skill) || !strings.Contains(history[0].Text(), toolName) {
						t.Fatal("skill must be discoverable without inlining its contents")
					}
					if bundle.scenario == "pentest" && !strings.Contains(history[0].Text(), "Before web/API testing, call read_pentest_skill({}) once") {
						t.Fatal("pentest entry instruction must specify when and how to read the guide")
					}
					switch calls {
					case 1:
						return observationCall(toolName, `{}`, calls), nil
					case 2, 3:
						last := history[len(history)-1].Content
						if len(last) != 1 || last[0].Type != "tool_result" || last[0].IsError {
							t.Fatalf("failed skill read: %+v", last)
						}
						var content string
						if err := json.Unmarshal(last[0].Content, &content); err != nil {
							t.Fatal(err)
						}
						path := "SKILL.md"
						if calls == 3 {
							path = bundle.reference
						}
						want, err := bundle.files.ReadFile(bundle.root + path)
						if err != nil || content != string(want) {
							t.Fatalf("wrong skill content for %s: %v", path, err)
						}
						if calls == 2 {
							return observationCall(toolName, `{"path":"`+bundle.reference+`"}`, calls), nil
						}
						if kind == "explore" {
							return agent.Text("assistant", "Methodology loaded; no target observation."), nil
						}
					case 4:
						if kind != "explore" || len(definitions) != 0 {
							t.Fatal("result repair must not expose the skill tool")
						}
					default:
						t.Fatal("unexpected extra model call")
					}
					return agent.Text("assistant", `{"accepted":false,"reason":"Synthetic skill read only; no target evidence."}`), nil
				})
				result, err := runTestWorker(context.Background(), job, Options{Provider: provider, RunDir: dir})
				wantCalls := 3
				if kind == "explore" {
					wantCalls = 4
				}
				if err != nil || result.Status != "success" || calls != wantCalls {
					t.Fatalf("skill workflow: %+v, %v, calls=%d", result, err, calls)
				}
				saved := outcomeSession(t, dir)
				if !strings.Contains(saved.TaskPrompt, toolName) || strings.Contains(saved.RepairPrompt, scenarioPrompt(job)) {
					t.Fatal("skill discovery was lost or policy was duplicated in repair")
				}
				var reads int
				for _, message := range saved.History {
					for _, block := range message.Content {
						if block.Type == "tool_result" && !block.IsError {
							reads++
						}
					}
				}
				if reads != 2 {
					t.Fatalf("session lost skill reads: %d", reads)
				}
			})
		}
	}
}
