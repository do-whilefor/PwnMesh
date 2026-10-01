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
	for _, scenario := range []string{"accepted", "negative", "new_hint", "commit_race"} {
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
			const goal = "Obtain actual verifier acceptance of request R17 in production."
			var graph board.Graph
			f.do("POST", "/projects", map[string]any{"title": "Final assessment", "origin": "Inspect production request R17", "goal": goal, "orchestration_version": 1}, &graph, nil)
			f.project = graph.Project
			base := projectPath(graph.Project.ID)
			f.do("POST", base+"/hints", map[string]string{"creator": "user", "content": "Keep the original deployment and identity"}, nil, nil)
			response := fmt.Sprintf(`{"request":"R17","accepted":%t}`, scenario != "negative")
			fact := board.FactRecord{ID: "f001", Description: "The verifier returned its decision", Scope: "production R17", Status: "valid", RunID: "retained", Evidence: []board.EvidenceRef{{RunID: "retained", Path: "/retained/response.json", Excerpt: response}}}
			if err := store.Do(ctx, func(tx *board.Tx) error {
				g, err := tx.Load(graph.Project.ID)
				if err != nil {
					return err
				}
				g.Facts = append(g.Facts, board.Fact{ID: fact.ID, Description: fact.Description})
				if err = tx.Save(g); err != nil {
					return err
				}
				data, _ := json.Marshal(map[string]any{"facts": []board.FactRecord{fact}, "curation": board.CurationProgress{ThroughRevision: 2}})
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
			if job.Decision.CompletionAssessment == nil || job.Decision.Mode != "completion" || job.Decision.CompletionAssessment.Acceptance != "not_checked" || job.Decision.CompletionAssessment.StateVersion != job.InputSnapshot.StateVersion {
				t.Fatal("server did not bind the assessment to its actual immutable input")
			}
			run := &task{Job: job, Worker: config.Worker{Name: "fixture", Type: "go"}, Lease: lease, Execution: execution, LeaseTimeout: 30 * time.Second}
			f.start(run)
			calls, previews, commits := 0, 0, 0
			forward := runner.handler
			runner.handler = func(ctx context.Context, j worker.Job, request worker.GraphRequest) (any, error) {
				if request.Op == "decision_preview" {
					previews++
				}
				if request.Op == "decision_commit" {
					commits++
					if scenario == "commit_race" {
						f.do("POST", base+"/hints", map[string]string{"creator": "user", "content": "Also obtain approval of request R18"}, nil, nil)
					}
				}
				return forward(ctx, j, request)
			}
			provider := updateProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				if calls != 1 {
					return agent.Message{}, fmt.Errorf("unexpected redundant request %d", calls)
				}
				prompt := ""
				for _, message := range history {
					prompt += message.Text()
				}
				encoded, _ := json.Marshal(response)
				if !strings.Contains(prompt, goal) || !strings.Contains(prompt, "Keep the original deployment and identity") || strings.Count(prompt, string(encoded)) != 1 || !strings.Contains(prompt, "not_checked") {
					t.Fatal("actual provider input lost or repeated original acceptance evidence")
				}
				if scenario == "new_hint" {
					f.do("POST", base+"/hints", map[string]string{"creator": "user", "content": "Also obtain approval of request R18"}, nil, nil)
				}
				var proposed agent.Message
				if scenario == "negative" {
					proposed = updateToolCall("followup", "graph_action", `{"op":"step","idempotency_key":"followup","payload":{"action":"add","from":["f001"],"description":"Investigate the refusal and obtain actual R17 acceptance"}}`)
				} else {
					proposed = updateToolCall("complete", "graph_action", `{"op":"complete","idempotency_key":"complete","payload":{"from":["f001"],"description":"The retained production verifier response explicitly accepts R17"}}`)
				}
				proposed.Content = append(proposed.Content, updateToolCall("commit", "graph_action", `{"op":"commit","idempotency_key":"commit"}`).Content...)
				return proposed, nil
			})
			runner.options = worker.Options{Provider: provider, RunDir: t.TempDir()}
			result, err := runner.Run(ctx, run.Worker, run.Job)
			if calls != 1 {
				t.Fatalf("expected one model request, got %d", calls)
			}
			var final board.State
			f.do("GET", base+"/state", nil, &final, nil)
			switch scenario {
			case "accepted":
				if err != nil || result.Status != "success" || previews != 1 || commits != 1 || final.Graph.Project.Status != "completed" {
					t.Fatalf("fast completion did not traverse real preview/commit: %+v %v preview=%d commit=%d status=%s", result, err, previews, commits, final.Graph.Project.Status)
				}
			case "negative":
				if err != nil || result.Status != "success" || previews != 0 || commits != 1 || final.Graph.Project.Status != "active" || len(final.Steps) != 1 || final.Steps[0].Status != "open" {
					t.Fatal("mechanical eligibility falsely accepted missing coverage")
				}
			case "new_hint":
				if result.Status == "success" || previews != 1 || commits != 0 || final.Graph.Project.Status != "active" {
					t.Fatal("new user requirement bypassed the live preview CAS")
				}
			case "commit_race":
				if result.Status == "success" || previews != 1 || commits != 1 || final.Graph.Project.Status != "active" {
					t.Fatal("a requirement arriving after preview bypassed the commit CAS")
				}
			}
		})
	}
}
