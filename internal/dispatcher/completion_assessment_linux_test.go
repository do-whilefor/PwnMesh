//go:build linux

package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/server"
	"pwnmesh/internal/worker"
)

// Earlier observations are seeded; prepare, immutable input, the real Worker,
// Dispatcher bridge, transactional preview and commit all remain production code.
// The scripted model must distinguish semantic failure from mechanical eligibility.
func TestCompletionAssessmentProductionBridgeCompletesOrKeepsMissingCoverage(t *testing.T) {
	for _, scenario := range []string{"accepted", "negative", "new_hint", "commit_race", "two_flags_optional", "two_flags_missing_submission", "two_flags_missing_report"} {
		t.Run(scenario, func(t *testing.T) {
			store, err := board.Open(filepath.Join(t.TempDir(), "assessment.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			api := httptest.NewServer(server.New(store))
			t.Cleanup(api.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			runner := &updateLocalRunner{}
			s := New(config.Config{Server: api.URL, Runtime: config.Runtime{MaxWorkers: 1, Interval: 1, HealthMode: "disabled"}}, runner)
			f := &updateRequestFixture{t: t, ctx: ctx, scheduler: s, runner: runner, workspace: t.TempDir()}
			goal := "Obtain actual verifier acceptance of request R17 in production."
			origin := "Inspect production request R17"
			optional := scenario == "two_flags_optional"
			flags := strings.HasPrefix(scenario, "two_flags_")
			missing := scenario == "negative" || strings.Contains(scenario, "missing_")
			conflict := scenario == "new_hint" || scenario == "commit_race"
			gap := "The verifier refused R17; obtain actual acceptance"
			if flags {
				origin = "Inspect the local two-flag CTF challenge"
				goal = "Obtain the two flags from the challenge."
				switch scenario {
				case "two_flags_missing_submission":
					goal += " Submit both to the platform and obtain actual acceptance."
					gap = "Both flags are evidenced, but required platform acceptance is missing"
				case "two_flags_missing_report":
					goal += " Deliver a report with both extraction procedures."
					gap = "Both flags are evidenced, but the required extraction report has not been delivered"
				}
			}
			var graph board.Graph
			f.do("POST", "/projects", map[string]any{"title": "Final assessment", "origin": origin, "goal": goal, "orchestration_version": 1}, &graph, nil)
			f.project = graph.Project
			base := projectPath(graph.Project.ID)
			f.do("POST", base+"/hints", map[string]string{"creator": "user", "content": "Keep the original deployment and identity"}, nil, nil)
			response := fmt.Sprintf(`{"request":"R17","accepted":%t}`, scenario != "negative")
			facts := []board.FactRecord{{ID: "f001", Description: "The verifier returned its decision", Scope: "production R17", Status: "valid", RunID: "retained", Evidence: []board.EvidenceRef{{RunID: "retained", Path: "/retained/response.json", Excerpt: response}}}}
			from := []string{"f001"}
			proof := "The retained production verifier response explicitly accepts R17"
			if flags {
				facts = []board.FactRecord{
					{ID: "f001", Description: "The first endpoint returned flag{synthetic_one}", Scope: "local challenge /first", Status: "valid", RunID: "retained", Evidence: []board.EvidenceRef{{RunID: "retained", Path: "/retained/first.txt", Excerpt: "HTTP 200: flag{synthetic_one}"}}},
					{ID: "f002", Description: "The second endpoint returned flag{synthetic_two}", Scope: "local challenge /second", Status: "valid", RunID: "retained", Evidence: []board.EvidenceRef{{RunID: "retained", Path: "/retained/second.txt", Excerpt: "HTTP 200: flag{synthetic_two}"}}},
				}
				from = []string{"f001", "f002"}
				proof = "Two distinct flags are evidenced by retained responses; submission, reporting and proving no third flag are not original requirements"
			}
			if err := store.Do(ctx, func(tx *board.Tx) error {
				g, err := tx.Load(graph.Project.ID)
				if err != nil {
					return err
				}
				for _, fact := range facts {
					g.Facts = append(g.Facts, board.Fact{ID: fact.ID, Description: fact.Description})
				}
				if optional {
					g.Intents = append(g.Intents, board.Intent{ID: "optional", From: from, Description: "Write an extra report and prove there is no third flag", Creator: "planner", CreatedAt: tx.Now})
				}
				if err = tx.Save(g); err != nil {
					return err
				}
				data, _ := json.Marshal(map[string]any{"facts": facts, "curation": board.CurationProgress{ThroughRevision: 2}})
				_, err = tx.Exec("INSERT INTO xloom_state(project_id,data,revision,decision_revision) VALUES(?,?,2,2) ON CONFLICT(project_id) DO UPDATE SET data=excluded.data,revision=excluded.revision,decision_revision=excluded.decision_revision", graph.Project.ID, string(data))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			lease := Lease{Run: "fixture@completion", Kind: "reason"}
			f.do("POST", base+"/reason/claim", map[string]string{"worker": lease.Run, "trigger": "finished_intents"}, nil, nil)
			job := worker.Job{RunID: "completion", Kind: "reason", WorkerType: "go", Graph: board.Graph{Project: graph.Project}, GraphRPC: true, ResultContractVersion: 2, Workspace: f.workspace, Budget: config.Task{Timeout: 60, MaxIntents: 2}}
			raw, _ := json.Marshal(job)
			execution := board.Execution{ProjectID: graph.Project.ID, ID: job.RunID, Namespace: "pwnmesh", Backend: "fixture", Kind: "reason", Lease: lease.Run, Job: raw}
			f.do("POST", base+"/executions/prepare", execution, &execution, &lease)
			if err := json.Unmarshal(execution.Job, &job); err != nil {
				t.Fatal(err)
			}
			if job.Decision == nil || job.Decision.ClosureProtocol != 1 || job.InputSnapshot == nil || job.Decision.StateVersion != job.InputSnapshot.StateVersion {
				t.Fatal("server did not bind root assessment protocol to its immutable input")
			}
			if !optional && (job.Decision.CompletionAssessment == nil || job.Decision.Mode != "completion" || job.Decision.CompletionAssessment.Acceptance != "not_checked" || job.Decision.CompletionAssessment.StateVersion != job.InputSnapshot.StateVersion) {
				t.Fatal("server did not bind the assessment to its actual immutable input")
			}
			run := &task{Job: job, Worker: config.Worker{Name: "fixture", Type: "go"}, Lease: lease, Execution: execution, LeaseTimeout: 30 * time.Second}
			f.start(run)
			calls, previews, commits := 0, 0, 0
			var addedHint board.Hint
			addRequirement := func() {
				if addedHint.ID == "" {
					f.do("POST", base+"/hints", map[string]string{"creator": "user", "content": "Also obtain approval of request R18"}, &addedHint, nil)
				}
			}
			forward := runner.handler
			runner.handler = func(ctx context.Context, j worker.Job, request worker.GraphRequest) (any, error) {
				if request.Op == "decision_preview" || request.Op == "decision_commit" {
					if calls < 2 || request.Batch == nil || request.Batch.Assessment == nil {
						t.Fatal("actions reached server before a separate root assessment turn")
					}
					if missing || conflict && calls >= 5 {
						if request.Batch.Assessment.Status != "missing" || len(request.Batch.Actions) != 1 || request.Batch.Actions[0].Op != "step" || request.Batch.Actions[0].GapID != "required" {
							t.Fatalf("new work lost the assessed original requirement: %+v", request.Batch)
						}
					} else if request.Batch.Assessment.Status != "satisfied" {
						t.Fatal("completion lost its satisfied root assessment")
					}
				}
				if request.Op == "decision_preview" {
					previews++
				}
				if request.Op == "decision_commit" {
					commits++
					if scenario == "commit_race" && commits == 1 {
						addRequirement()
					}
				}
				return forward(ctx, j, request)
			}
			combine := func(messages ...agent.Message) agent.Message {
				message := messages[0]
				for _, next := range messages[1:] {
					message.Content = append(message.Content, next.Content...)
				}
				return message
			}
			assess := func(status, description, inputID string) agent.Message {
				a := board.RootAssessment{Status: status, From: from, Description: description}
				if status == "missing" {
					a.Gaps = []board.RequirementGap{{ID: "required", InputIDs: []string{inputID}, Description: description}}
				}
				raw, _ := json.Marshal(a)
				return updateToolCall(fmt.Sprintf("assess%d", calls), "assess_root", string(raw))
			}
			completePayload, _ := json.Marshal(map[string]any{"op": "complete", "idempotency_key": "complete", "payload": map[string]any{"from": from, "description": proof}})
			commit := func() agent.Message {
				return updateToolCall(fmt.Sprintf("commit%d", calls), "graph_action", `{"op":"commit","idempotency_key":"commit"}`)
			}
			followup := func(description string) agent.Message {
				raw, _ := json.Marshal(map[string]any{"op": "step", "idempotency_key": "followup", "gap_id": "required", "payload": map[string]any{"action": "add", "from": from, "description": description}})
				return combine(updateToolCall("followup", "graph_action", string(raw)), commit())
			}
			provider := updateProvider(func(_ context.Context, history []agent.Message, definitions []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				prompt := ""
				for _, message := range history {
					prompt += message.Text()
				}
				switch calls {
				case 1:
					rootTool := false
					for _, definition := range definitions {
						rootTool = rootTool || definition.Name == "assess_root"
					}
					if !rootTool || !strings.Contains(prompt, goal) || !strings.Contains(prompt, "Keep the original deployment and identity") {
						t.Fatal("actual provider input lost original requirements or assessment capability")
					}
					for _, fact := range facts {
						encoded, _ := json.Marshal(fact.Evidence[0].Excerpt)
						if strings.Count(prompt, string(encoded)) != 1 {
							t.Fatal("actual provider input lost or repeated original acceptance evidence")
						}
					}
					if optional && !strings.Contains(prompt, "Write an extra report and prove there is no third flag") {
						t.Fatal("root assessment did not see the active optional task")
					}
					if missing {
						return assess("missing", gap, "goal"), nil
					}
					return assess("satisfied", proof, ""), nil
				case 2:
					if !strings.Contains(updateHistoryText(history), `\"assessment\"`) {
						t.Fatal("planning request did not observe the separate root assessment")
					}
					if missing {
						return followup(gap), nil
					}
					if scenario == "new_hint" {
						addRequirement()
					}
					complete := updateToolCall("complete", "graph_action", string(completePayload))
					if optional {
						return combine(updateToolCall("abandon", "graph_action", `{"op":"step","idempotency_key":"abandon","payload":{"action":"abandon","id":"optional","reason":"The requested two flags are evidenced; reporting and proving no third flag are not user requirements"}}`), complete, updateToolCall("preview", "graph_action", `{"op":"preview","idempotency_key":"preview"}`)), nil
					}
					return combine(complete, commit()), nil
				case 3:
					if optional {
						if !strings.Contains(updateHistoryText(history), "completion_review") {
							t.Fatal("completion did not observe authoritative preview evidence in a subsequent request")
						}
						return commit(), nil
					}
					if conflict {
						if !strings.Contains(updateHistoryText(history), "state_changed") {
							t.Fatal("concurrent user requirement did not invalidate completion")
						}
						return combine(updateToolCall("overview", "read_graph", `{"section":"overview"}`), updateToolCall("hints", "read_graph", `{"section":"hints"}`)), nil
					}
				case 4:
					if conflict && addedHint.ID != "" && strings.Contains(updateHistoryText(history), addedHint.Content) {
						return assess("missing", "The newly requested R18 approval has no supporting evidence", addedHint.ID), nil
					}
				case 5:
					if conflict {
						return followup("Obtain the newly requested R18 approval"), nil
					}
				}
				return agent.Message{}, fmt.Errorf("unexpected redundant request %d for %s", calls, scenario)
			})
			runner.options = worker.Options{Provider: provider, RunDir: t.TempDir()}
			result, err := runner.Run(ctx, run.Worker, run.Job)
			var final board.State
			f.do("GET", base+"/state", nil, &final, nil)
			wantCalls, wantPreviews, wantCommits := 2, 1, 1
			if missing {
				wantPreviews = 0
			} else if optional {
				wantCalls = 3
			} else if conflict {
				wantCalls = 5
				if scenario == "commit_race" {
					wantCommits = 2
				}
			}
			if err != nil || result.Status != "success" || calls != wantCalls || previews != wantPreviews || commits != wantCommits {
				t.Fatalf("assessed decision failed: result=%+v err=%v calls=%d/%d preview=%d/%d commit=%d/%d", result, err, calls, wantCalls, previews, wantPreviews, commits, wantCommits)
			}
			if missing || conflict {
				if final.Graph.Project.Status != "active" || len(final.Steps) != 1 || final.Steps[0].Status != "open" {
					t.Fatalf("missing requirements were completed or lost: status=%s steps=%+v", final.Graph.Project.Status, final.Steps)
				}
				if conflict && (result.Metrics == nil || result.Metrics.StateChanged != 1 || !result.Metrics.Committed) {
					t.Fatalf("same-session recovery lost version conflict: %+v", result.Metrics)
				}
			} else {
				if final.Graph.Project.Status != "completed" {
					t.Fatalf("satisfied requirements did not complete: status=%s", final.Graph.Project.Status)
				}
				if optional {
					abandoned := false
					for _, step := range final.Steps {
						abandoned = abandoned || step.ID == "optional" && step.Status == "abandoned"
						if step.Status != "abandoned" && step.Status != "completed" {
							t.Fatalf("completion left active optional work: %+v", step)
						}
					}
					if !abandoned {
						t.Fatal("completion did not explicitly abandon the optional step")
					}
				}
			}
		})
	}
}
