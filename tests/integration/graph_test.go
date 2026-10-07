//go:build linux

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/dispatcher"
	"pwnmesh/internal/docker"
	"pwnmesh/internal/server"
)

// Real containers/Worker/HTTP/SQLite with a deterministic local model: no
// external target or model is contacted. The model deliberately verifies each
// intermediate graph reply before returning the next call or final contract.
func TestDockerGraphBridgeDecideAndExecute(t *testing.T) {
	image := os.Getenv("PWNMESH_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set PWNMESH_DOCKER_TEST_IMAGE for real graph bridge acceptance")
	}
	store, err := board.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := httptest.NewServer(server.New(store))
	defer api.Close()
	client := &dispatcher.Client{Base: api.URL}
	var project board.Graph
	if err = client.Do(context.Background(), "POST", "/projects", map[string]any{"title": "Controlled FGS graph bridge", "origin": "Use only a synthetic local evidence file.", "goal": "Submit verified synthetic evidence and its candidate note, then finish.", "bootstrap_enabled": false}, &project, nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	turns := map[string]int{}
	decisionRuns := map[string]bool{}
	decisionStages := map[string]string{}
	decisionPlans := map[string]string{}
	rootAssessments := map[string]board.RootAssessment{}
	var executionRun, factID string
	var runtimeContainer string
	var observedMidTask, completionReviewed, rootAssessmentReviewed bool
	var modelErrors []string
	// Visually similar Unicode remains byte-distinct; the retained file also
	// preserves CRLF and an integer beyond JavaScript's exact number range.
	const proof = "synthetic-proof-verified e\u0301 \u00e9 9007199254740993\r\n"
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fail := func(err error) {
			modelErrors = append(modelErrors, err.Error())
			http.Error(w, "controlled model assertion failed", http.StatusBadRequest)
		}
		var request struct {
			Messages []agent.Message    `json:"messages"`
			Tools    []agent.Definition `json:"tools"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&request); err != nil {
			fail(err)
			return
		}
		run := r.Header.Get("x-opencode-session")
		if run == "" {
			fail(errors.New("missing model session ID"))
			return
		}
		turns[run]++
		names := []string{}
		for _, tool := range request.Tools {
			names = append(names, tool.Name)
		}
		sort.Strings(names)
		assessRoot := strings.Join(names, ",") == "assess_root,graph_action,read_evidence,read_graph,read_snapshot"
		decide := assessRoot || strings.Join(names, ",") == "graph_action,read_evidence,read_graph,read_snapshot"
		if !decide && strings.Join(names, ",") != "bash,edit,find,finish_step,graph_action,grep,ls,read,read_evidence,read_graph,read_snapshot,run_graph,write" {
			fail(fmt.Errorf("unexpected tool capabilities: %v", names))
			return
		}
		reply := scriptedModelReply{w: w, turn: turns[run]}
		completed := func(value any) {
			raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": value})
			reply.respond(agent.Block{Type: "text", Text: string(raw)}, "end_turn")
		}
		lastResult := func() (string, error) {
			return scriptedToolResult(request.Messages, fmt.Sprintf("call-%d", turns[run]-1), false)
		}
		var state board.State
		if err := client.Do(r.Context(), "GET", "/projects/"+project.Project.ID+"/state", nil, &state, nil); err != nil {
			fail(err)
			return
		}
		checkEvidence := func(refs []board.EvidenceRef, evidenceRun string) error {
			if len(refs) != 1 {
				return fmt.Errorf("expected one materialized evidence reference, got %d", len(refs))
			}
			ref := refs[0]
			if evidenceRun == "" || ref.RunID != evidenceRun || ref.Excerpt != proof || ref.StartLine != 1 || ref.EndLine != 1 || !strings.HasPrefix(ref.Path, "/workspace/.pwnmesh/runs/"+evidenceRun+"/evidence/") {
				return fmt.Errorf("invalid materialized evidence reference: %+v", ref)
			}
			retained, err := dockerExec(r.Context(), runtimeContainer, []string{"cat", "--", ref.Path})
			if err != nil || retained != proof {
				return fmt.Errorf("retained evidence differs from source bytes: %q, %v", retained, err)
			}
			return nil
		}
		if decide {
			decisionRuns[run] = true
			stagePlan := func() {
				switch decisionPlans[run] {
				case "step":
					decisionStages[run] = "step_staged"
					payload := map[string]any{"action": "add", "from": []string{"origin"}, "description": "Write synthetic evidence, submit a Fact and Candidate, then finish."}
					if assessRoot {
						reply.call("graph_action", map[string]any{"op": "step", "idempotency_key": "plan-one", "gap_id": "synthetic-evidence", "payload": payload})
					} else {
						reply.action("step", "plan-one", payload)
					}
				case "complete":
					decisionStages[run] = "completion_staged"
					reply.action("complete", "finish", map[string]any{"from": []string{factID}, "description": "Verified synthetic evidence and Candidate are retained."})
				default:
					// Facts may trigger Decide while Execute is still running.
					// Keep its current work with a checked empty batch.
					decisionStages[run] = "committing"
					reply.action("commit", "commit", map[string]any{})
				}
			}
			switch decisionStages[run] {
			case "":
				if len(state.Steps) == 0 {
					decisionPlans[run] = "step"
				} else if len(state.Candidates) > 0 && state.Steps[0].Status == "completed" {
					decisionPlans[run] = "complete"
				} else {
					decisionPlans[run] = "commit"
				}
				if assessRoot {
					assessment := board.RootAssessment{Status: "missing", From: []string{}, Description: "The required evidence, Candidate and accepted Step result are not all available.", Gaps: []board.RequirementGap{{ID: "synthetic-evidence", InputIDs: []string{"goal"}, Description: "Publish verified synthetic evidence and its Candidate, then finish the assigned Step."}}}
					if decisionPlans[run] == "complete" {
						assessment = board.RootAssessment{Status: "satisfied", From: []string{factID}, Description: "The verified synthetic evidence and Candidate are retained and the assigned Step has completed."}
					}
					rootAssessments[run] = assessment
					decisionStages[run] = "root_assessed"
					reply.call("assess_root", assessment)
					return
				}
				stagePlan()
				return
			case "root_assessed":
				text, err := lastResult()
				if err != nil {
					fail(err)
					return
				}
				var receipt struct {
					Assessment board.RootAssessment `json:"assessment"`
					Next       string               `json:"next"`
				}
				if err := json.Unmarshal([]byte(text), &receipt); err != nil || !assessRoot || !reflect.DeepEqual(receipt.Assessment, rootAssessments[run]) || receipt.Next == "" {
					fail(fmt.Errorf("invalid root assessment receipt: %s", text))
					return
				}
				rootAssessmentReviewed = true
				stagePlan()
				return
			case "step_staged", "completion_staged":
				text, err := lastResult()
				if err != nil {
					fail(err)
					return
				}
				var draft struct {
					Draft bool   `json:"draft"`
					Op    string `json:"op"`
					ID    string `json:"id"`
				}
				wantOp := "step"
				if decisionStages[run] == "completion_staged" {
					wantOp = "complete"
				}
				if err := json.Unmarshal([]byte(text), &draft); err != nil || !draft.Draft || draft.Op != wantOp || wantOp == "step" && draft.ID != "$plan-one" {
					fail(fmt.Errorf("invalid private %s draft reply: %s", wantOp, text))
					return
				}
				if wantOp == "complete" {
					decisionStages[run] = "completion_preview"
					reply.action("preview", "preview", map[string]any{})
				} else {
					decisionStages[run] = "committing"
					reply.action("commit", "commit", map[string]any{})
				}
				return
			case "completion_preview":
				text, err := lastResult()
				if err != nil {
					fail(err)
					return
				}
				var receipt board.DecisionReceipt
				if err := json.Unmarshal([]byte(text), &receipt); err != nil || receipt.Committed || receipt.Completed || receipt.ValidationScope != "protocol_only" || receipt.CompletionReview == nil {
					fail(fmt.Errorf("invalid completion preview receipt: %s", text))
					return
				}
				review := receipt.CompletionReview
				if review.Acceptance != "not_checked" || len(review.StateVersion) != 64 || len(review.UserInputs) != 2 || len(review.From) != 1 || review.From[0] != factID || len(review.FactRecords) != 1 || review.FactRecords[0].ID != factID || len(review.OmittedFactIDs) != 0 {
					fail(fmt.Errorf("completion preview omitted original inputs or cited proof: %+v", review))
					return
				}
				if err := checkEvidence(review.FactRecords[0].Evidence, executionRun); err != nil {
					fail(err)
					return
				}
				completionReviewed = true
				decisionStages[run] = "committing"
				reply.action("commit", "commit", map[string]any{})
				return
			default:
				// A commit either ends successfully or terminates this stale run.
				// A changed graph is handled by a new registered decision input.
				fail(fmt.Errorf("unexpected Decide turn after %s: %s", decisionStages[run], run))
				return
			}
		}
		executionRun = run
		path := "/workspace/.pwnmesh/runs/" + run + "/output-proof.txt"
		// The model selects source bytes. Worker fills run identity, retained
		// path and exact excerpt, including the original trailing newline.
		evidence := []map[string]any{{"path": path, "start_line": 1, "end_line": 1}}
		switch turns[run] {
		case 1:
			reply.call("write", map[string]any{"path": path, "content": proof})
		case 2:
			if _, err := lastResult(); err != nil {
				fail(err)
				return
			}
			reply.call("graph_action", map[string]any{"op": "fact", "idempotency_key": "fact-one", "payload": map[string]any{"description": "The controlled evidence file contains synthetic-proof-verified.", "scope": "Synthetic local test only", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence": evidence}})
		case 3:
			text, err := lastResult()
			if err != nil {
				fail(err)
				return
			}
			var result board.StateActionResult
			if err = json.Unmarshal([]byte(text), &result); err != nil || result.Op != "fact" || result.ID == "" {
				fail(fmt.Errorf("invalid Fact reply: %s", text))
				return
			}
			factID = result.ID
			for _, f := range state.FactRecords {
				if f.ID == factID && f.RunID == "controlled@"+run {
					if err := checkEvidence(f.Evidence, run); err != nil {
						fail(err)
						return
					}
					for _, step := range state.Steps {
						if step.Status == "running" {
							observedMidTask = true
						}
					}
				}
			}
			reply.call("graph_action", map[string]any{"op": "candidate", "idempotency_key": "candidate-one", "payload": map[string]any{"claim": "Synthetic evidence is retained and verified.", "reason": "The retained file was observed during this Step.", "scope": "Synthetic local test only", "status": "verified", "sources": []string{factID}, "evidence": evidence}})
		case 4:
			text, err := lastResult()
			if err != nil {
				fail(err)
				return
			}
			var result board.StateActionResult
			if err = json.Unmarshal([]byte(text), &result); err != nil || result.Op != "candidate" || len(state.Candidates) != 1 {
				fail(fmt.Errorf("invalid Candidate reply: %s", text))
				return
			}
			if err := checkEvidence(state.Candidates[0].Evidence, run); err != nil {
				fail(err)
				return
			}
			completed(map[string]any{"fact_id": factID})
		default:
			fail(fmt.Errorf("unexpected Execute turn %d", turns[run]))
		}
	}))
	defer model.Close()
	network := testContainerNetwork(t)
	c := config.Config{Server: api.URL, Runtime: config.Runtime{Interval: 1, MaxWorkers: 2, MaxProjects: 1, MaxProjectWorkers: 2, HealthMode: "disabled", HealthTimeout: 10}, Tasks: config.Tasks{Reason: config.Task{Timeout: 45, MaxIntents: 3}, Explore: config.Task{Timeout: 45, ConcludeTimeout: 5}}, Container: config.Container{Image: image, Network: network, Namespace: fmt.Sprintf("pwnmesh-graph-%d", time.Now().UnixNano()), CompletedAction: "stop"}, Workers: []config.Worker{{Name: "controlled", Type: "go", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: 1, Env: map[string]string{"ANTHROPIC_BASE_URL": model.URL, "ANTHROPIC_AUTH_TOKEN": "controlled-test-only", "ANTHROPIC_MODEL": "controlled", "PWNMESH_REQUEST_TIMEOUT": "10"}}}}
	runtimeContainer = c.Container.Namespace + "-dispatch-" + project.Project.ID
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	runner := docker.New(c.Container)
	defer runner.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := runner.Cleanup(ctx, project.Project.ID, "deleted"); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	done := make(chan error, 1)
	go func() { done <- dispatcher.New(c, runner).Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("dispatcher shutdown timed out")
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			mu.Lock()
			diagnostics := append([]string{}, modelErrors...)
			mu.Unlock()
			t.Fatalf("graph chain timed out; model errors: %v", diagnostics)
		case <-ticker.C:
			mu.Lock()
			diagnostics := append([]string{}, modelErrors...)
			mu.Unlock()
			if len(diagnostics) > 0 {
				t.Fatalf("controlled model assertions failed: %v", diagnostics)
			}
			var g board.Graph
			if err := client.Do(ctx, "GET", "/projects/"+project.Project.ID, nil, &g, nil); err != nil {
				t.Fatal(err)
			}
			if g.Project.Status != "completed" {
				continue
			}
			var state board.State
			if err := client.Do(ctx, "GET", "/projects/"+project.Project.ID+"/state", nil, &state, nil); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(modelErrors) > 0 || !observedMidTask || !completionReviewed || len(rootAssessments) > 0 && !rootAssessmentReviewed || executionRun == "" || len(decisionRuns) < 2 || len(state.Candidates) != 1 || state.Candidates[0].Status != "verified" {
				t.Fatalf("incomplete acceptance: errors=%v midway=%t reviewed=%t root_reviewed=%t decide=%d candidates=%+v", modelErrors, observedMidTask, completionReviewed, rootAssessmentReviewed, len(decisionRuns), state.Candidates)
			}
			runs, err := testExecutions(ctx, store, c.Container.Namespace)
			if err != nil {
				t.Fatal(err)
			}
			decided := false
			executed := false
			for _, run := range runs {
				if run.Status != "succeeded" {
					continue
				}
				if run.Kind == "reason" && strings.Contains(string(run.Result), `\"decided\":true`) {
					decided = true
				}
				if run.Kind == "explore" && run.ID == executionRun {
					executed = true
				}
			}
			if !decided || !executed {
				t.Fatalf("missing final protocol receipts: decided=%t execute=%t runs=%+v", decided, executed, runs)
			}
			return
		}
	}
}
