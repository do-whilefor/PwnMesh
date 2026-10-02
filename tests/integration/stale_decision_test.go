//go:build linux

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		Workers: []config.Worker{{Name: "controlled", Type: "go", TaskTypes: []string{"reason", "explore"}, MaxRunning: 2,
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
