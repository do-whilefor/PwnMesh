//go:build linux

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	"pwnmesh/internal/worker"
)

// The scripted endpoint corrupts the real provider stream, not a Runner
// result. The production Docker Worker must preserve the transport cause
// through same-run recovery and exhaustion before the scheduler may authorize
// one fresh curator. This fixture does not alter the live timing workload.
func TestDockerCurationInfrastructureRetry(t *testing.T) {
	const budget = 6
	var mu sync.Mutex
	var originalRun, namespace string
	requests, successorRequests := 0, 0
	hooks := curationDockerHooks{
		configure: func(c *config.Config) {
			c.Tasks.Curate.Timeout = budget
			namespace = c.Container.Namespace
		},
		beforeModel: func(w http.ResponseWriter, r *http.Request, job worker.Job) bool {
			if job.Kind != "curate" {
				return false
			}
			mu.Lock()
			if originalRun == "" {
				originalRun = job.RunID
			}
			if job.RunID != originalRun {
				successorRequests++
				mu.Unlock()
				return false
			}
			requests++
			attempt := requests
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n\n")
			w.(http.Flusher).Flush()
			if attempt > 1 {
				// The first response ends without message_stop. The next request
				// belongs to the resumed same run and reaches its original deadline.
				<-r.Context().Done()
			}
			return true
		},
		verify: func(store *board.Store, state board.State, files map[string][]byte) {
			mu.Lock()
			failedID, failedRequests, successfulRequests := originalRun, requests, successorRequests
			mu.Unlock()
			if failedID == "" || failedRequests < 2 || successfulRequests != 1 {
				t.Fatalf("provider did not exercise failed same-run recovery and one successful curator: failed=%d successor=%d", failedRequests, successfulRequests)
			}
			runs, err := testExecutions(context.Background(), store, namespace)
			if err != nil {
				t.Fatal(err)
			}
			var first, second board.Execution
			var producerStepID string
			curators, producers := 0, 0
			for _, run := range runs {
				switch run.Kind {
				case "curate":
					curators++
					if run.ID == failedID {
						first = run
					} else {
						second = run
					}
				case "explore":
					producers++
					producerStepID = run.Intent
				}
			}
			var firstJob, secondJob worker.Job
			if json.Unmarshal(first.Job, &firstJob) != nil || json.Unmarshal(second.Job, &secondJob) != nil {
				t.Fatal("missing persisted curator inputs")
			}
			if curators != 2 || first.Status != "retried" || second.Status != "succeeded" || first.ID == second.ID || secondJob.PreviousRunID != first.ID || firstJob.PreviousRunID != "" || first.RetryKey == "" || first.RetryKey != second.RetryKey || firstJob.InputSnapshot == nil || secondJob.InputSnapshot == nil || firstJob.InputSnapshot.StateVersion != secondJob.InputSnapshot.StateVersion {
				t.Fatalf("invalid one-use curator successor: count=%d first=%s/%s second=%s/%s previous=%s", curators, first.ID, first.Status, second.ID, second.Status, secondJob.PreviousRunID)
			}
			var terminal struct {
				Status       string `json:"status"`
				FailureKind  string `json:"failure_kind"`
				FailureCause string `json:"failure_cause"`
				Retryable    bool   `json:"retryable"`
			}
			if json.Unmarshal(first.Result, &terminal) != nil || terminal.Status != "failed" || terminal.FailureKind != "budget_exhausted" || terminal.FailureCause != "transport" || terminal.Retryable {
				t.Fatalf("terminal exhaustion lost its typed transport cause: %+v", terminal)
			}
			type savedSession struct {
				RunID          string    `json:"run_id"`
				RecoveryCount  int       `json:"recovery_count"`
				StartedAt      time.Time `json:"started_at"`
				ReasonDeadline time.Time `json:"reason_deadline"`
			}
			var failedSession, successfulSession savedSession
			prefix := "/workspace/.pwnmesh/runs/"
			if json.Unmarshal(files[prefix+first.ID+"/session.json"], &failedSession) != nil || json.Unmarshal(files[prefix+second.ID+"/session.json"], &successfulSession) != nil || failedSession.RunID != first.ID || successfulSession.RunID != second.ID || failedSession.RecoveryCount < 1 || successfulSession.RecoveryCount != 0 || !failedSession.ReasonDeadline.Equal(failedSession.StartedAt.Add(budget*time.Second)) || !successfulSession.StartedAt.After(failedSession.ReasonDeadline) {
				t.Fatal("recovery reset the original deadline or successor reused the exhausted session")
			}
			decoder := json.NewDecoder(bytes.NewReader(files[prefix+first.ID+"/events.jsonl"]))
			sawTransport, sawExhaustion := false, false
			for {
				var event struct {
					Type        string `json:"type"`
					FailureKind string `json:"failure_kind"`
					Retryable   bool   `json:"retryable"`
				}
				if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if event.Type == "result" {
					sawTransport = sawTransport || event.FailureKind == "transport" && event.Retryable
					sawExhaustion = sawExhaustion || event.FailureKind == "budget_exhausted" && !event.Retryable
				}
			}
			if !sawTransport || !sawExhaustion {
				t.Fatal("real Worker journal did not retain transport failure and terminal exhaustion")
			}
			if err := store.Do(context.Background(), func(tx *board.Tx) error {
				_, err := tx.CurationReceipt(state.Graph.Project.ID, first.Lease)
				var apiError *board.APIError
				if !errors.As(err, &apiError) || apiError.Status != 404 {
					t.Fatal("failed curator already had a committed receipt")
				}
				receipt, err := tx.CurationReceipt(state.Graph.Project.ID, second.Lease)
				if err != nil {
					return err
				}
				if !receipt.Committed || state.Curation.RunID != second.Lease {
					t.Fatal("successful successor did not own the final curation")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var businessSteps []board.Step
			markers := 0
			for _, step := range state.Steps {
				if staleRepairCompletionMarker(state, step) {
					markers++
				} else {
					businessSteps = append(businessSteps, step)
				}
			}
			if producers != 1 || len(businessSteps) != 1 || businessSteps[0].ID != producerStepID || businessSteps[0].Status != "completed" || markers > 1 || state.Graph.Project.Status != "completed" {
				t.Fatal("infrastructure recovery created business work or bypassed final completion")
			}
			t.Logf("phase=curator_infrastructure_successor failed_run=%s successor_run=%s original_session_recoveries=%d model=local-scripted", first.ID, second.ID, failedSession.RecoveryCount)
		},
	}
	runDockerCurationInfrastructureFixture(t, hooks)
}

// The local model controls protocol choices; Docker executes the production
// Go Loop, real file tools, graph bridge, curation transaction and completion.
func TestDockerCurationRelations(t *testing.T) {
	runDockerCurationRelations(t, false)
}

func TestDockerCurationSupport(t *testing.T) {
	runDockerCurationRelations(t, true)
}

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
		assessRoot := strings.Join(names, ",") == graphBridgeAssessRootTools
		decide := assessRoot || strings.Join(names, ",") == graphBridgeDecideTools
		if !decide && strings.Join(names, ",") != graphBridgeExploreTools {
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
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	runner := docker.New(c.Container)
	bindTestContainerOwner(t, runner, store)
	// Initialize before the Scheduler can issue requests to the model handler.
	runtimeContainer = runner.ContainerName(project.Project.ID)
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

func TestDockerInterruptedRunResumesWithoutRepeatingSideEffects(t *testing.T) {
	testDockerRunResumesWithoutRepeatingSideEffects(t, false)
}

func TestDockerWorkerSIGKILLResumesWithoutRepeatingSideEffects(t *testing.T) {
	testDockerRunResumesWithoutRepeatingSideEffects(t, true)
}

// The only credential is generated in the Dispatcher-side test process. Tool
// inputs contain its digest, never its value. Exercise the real Worker, file
// and shell tools, concurrent child Agents, model bridge and durable handoff.
func TestDockerModelCredentialsStayOutsideWorker(t *testing.T) {
	image := os.Getenv("PWNMESH_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set PWNMESH_DOCKER_TEST_IMAGE for real model credential isolation acceptance")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	token := "audit-" + hex.EncodeToString(random) + "-token"
	digest := sha256.Sum256([]byte(token))
	const projectID, runID = "credential-isolation", "isolation-run"
	const runDir = "/workspace/.pwnmesh/runs/" + runID
	const mainEvidence = runDir + "/isolation-main.json"
	probe := func(destination string) string {
		return modelIsolationProbe(hex.EncodeToString(digest[:]), destination)
	}

	var mu sync.Mutex
	turns := map[string]int{}
	var modelErrors []string
	readChecks, shellChecks, childStarts := 0, 0, 0
	childrenReady := make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(message string) {
			mu.Lock()
			modelErrors = append(modelErrors, message)
			mu.Unlock()
			http.Error(w, "controlled isolation assertion failed", http.StatusBadRequest)
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("x-api-key") != token {
			fail("Dispatcher model request did not carry the configured synthetic credential")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil || bytes.Contains(raw, []byte(token)) {
			fail("model request body unreadable or contains the synthetic credential")
			return
		}
		var input struct {
			Messages []agent.Message    `json:"messages"`
			Tools    []agent.Definition `json:"tools"`
		}
		if json.Unmarshal(raw, &input) != nil {
			fail("invalid model request JSON")
			return
		}
		session := r.Header.Get("x-opencode-session")
		child := session == runID+":isolation:left" || session == runID+":isolation:right"
		if session != runID && !child {
			fail("model request lost its root or child session identity")
			return
		}
		mu.Lock()
		turns[session]++
		turn := turns[session]
		if child && turn == 1 {
			childStarts++
			if childStarts == 2 {
				close(childrenReady)
			}
		}
		mu.Unlock()
		if child && turn == 1 {
			// A synchronous bridge would hold the first request while preventing
			// the second child from reaching the model. Require actual overlap.
			select {
			case <-childrenReady:
			case <-time.After(10 * time.Second):
				fail("concurrent child model requests did not overlap")
				return
			case <-r.Context().Done():
				fail("child model request cancelled before concurrent sibling arrived")
				return
			}
		}
		reply := scriptedModelReply{w: w, turn: turn}
		lastResult := func() (string, error) {
			return scriptedToolResult(input.Messages, fmt.Sprintf("call-%d", turn-1), false)
		}
		switch turn {
		case 1:
			reply.call("read", map[string]string{"path": "/proc/self/environ"})
		case 2:
			text, err := lastResult()
			if err != nil || strings.Contains(text, token) || strings.Contains(text, "ANTHROPIC_") || !strings.Contains(text, "PWNMESH_MODEL_BRIDGE=dispatcher-v1") {
				fail("read tool exposed provider environment or omitted bridge marker")
				return
			}
			mu.Lock()
			readChecks++
			mu.Unlock()
			destination := mainEvidence
			if child {
				destination = "isolation.json"
			}
			reply.call("bash", map[string]any{"command": probe(destination), "timeout": 30})
		case 3:
			text, err := lastResult()
			if err != nil || checkModelIsolationReceipt(text, true) != nil {
				fail("shell probe found credential/provider environment or failed to scan retained files")
				return
			}
			mu.Lock()
			shellChecks++
			mu.Unlock()
			if child {
				reply.respond(agent.Block{Type: "text", Text: "Credential isolation probe passed; original receipt is isolation.json."}, "end_turn")
				return
			}
			nodes := []map[string]any{}
			for _, id := range []string{"left", "right"} {
				nodes = append(nodes, map[string]any{"id": id, "kind": "agent", "task": "Read your process environment and run the supplied synthetic credential-isolation probe. Preserve its JSON receipt as isolation.json.", "resources": []string{}, "inputs": []any{}, "artifacts": []string{"isolation.json"}, "timeout": 45})
			}
			reply.call("run_graph", map[string]any{"key": "isolation", "parallelism": 2, "nodes": nodes})
		case 4:
			if child {
				fail("child Agent unexpectedly continued after its final account")
				return
			}
			text, err := lastResult()
			var graph struct {
				Status string `json:"status"`
				Nodes  []struct {
					ID, Status string
				} `json:"nodes"`
			}
			if err != nil || json.Unmarshal([]byte(text), &graph) != nil || graph.Status != "succeeded" || len(graph.Nodes) != 2 {
				fail("concurrent child graph did not complete successfully")
				return
			}
			seen := map[string]bool{}
			for _, node := range graph.Nodes {
				if (node.ID != "left" && node.ID != "right") || node.Status != "succeeded" || seen[node.ID] {
					fail("child graph returned an unexpected node result")
					return
				}
				seen[node.ID] = true
			}
			reply.call("finish_step", map[string]any{"fact": map[string]any{
				"description": "The main Worker and two concurrent child Agents completed synthetic credential-isolation probes without observing the Dispatcher credential.",
				"scope":       "Local synthetic credential-isolation fixture only", "observed_at": time.Now().UTC().Format(time.RFC3339Nano),
				"evidence": []map[string]string{{"path": mainEvidence}},
			}})
		default:
			fail("unexpected additional model request")
		}
	}))
	defer model.Close()

	c := config.Container{Socket: "/var/run/docker.sock", Image: image, Network: testContainerNetwork(t), Namespace: fmt.Sprintf("pwnmesh-isolation-%d", time.Now().UnixNano()), CompletedAction: "stop"}
	runner := docker.New(c)
	bindTestContainerOwner(t, runner, nil)
	defer runner.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := runner.Cleanup(ctx, projectID, "deleted"); err != nil {
			t.Error(err)
		}
	}()
	runner.SetGraphHandler(func(_ context.Context, j worker.Job, request worker.GraphRequest) (any, error) {
		if request.Op != "read_updates" {
			return nil, fmt.Errorf("unexpected isolation fixture graph request %s", request.Op)
		}
		cursor := board.ExecuteUpdateCursor{ProjectID: projectID, StepID: j.Intent.ID, RunID: runID}
		if request.Updates != nil {
			cursor = *request.Updates
		}
		return board.ExecuteUpdates{Version: 1, ExecuteUpdateCursor: cursor, FromRevision: cursor.Revision, ToRevision: cursor.Revision, StateVersion: strings.Repeat("a", 64), Complete: true}, nil
	})
	intent := board.Intent{ID: "i001", From: []string{"origin"}, Description: "Verify synthetic model credential isolation in the main Worker and concurrent child Agents, retaining the probe receipt."}
	job := worker.Job{RunID: runID, Kind: "explore", WorkerType: "go", GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", Budget: config.Task{Timeout: 120, ConcludeTimeout: 10}, Intent: &intent,
		Graph: board.Graph{Project: board.Project{ID: projectID, OrchestrationVersion: 1, Title: "Synthetic model credential isolation", Status: "active"}, Facts: []board.Fact{{ID: "origin", Description: "Local synthetic test only"}, {ID: "goal", Description: "Keep provider credentials outside all Worker tools"}}, Intents: []board.Intent{intent}}}
	backend := config.Worker{Name: "controlled", Type: "go", Env: map[string]string{
		"ANTHROPIC_BASE_URL": model.URL, "ANTHROPIC_AUTH_TOKEN": token, "ANTHROPIC_API_KEY": token, "ANTHROPIC_CUSTOM_SECRET": token,
		"ANTHROPIC_MODEL": "controlled", "PWNMESH_REQUEST_TIMEOUT": "30",
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	result, runErr := runner.Run(ctx, backend, job)
	mu.Lock()
	diagnostics := append([]string{}, modelErrors...)
	requests, reads, shells, children := turns[runID], readChecks, shellChecks, childStarts
	mu.Unlock()
	if len(diagnostics) != 0 {
		t.Fatalf("model isolation assertions failed: %v", diagnostics)
	}
	if runErr != nil || result.Status != "success" || result.Conclude || !strings.Contains(result.Text, `"outcome":"completed"`) {
		t.Fatalf("real Worker did not finish its durable evidence handoff: status=%s error=%v", result.Status, runErr)
	}
	if strings.Contains(result.Text, token) || requests != 4 || reads != 3 || shells != 3 || children != 2 {
		t.Fatalf("incomplete isolation checks: parent_requests=%d reads=%d shells=%d child_sessions=%d", requests, reads, shells, children)
	}
	// Repeat after final session, model response, child receipts and accepted
	// graph result have all been persisted. The exec receives only the digest.
	text, err := dockerExec(ctx, runner.ContainerName(projectID), []string{"bash", "-c", probe("/tmp/isolation-final.json")})
	if err != nil {
		t.Fatal("final retained-file probe failed", err)
	}
	if err := checkModelIsolationReceipt(text, false); err != nil {
		t.Fatal(err)
	}
}

// No upstream forwarding is implemented: both reserved .invalid destinations
// exist only in this local proxy. Without proxy routing, the test cannot pass.
func TestDockerModelAndWorkerUseSameConfiguredProxy(t *testing.T) {
	image := os.Getenv("PWNMESH_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set PWNMESH_DOCKER_TEST_IMAGE for real model and worker proxy acceptance")
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var bindIP net.IP
	for _, address := range addresses {
		ip, _, parseErr := net.ParseCIDR(address.String())
		if parseErr == nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			bindIP = ip.To4()
			break
		}
	}
	if bindIP == nil {
		t.Fatal("proxy acceptance requires a non-loopback IPv4 interface")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(bindIP.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	const projectID = "proxy-project"
	const runID = "proxy-run"
	const runDir = "/workspace/.pwnmesh/runs/" + runID
	const credential = "synthetic-proxy-integration-credential"
	const marker = "PWNMESH_SYNTHETIC_PROXY_OK"
	var mu sync.Mutex
	modelCalls, workerCalls := 0, 0
	var failures []string
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !r.URL.IsAbs() || r.URL.Scheme != "http" {
			failures = append(failures, "request was not addressed through an HTTP forward proxy")
			http.Error(w, "absolute proxy URL required", 400)
			return
		}
		switch r.URL.Host {
		case "worker.example.invalid":
			workerCalls++
			if r.Method != "GET" || r.URL.Path != "/check" || r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
				failures = append(failures, "worker request had the wrong target or included model authentication")
				http.Error(w, "invalid synthetic worker request", 400)
				return
			}
			_, _ = fmt.Fprintln(w, marker)
		case "model.example.invalid":
			modelCalls++
			if r.Method != "POST" || r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != credential || r.Header.Get("anthropic-version") != "2023-06-01" {
				failures = append(failures, "model request did not use the configured Messages protocol")
				http.Error(w, "invalid synthetic model request", 400)
				return
			}
			var payload struct {
				Messages []agent.Message    `json:"messages"`
				Tools    []agent.Definition `json:"tools"`
				Model    string             `json:"model"`
				Stream   bool               `json:"stream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Model != "proxy-fixture" || !payload.Stream {
				failures = append(failures, "invalid model payload")
				http.Error(w, "invalid synthetic payload", 400)
				return
			}
			respond := func(block agent.Block, stop string) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"role": "assistant", "content": []agent.Block{block}, "stop_reason": stop})
			}
			switch modelCalls {
			case 1:
				hasBash := false
				for _, tool := range payload.Tools {
					hasBash = hasBash || tool.Name == "bash"
				}
				if !hasBash {
					failures = append(failures, "worker did not advertise bash")
				}
				// The credential never appears in a tool command or worker input.
				command := "set -eu; env > " + runDir + "/worker-env.txt; curl --fail --silent --show-error --max-time 10 http://worker.example.invalid/check > " + runDir + "/proxy-marker.txt; cat " + runDir + "/proxy-marker.txt"
				input, _ := json.Marshal(map[string]any{"command": command, "timeout": 15})
				respond(agent.Block{Type: "tool_use", ID: "proxy-curl", Name: "bash", Input: input}, "tool_use")
			case 2:
				validReceipt := false
				if len(payload.Messages) > 0 {
					for _, block := range payload.Messages[len(payload.Messages)-1].Content {
						if block.Type == "tool_result" && block.ToolUseID == "proxy-curl" && !block.IsError && strings.Contains(string(block.Content), marker) {
							validReceipt = true
						}
					}
				}
				if !validReceipt || workerCalls != 1 {
					failures = append(failures, "model did not receive the successful proxied curl result")
				}
				result, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": map[string]any{
					"description": "A synthetic worker HTTP request reached the configured project proxy.", "scope": "local proxy fixture", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence": []map[string]string{{"path": runDir + "/proxy-marker.txt"}},
				}}})
				respond(agent.Block{Type: "text", Text: string(result)}, "end_turn")
			default:
				failures = append(failures, "unexpected additional model request")
				http.Error(w, "unexpected synthetic model request", 400)
			}
		default:
			failures = append(failures, "unexpected destination; no forwarding is permitted")
			http.Error(w, "unknown synthetic destination", 400)
		}
	}))
	_ = proxy.Listener.Close()
	proxy.Listener = listener
	proxy.Start()
	defer proxy.Close()
	c := config.Container{Socket: "/var/run/docker.sock", Image: image, Network: testContainerNetwork(t), Namespace: fmt.Sprintf("pwnmesh-proxy-%d", time.Now().UnixNano()), CompletedAction: "stop"}
	runner := docker.New(c)
	bindTestContainerOwner(t, runner, nil)
	defer runner.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := runner.Cleanup(ctx, projectID, "deleted"); err != nil {
			t.Error(err)
		}
	}()
	runner.SetGraphHandler(func(_ context.Context, j worker.Job, r worker.GraphRequest) (any, error) {
		if r.Op != "read_updates" {
			return nil, fmt.Errorf("unexpected proxy fixture graph request %s", r.Op)
		}
		cursor := board.ExecuteUpdateCursor{ProjectID: projectID, StepID: j.Intent.ID, RunID: runID}
		if r.Updates != nil {
			cursor = *r.Updates
		}
		return board.ExecuteUpdates{Version: 1, ExecuteUpdateCursor: cursor, FromRevision: cursor.Revision, ToRevision: cursor.Revision, StateVersion: strings.Repeat("a", 64), Complete: true}, nil
	})
	intent := board.Intent{ID: "proxy-step", From: []string{"origin"}, Description: "Request the local synthetic HTTP fixture through the configured proxy and retain its marker."}
	job := worker.Job{RunID: runID, Kind: "explore", WorkerType: "go", GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", Budget: config.Task{Timeout: 45, ConcludeTimeout: 10}, Intent: &intent, Graph: board.Graph{
		Project: board.Project{ID: projectID, OrchestrationVersion: 1, Title: "Synthetic proxy acceptance", Status: "active"},
		Facts:   []board.Fact{{ID: "origin", Description: "Local synthetic proxy only; do not contact any real service."}, {ID: "goal", Description: "Verify model and worker routing through one proxy."}}, Intents: []board.Intent{intent},
	}}
	backend := config.Worker{Name: "proxy-controlled", Type: "go", Env: map[string]string{"ANTHROPIC_BASE_URL": "http://model.example.invalid", "ANTHROPIC_AUTH_TOKEN": credential, "ANTHROPIC_MODEL": "proxy-fixture", "PWNMESH_CONNECTION_MODE": "proxy", "PWNMESH_PROXY_URL": proxy.URL, "PWNMESH_REQUEST_TIMEOUT": "15", "PWNMESH_MAX_OUTPUT_TOKENS": "2048"}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, backend, job)
	if err != nil || result.Status != "success" || result.Conclude {
		t.Fatalf("proxied worker failed: status=%s conclude=%v error=%v", result.Status, result.Conclude, err)
	}
	container := runner.ContainerName(projectID)
	environment, err := dockerExec(ctx, container, []string{"cat", runDir + "/worker-env.txt"})
	if err != nil {
		t.Fatal("cannot inspect synthetic worker environment", err)
	}
	if strings.Contains(environment, credential) || strings.Contains(environment, "ANTHROPIC_") {
		t.Fatal("model credentials or provider configuration entered the worker environment")
	}
	values := map[string]string{}
	for _, line := range strings.Split(environment, "\n") {
		key, value, _ := strings.Cut(line, "=")
		values[key] = value
	}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if values[key] != proxy.URL {
			t.Errorf("worker %s does not use the configured proxy", key)
		}
	}
	proof, err := dockerExec(ctx, container, []string{"cat", runDir + "/proxy-marker.txt"})
	if err != nil || strings.TrimSpace(proof) != marker {
		t.Fatal("worker did not preserve the synthetic proxy response")
	}
	mu.Lock()
	defer mu.Unlock()
	if modelCalls != 2 || workerCalls != 1 || len(failures) > 0 {
		t.Fatalf("proxy verification: model=%d worker=%d failures=%v", modelCalls, workerCalls, failures)
	}
}

// This is a local scripted-model acceptance test of the real Go Worker,
// graph bridge and Docker lifecycle, not a real-model performance baseline.
func TestDockerOrchestrationBusinessRetry(t *testing.T) {
	image := os.Getenv("PWNMESH_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set PWNMESH_DOCKER_TEST_IMAGE for real Worker business-retry acceptance")
	}
	started := time.Now()
	store, err := board.Open(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := httptest.NewServer(server.New(store))
	defer api.Close()
	client := &dispatcher.Client{Base: api.URL}
	var project board.Graph
	if err := client.Do(context.Background(), "POST", "/projects", map[string]any{"title": "Controlled business retry", "origin": "Use only local synthetic files; preserve each attempt's evidence.", "goal": "Complete the same Step after one main-Agent-authorized retry.", "bootstrap_enabled": false, "orchestration_version": 1}, &project, nil); err != nil {
		t.Fatal(err)
	}

	const failedProof = "attempt-one: fixture prerequisite unavailable\n"
	const successfulProof = "attempt-two: fixture prerequisite available\n"
	modelErrors := make(chan error, 16)
	retryReady, authorize := make(chan string, 1), make(chan struct{})
	var mu sync.Mutex
	turns, stages := map[string]int{}, map[string]string{}
	plannerSteps := map[string][]board.Step{}
	assessments := map[string]board.RootAssessment{}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fail := func(err error) {
			select {
			case modelErrors <- err:
			default:
			}
			http.Error(w, "controlled retry model assertion failed", http.StatusBadRequest)
		}
		var request struct {
			Messages []agent.Message `json:"messages"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&request); err != nil {
			fail(err)
			return
		}
		run := r.Header.Get("x-opencode-session")
		job, err := scriptedJob(r.Context(), store, project.Project.ID, run)
		if err != nil {
			fail(err)
			return
		}
		turns[run]++
		reply := scriptedModelReply{w: w, turn: turns[run]}
		lastResult := func() (string, error) {
			return scriptedToolResult(request.Messages, fmt.Sprintf("call-%d", turns[run]-1), false)
		}
		switch job.Kind {
		case "curate":
			if turns[run] != 1 {
				fail(errors.New("curator continued after its committed receipt"))
				return
			}
			reply.action("curate", "curate", map[string]any{"groups": []any{}})
		case "reason":
			closure := job.Decision != nil && job.Decision.ClosureProtocol == 1
			stageStep := func(key string, payload map[string]any) {
				input := map[string]any{"op": "step", "idempotency_key": key, "payload": payload}
				if closure {
					input["gap_id"] = "business_retry"
				}
				reply.call("graph_action", input)
			}
			switch stages[run] {
			case "":
				stages[run] = "read_steps"
				reply.call("read_graph", map[string]any{"section": "steps", "limit": 20})
			case "read_steps", "assessed_steps":
				text, err := lastResult()
				if err != nil {
					fail(err)
					return
				}
				if stages[run] == "read_steps" {
					var page struct {
						Items []board.Step `json:"items"`
					}
					if err := json.Unmarshal([]byte(text), &page); err != nil {
						fail(err)
						return
					}
					plannerSteps[run] = page.Items
					if closure {
						assessment := board.RootAssessment{
							Status: "missing", From: []string{},
							Description: "The requested Step has not completed through an authorized successful attempt.",
							Gaps:        []board.RequirementGap{{ID: "business_retry", InputIDs: []string{"goal"}, Description: "Complete the same Step, explicitly authorizing a fresh attempt if the fixture prerequisite was unavailable."}},
						}
						switch {
						case len(page.Items) == 0, len(page.Items) == 1 && page.Items[0].Status == "failed":
						case len(page.Items) == 1 && page.Items[0].Status == "completed" && page.Items[0].Result != nil:
							assessment = board.RootAssessment{Status: "satisfied", From: []string{*page.Items[0].Result}, Description: "The authorized successor completed with new evidence; the original failed attempt remains auditable."}
						default:
							fail(fmt.Errorf("unexpected Steps in root assessment: %+v", page.Items))
							return
						}
						assessments[run] = assessment
						stages[run] = "assessed_steps"
						reply.call("assess_root", assessment)
						return
					}
				} else {
					// Observe the assessment in a separate model request before
					// staging any new work or project completion.
					var receipt struct {
						Assessment board.RootAssessment `json:"assessment"`
					}
					if json.Unmarshal([]byte(text), &receipt) != nil || !reflect.DeepEqual(receipt.Assessment, assessments[run]) {
						fail(fmt.Errorf("missing original-requirement assessment: %s", text))
						return
					}
				}
				steps := plannerSteps[run]
				switch {
				case len(steps) == 0:
					stages[run] = "step_staged"
					stageStep("fixture-step", map[string]any{"action": "add", "from": []string{"origin"}, "description": "Write the observed fixture status. If unavailable report incomplete; a new business attempt requires explicit main-Agent authorization."})
				case len(steps) == 1 && steps[0].Status == "failed":
					step := steps[0]
					if !board.ValidExecutionID(step.LatestRunID) {
						fail(errors.New("read_graph omitted latest_run_id of failed Step"))
						return
					}
					retryReady <- step.LatestRunID
					select {
					case <-authorize:
					case <-r.Context().Done():
						return
					}
					stages[run] = "retry_staged"
					stageStep("retry-once", map[string]any{"action": "retry", "id": step.ID, "latest_run_id": step.LatestRunID, "reason": "Authorize exactly one fresh attempt after the fixture prerequisite changed."})
				case len(steps) == 1 && steps[0].Status == "completed" && steps[0].Result != nil:
					stages[run] = "complete_staged"
					reply.action("complete", "finish", map[string]any{"from": []string{*steps[0].Result}, "description": "The authorized successor completed with new evidence; the original failed attempt remains auditable."})
				default:
					fail(fmt.Errorf("unexpected Steps in scripted planner: %+v", steps))
				}
			case "step_staged", "retry_staged", "complete_staged":
				text, err := lastResult()
				if err != nil {
					fail(err)
					return
				}
				var draft struct {
					Draft bool   `json:"draft"`
					Op    string `json:"op"`
				}
				if json.Unmarshal([]byte(text), &draft) != nil || !draft.Draft {
					fail(fmt.Errorf("missing private decision draft: %s", text))
					return
				}
				if stages[run] == "complete_staged" {
					stages[run] = "completion_review"
					reply.action("preview", "preview", map[string]any{})
				} else {
					if stages[run] == "retry_staged" {
						t.Logf("phase=retry_authorization_commit_requested elapsed=%.3fs model=local-scripted", time.Since(started).Seconds())
					}
					stages[run] = "committed"
					reply.action("commit", "commit", map[string]any{})
				}
			case "completion_review":
				text, err := lastResult()
				if err != nil {
					fail(err)
					return
				}
				var preview board.DecisionReceipt
				if json.Unmarshal([]byte(text), &preview) != nil || preview.Committed || preview.CompletionReview == nil {
					fail(fmt.Errorf("missing completion review: %s", text))
					return
				}
				stages[run] = "committed"
				reply.action("commit", "commit", map[string]any{})
			default:
				fail(errors.New("planner continued after commit"))
			}
		case "explore":
			proof := failedProof
			if job.PreviousRunID != "" {
				proof = successfulProof
			}
			name := "/workspace/.pwnmesh/runs/" + run + "/output-business-proof.txt"
			switch turns[run] {
			case 1:
				reply.call("write", map[string]any{"path": name, "content": proof})
			case 2:
				if _, err := lastResult(); err != nil {
					fail(err)
					return
				}
				reply.action("fact", "attempt-observation", map[string]any{"description": strings.TrimSpace(proof), "scope": "Synthetic local retry fixture", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence": []map[string]any{{"path": name, "start_line": 1, "end_line": 1}}})
			case 3:
				text, err := lastResult()
				if err != nil {
					fail(err)
					return
				}
				var receipt board.StateActionResult
				if json.Unmarshal([]byte(text), &receipt) != nil || receipt.Op != "fact" || receipt.ID == "" {
					fail(fmt.Errorf("missing original observation receipt: %s", text))
					return
				}
				result := map[string]any{"accepted": true, "outcome": "incomplete", "reason": "Fixture prerequisite unavailable; published observation is retained. A new business attempt needs main-Agent authorization."}
				if job.PreviousRunID != "" {
					result = map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact_id": receipt.ID}}
				}
				raw, _ := json.Marshal(result)
				reply.respond(agent.Block{Type: "text", Text: string(raw)}, "end_turn")
			default:
				fail(fmt.Errorf("unexpected business-attempt turn %d", turns[run]))
			}
		default:
			fail(fmt.Errorf("unexpected role %s", job.Kind))
		}
	}))
	defer model.Close()
	conf := config.Config{Server: api.URL, Runtime: config.Runtime{Interval: 1, MaxWorkers: 2, MaxProjects: 1, MaxProjectWorkers: 2, HealthMode: "disabled", HealthTimeout: 10}, Tasks: config.Tasks{Reason: config.Task{Timeout: 30, MaxIntents: 1}, Curate: config.Task{Timeout: 30}, Explore: config.Task{Timeout: 30, ConcludeTimeout: 5}}, Container: config.Container{Image: image, Network: testContainerNetwork(t), Namespace: fmt.Sprintf("pwnmesh-business-retry-%d", time.Now().UnixNano()), CompletedAction: "stop"}, Workers: []config.Worker{{Name: "controlled", Type: "go", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: 2, Env: map[string]string{"ANTHROPIC_BASE_URL": model.URL, "ANTHROPIC_AUTH_TOKEN": "controlled-test-only", "ANTHROPIC_MODEL": "controlled", "PWNMESH_REQUEST_TIMEOUT": "10"}}}}
	if err := conf.Validate(); err != nil {
		t.Fatal(err)
	}
	runner := &businessRetryDockerRunner{Client: docker.New(conf.Container), t: t, started: started}
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
	go func() { done <- dispatcher.New(conf, runner).Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("retry dispatcher shutdown timed out")
		}
	}()
	var originalRun string
	select {
	case originalRun = <-retryReady:
	case err := <-modelErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("main Agent did not observe business failure", ctx.Err())
	}
	// A planner is waiting, but a second runtime slot remains free. More than
	// two scheduling ticks cannot launch another Execute without its commit.
	wait := time.NewTimer(2200 * time.Millisecond)
	defer wait.Stop()
	select {
	case <-wait.C:
	case err := <-modelErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	runs, err := testExecutions(ctx, store, conf.Container.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	executions := 0
	for _, run := range runs {
		if run.Kind == "explore" {
			executions++
			if run.ID != originalRun || run.Status != "failed" {
				t.Fatalf("unauthorized execution before retry decision: %s %s", run.ID, run.Status)
			}
		}
	}
	if executions != 1 {
		t.Fatalf("expected exactly one failed execution before authorization; got %d", executions)
	}
	close(authorize)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var state board.State
	for state.Graph.Project.Status != "completed" {
		select {
		case err := <-modelErrors:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("authorized retry did not complete", ctx.Err())
		case <-tick.C:
		}
		if err := client.Do(ctx, "GET", "/projects/"+project.Project.ID+"/state", nil, &state, nil); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("phase=project_completed_observed elapsed=%.3fs model=local-scripted", time.Since(started).Seconds())
	runs, err = testExecutions(ctx, store, conf.Container.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	var first, second board.Execution
	var firstJob, secondJob worker.Job
	executions = 0
	for _, run := range runs {
		if run.Kind == "explore" {
			executions++
			if run.ID == originalRun {
				first = run
				_ = json.Unmarshal(run.Job, &firstJob)
			} else {
				second = run
				_ = json.Unmarshal(run.Job, &secondJob)
			}
		}
	}
	if executions != 2 || first.Status != "retried" || second.Status != "succeeded" || first.Intent != second.Intent || first.ID == second.ID || firstJob.PreviousRunID != "" || secondJob.PreviousRunID != first.ID || firstJob.InputSnapshot == nil || secondJob.InputSnapshot == nil || firstJob.InputSnapshot.ID == secondJob.InputSnapshot.ID {
		t.Fatalf("invalid successor chain: executions=%d first=%s/%s second=%s/%s previous=%s", executions, first.ID, first.Status, second.ID, second.Status, secondJob.PreviousRunID)
	}
	var failure worker.Result
	if json.Unmarshal(first.Result, &failure) != nil || failure.Status != "failed" || failure.FailureKind != "incomplete" || failure.Retryable || !strings.Contains(failure.Text, `"outcome":"incomplete"`) {
		t.Fatalf("lost original nonretryable business failure: %+v", failure)
	}
	proofs := map[string]string{first.Lease: failedProof, second.Lease: successfulProof}
	retained := map[string]bool{}
	container := runner.ContainerName(project.Project.ID)
	for _, fact := range state.FactRecords {
		proof, exists := proofs[fact.RunID]
		if !exists {
			continue
		}
		if fact.SourceStepID != first.Intent || fact.Status != "valid" || len(fact.Evidence) != 1 {
			t.Fatalf("attempt lost its original observation: %+v", fact)
		}
		ref := fact.Evidence[0]
		if ref.RunID != orchestrationRunID(fact.RunID) || ref.Excerpt != proof {
			t.Fatalf("attempt evidence identity changed: %+v", ref)
		}
		// A completed container may be stopped already; the Docker archive API
		// keeps its immutable workspace readable without restarting the project.
		retained[fact.RunID] = true
	}
	files, err := collectLiveWorkspace(ctx, container, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range state.FactRecords {
		if proof, ok := proofs[fact.RunID]; ok {
			if !orchestrationEvidenceValid(fact.Evidence[0], fact.RunID, files) || string(files[fact.Evidence[0].Path]) != proof {
				t.Fatal("original attempt evidence bytes or hash changed")
			}
		}
	}
	if len(retained) != 2 {
		t.Fatal("failed and successful observations were not both retained")
	}
	var events []board.StateEvent
	if err := client.Do(ctx, "GET", "/projects/"+project.Project.ID+"/state/events?after=0", nil, &events, nil); err != nil {
		t.Fatal(err)
	}
	grants := 0
	for _, event := range events {
		if event.Op == "step" {
			var payload struct {
				Action      string `json:"action"`
				LatestRunID string `json:"latest_run_id"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Action == "retry" {
				grants++
				if event.ID != first.Intent || payload.LatestRunID != first.ID {
					t.Fatal("retry event authorized a different Step or attempt")
				}
			}
		}
	}
	if grants != 1 {
		t.Fatalf("expected one persisted retry authorization; got %d", grants)
	}
	select {
	case err := <-modelErrors:
		t.Fatal(err)
	default:
	}
}

// A new observation must not kill a refreshable planner stream, but it must
// still reject the old CAS version. The planner's original deadline eventually
// cancels only its model request; a sibling Execute retains its own process.
func TestDockerDecisionSurvivesNewFactsUntilDeadlineWithoutStoppingExecute(t *testing.T) {
	image := os.Getenv("PWNMESH_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set PWNMESH_DOCKER_TEST_IMAGE for Decide refresh/deadline acceptance")
	}
	store, err := board.Open(filepath.Join(t.TempDir(), "stale.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := httptest.NewServer(server.New(store))
	defer api.Close()
	client := &dispatcher.Client{Base: api.URL}
	var project board.Graph
	if err := client.Do(context.Background(), "POST", "/projects", map[string]any{
		"title": "Refreshable Decide deadline", "origin": "Use local synthetic files only.",
		"goal": "Preserve a sibling Execute while a planner retains its original deadline.", "bootstrap_enabled": false,
	}, &project, nil); err != nil {
		t.Fatal(err)
	}
	base := "/projects/" + project.Project.ID
	authorizeIntegrationStep(t, client, project, "Write and retain the synthetic sibling proof.")
	decideStarted, executeStarted := make(chan string, 1), make(chan string, 1)
	staleDisconnected, siblingDisconnected := make(chan struct{}, 1), make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var mu sync.Mutex
	turns := map[string]int{}
	staleRun := ""
	var modelErrors []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []agent.Definition `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid controlled request", http.StatusBadRequest)
			return
		}
		run := r.Header.Get("x-opencode-session")
		decide := true
		for _, tool := range request.Tools {
			if tool.Name == "bash" {
				decide = false
			}
		}
		mu.Lock()
		turns[run]++
		turn := turns[run]
		if decide && staleRun == "" {
			staleRun = run
		}
		stale := decide && run == staleRun
		mu.Unlock()
		reply := scriptedModelReply{w: w, turn: turn}
		if stale {
			if turn == 1 {
				// Send response headers and one valid SSE event, then hold the body
				// open. Cancellation must close a request already being consumed.
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n\n")
				w.(http.Flusher).Flush()
				decideStarted <- run
				<-r.Context().Done()
				staleDisconnected <- struct{}{}
				return
			}
			mu.Lock()
			modelErrors = append(modelErrors, "stale Decide made another model request")
			mu.Unlock()
			http.Error(w, "stale request replayed", http.StatusBadRequest)
			return
		}
		if decide {
			// Successors are outside this fixture's acceptance question. Refuse
			// additional planning without depending on an older tool contract.
			reply.respond(agent.Block{Type: "text", Text: `{"accepted":false,"reason":"This controlled fixture only exercises the first planner deadline."}`}, "end_turn")
			return
		}
		path := "/workspace/.pwnmesh/runs/" + run + "/output-sibling.txt"
		switch turn {
		case 1:
			executeStarted <- run
			select {
			case <-release:
			case <-r.Context().Done():
				siblingDisconnected <- struct{}{}
				return
			}
			reply.call("write", map[string]string{"path": path, "content": "sibling-proof-preserved\n"})
		case 2:
			raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{
				"fact": map[string]any{"description": "The synthetic sibling file was written after the planner reached its original deadline.",
					"scope": "local synthetic fixture", "observed_at": time.Now().UTC().Format(time.RFC3339),
					"evidence": []map[string]string{{"path": path}}},
			}})
			reply.respond(agent.Block{Type: "text", Text: string(raw)}, "end_turn")
		default:
			mu.Lock()
			modelErrors = append(modelErrors, fmt.Sprintf("unexpected Execute model turn %d", turn))
			mu.Unlock()
			http.Error(w, "unexpected Execute continuation", http.StatusBadRequest)
		}
	}))
	defer model.Close()
	defer releaseOnce.Do(func() { close(release) })
	c := config.Config{
		Server:    api.URL,
		Runtime:   config.Runtime{Interval: 1, MaxWorkers: 2, MaxProjects: 1, MaxProjectWorkers: 2, HealthMode: "disabled", HealthTimeout: 10},
		Tasks:     config.Tasks{Reason: config.Task{Timeout: 20, MaxIntents: 3}, Explore: config.Task{Timeout: 0, ConcludeTimeout: 10}},
		Container: config.Container{Image: image, Network: testContainerNetwork(t), Namespace: fmt.Sprintf("pwnmesh-stale-%d", time.Now().UnixNano()), CompletedAction: "stop"},
		Workers: []config.Worker{{Name: "controlled", Type: "go", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: 2,
			Env: map[string]string{"ANTHROPIC_BASE_URL": model.URL, "ANTHROPIC_AUTH_TOKEN": "controlled-test-only", "ANTHROPIC_MODEL": "controlled", "PWNMESH_REQUEST_TIMEOUT": "120"}}},
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
			t.Error("scheduler shutdown timed out")
		}
	}()
	awaitRun := func(ch <-chan string) string {
		t.Helper()
		select {
		case run := <-ch:
			return run
		case <-ctx.Done():
			t.Fatal("model request did not start")
			return ""
		}
	}
	executeID := awaitRun(executeStarted)
	if err := client.Do(ctx, "POST", base+"/hints", map[string]string{"content": "Review the synthetic plan while its Step is running.", "creator": "fixture"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	staleID := awaitRun(decideStarted)
	// Seed an independent retained observation, without ending the sibling's
	// in-flight request or simulating the change with a user hint.
	if err := store.Do(ctx, func(tx *board.Tx) error {
		current, err := tx.Load(project.Project.ID)
		if err != nil {
			return err
		}
		current.Facts = append(current.Facts, board.Fact{ID: "concurrent-observation", Description: "Independent synthetic producer observed another response."})
		return tx.Save(current)
	}); err != nil {
		t.Fatal(err)
	}
	// Wait through two heartbeat intervals while both streams are held open.
	select {
	case <-staleDisconnected:
		t.Fatal("new shared evidence cancelled the refreshable planner stream")
	case <-siblingDisconnected:
		t.Fatal("new shared evidence cancelled the independent Execute stream")
	case <-time.After(2200 * time.Millisecond):
	}
	readRuns := func() []board.Execution {
		t.Helper()
		runs, err := testExecutions(ctx, store, c.Container.Namespace)
		if err != nil {
			t.Fatal(err)
		}
		return runs
	}
	waitFor := func(check func([]board.Execution) bool, failure string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !check(readRuns()) {
			if time.Now().After(deadline) {
				t.Fatal(failure)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	var original board.Execution
	for _, run := range readRuns() {
		if run.ID == staleID {
			original = run
		}
	}
	var originalJob worker.Job
	if original.Status != "running" || json.Unmarshal(original.Job, &originalJob) != nil || originalJob.Decision == nil {
		t.Fatal("the original planner did not retain its running execution and immutable input")
	}
	// Use the production Board transaction to check CAS while the model is
	// deliberately blocked and unable to issue its own graph_action.
	if err := store.Do(ctx, func(tx *board.Tx) error {
		_, err := tx.CommitDecision(project.Project.ID, board.ExecutionFence{Run: original.Lease, Lease: "reason"}, board.DecisionBatch{ExpectedVersion: originalJob.Decision.StateVersion, Actions: []board.DecisionAction{}})
		var conflict *board.APIError
		if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict || !strings.HasPrefix(fmt.Sprint(conflict.Detail), "state_changed:") {
			return fmt.Errorf("old decision CAS was not rejected: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-staleDisconnected:
	case <-time.After(25 * time.Second):
		t.Fatal("planner did not cancel its model stream at the original task deadline")
	}
	waitFor(func(runs []board.Execution) bool {
		for _, run := range runs {
			if run.ID != staleID || run.Status != "failed" {
				continue
			}
			var result worker.Result
			if json.Unmarshal(run.Result, &result) != nil || result.FailureKind != "budget_exhausted" || result.Retryable {
				t.Fatalf("planner did not preserve the original budget boundary: %s", run.Result)
			}
			if string(run.Job) != string(original.Job) {
				t.Fatal("concurrent facts rewrote the planner's immutable input")
			}
			return true
		}
		return false
	}, "planner deadline did not settle its failed execution")
	select {
	case <-siblingDisconnected:
		t.Fatal("planner deadline disconnected the sibling Execute request")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	waitFor(func(runs []board.Execution) bool {
		for _, run := range runs {
			if run.ID == executeID && run.Status == "succeeded" {
				return true
			}
		}
		return false
	}, "sibling Execute failed to complete after the planner deadline")
	mu.Lock()
	defer mu.Unlock()
	if turns[staleID] != 1 || turns[executeID] != 2 || len(modelErrors) != 0 {
		t.Fatalf("unexpected model continuation: stale=%d execute=%d errors=%v", turns[staleID], turns[executeID], modelErrors)
	}
}
