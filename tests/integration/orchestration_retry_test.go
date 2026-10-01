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
			switch stages[run] {
			case "":
				stages[run] = "read_steps"
				reply.call("read_graph", map[string]any{"section": "steps", "limit": 20})
			case "read_steps":
				text, err := lastResult()
				if err != nil {
					fail(err)
					return
				}
				var page struct {
					Items []board.Step `json:"items"`
				}
				if err := json.Unmarshal([]byte(text), &page); err != nil {
					fail(err)
					return
				}
				switch {
				case len(page.Items) == 0:
					stages[run] = "step_staged"
					reply.action("step", "fixture-step", map[string]any{"action": "add", "from": []string{"origin"}, "description": "Write the observed fixture status. If unavailable report incomplete; a new business attempt requires explicit main-Agent authorization."})
				case len(page.Items) == 1 && page.Items[0].Status == "failed":
					step := page.Items[0]
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
					reply.action("step", "retry-once", map[string]any{"action": "retry", "id": step.ID, "latest_run_id": step.LatestRunID, "reason": "Authorize exactly one fresh attempt after the fixture prerequisite changed."})
				case len(page.Items) == 1 && page.Items[0].Status == "completed" && page.Items[0].Result != nil:
					stages[run] = "complete_staged"
					reply.action("complete", "finish", map[string]any{"from": []string{*page.Items[0].Result}, "description": "The authorized successor completed with new evidence; the original failed attempt remains auditable."})
				default:
					fail(fmt.Errorf("unexpected Steps in scripted planner: %+v", page.Items))
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
	container := conf.Container.Namespace + "-dispatch-" + project.Project.ID
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

type businessRetryDockerRunner struct {
	*docker.Client
	t       *testing.T
	started time.Time
}

func (r *businessRetryDockerRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	if job.Kind == "explore" && job.PreviousRunID != "" {
		r.t.Logf("phase=retry_execute_started elapsed=%.3fs run=%s previous=%s model=local-scripted", time.Since(r.started).Seconds(), job.RunID, job.PreviousRunID)
	}
	result, err := r.Client.Run(ctx, backend, job)
	if job.Kind == "explore" && job.PreviousRunID == "" {
		r.t.Logf("phase=first_execute_returned elapsed=%.3fs status=%s failure=%s model=local-scripted", time.Since(r.started).Seconds(), result.Status, result.FailureKind)
	}
	return result, err
}
