//go:build linux

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

type curationDockerHooks struct {
	beforeModel func(http.ResponseWriter, *http.Request, worker.Job) bool
	configure   func(*config.Config)
	verify      func(*board.Store, board.State, map[string][]byte)
}

// Keep this small fault-injection fixture separate from the fixed live-model
// workload and its hashed acceptance contract.
func runDockerCurationInfrastructureFixture(t *testing.T, hook curationDockerHooks) {
	t.Helper()
	image := os.Getenv("PWNMESH_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set PWNMESH_DOCKER_TEST_IMAGE for real Worker curation recovery")
	}
	store, err := board.Open(filepath.Join(t.TempDir(), "curation-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := httptest.NewServer(server.New(store))
	defer api.Close()
	client := &dispatcher.Client{Base: api.URL}
	var project board.Graph
	if err := client.Do(context.Background(), "POST", "/projects", map[string]any{
		"title": "Controlled curator infrastructure recovery", "origin": "Use only local synthetic evidence.",
		"goal":              "Write the fixture evidence, curate it, and complete without creating extra business work.",
		"bootstrap_enabled": false, "orchestration_version": 1,
	}, &project, nil); err != nil {
		t.Fatal(err)
	}
	const proofPath, proof = "/workspace/curator-retry-proof.txt", "fixture ready\n"
	modelErrors := make(chan error, 16)
	var mu sync.Mutex
	turns, completing := map[string]int{}, map[string]bool{}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fail := func(err error) {
			select {
			case modelErrors <- err:
			default:
			}
			http.Error(w, "controlled recovery assertion failed", http.StatusBadRequest)
		}
		var request struct {
			Messages []agent.Message `json:"messages"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&request); err != nil {
			fail(err)
			return
		}
		run := r.Header.Get("x-opencode-session")
		var execution board.Execution
		if err := store.Do(r.Context(), func(tx *board.Tx) error {
			var err error
			execution, err = tx.Execution(project.Project.ID, run)
			return err
		}); err != nil {
			fail(err)
			return
		}
		var job worker.Job
		if err := json.Unmarshal(execution.Job, &job); err != nil {
			fail(err)
			return
		}
		if hook.beforeModel(w, r, job) {
			return
		}
		turns[run]++
		turn := turns[run]
		respond := func(block agent.Block, stop string) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"role": "assistant", "content": []agent.Block{block}, "stop_reason": stop})
		}
		call := func(name string, input any) {
			raw, _ := json.Marshal(input)
			respond(agent.Block{Type: "tool_use", ID: fmt.Sprintf("call-%d", turn), Name: name, Input: raw}, "tool_use")
		}
		action := func(op, key string, payload any) {
			call("graph_action", map[string]any{"op": op, "idempotency_key": key, "payload": payload})
		}
		previous := ""
		if turn > 1 {
			id, found := fmt.Sprintf("call-%d", turn-1), false
			for _, message := range request.Messages {
				for _, block := range message.Content {
					if block.Type == "tool_result" && block.ToolUseID == id {
						if block.IsError || json.Unmarshal(block.Content, &previous) != nil {
							fail(fmt.Errorf("%s tool %s failed: %s", job.Kind, id, block.Content))
							return
						}
						found = true
					}
				}
			}
			if !found {
				fail(fmt.Errorf("%s missing previous tool result", job.Kind))
				return
			}
		}
		switch job.Kind {
		case "reason":
			switch turn {
			case 1:
				call("read_graph", map[string]any{"section": "steps", "limit": 20})
			case 2:
				var page struct {
					Items []board.Step `json:"items"`
				}
				if err := json.Unmarshal([]byte(previous), &page); err != nil {
					fail(err)
					return
				}
				if len(page.Items) == 0 {
					action("step", "producer", map[string]any{"action": "add", "from": []string{"origin"}, "description": "Write and retain the local fixture evidence."})
				} else if len(page.Items) == 1 && page.Items[0].Status == "completed" && page.Items[0].Result != nil {
					var state board.State
					if err := store.Do(r.Context(), func(tx *board.Tx) error {
						var err error
						state, err = tx.State(project.Project.ID)
						return err
					}); err != nil {
						fail(err)
						return
					}
					if state.Curation.ThroughRevision == 0 {
						// Ordinary observations no longer schedule Curate automatically.
						// Explicitly authorize the curator whose transport will fail.
						action("curation_request", "review-evidence", map[string]any{"sources": []string{*page.Items[0].Result}, "reason": "Review the retained fixture evidence before completing the curation recovery workload."})
						return
					}
					completing[run] = true
					action("complete", "finish", map[string]any{"from": []string{*page.Items[0].Result}, "description": "The local evidence is retained and curated."})
				} else {
					fail(fmt.Errorf("unexpected business Steps: %+v", page.Items))
				}
			case 3:
				var draft struct{ Draft bool }
				if json.Unmarshal([]byte(previous), &draft) != nil || !draft.Draft {
					fail(errors.New("missing private main-Agent draft"))
					return
				}
				if completing[run] {
					action("preview", "preview", map[string]any{})
				} else {
					action("commit", "commit", map[string]any{})
				}
			case 4:
				var receipt board.DecisionReceipt
				if json.Unmarshal([]byte(previous), &receipt) != nil || receipt.CompletionReview == nil {
					fail(errors.New("missing final completion review"))
					return
				}
				action("commit", "commit", map[string]any{})
			default:
				fail(errors.New("main Agent continued after its durable commit"))
			}
		case "explore":
			switch turn {
			case 1:
				call("write", map[string]any{"path": proofPath, "content": proof})
			case 2:
				raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": map[string]any{
					"description": "Local fixture is ready", "scope": "controlled curator retry", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence": []map[string]any{{"path": proofPath}},
				}}})
				respond(agent.Block{Type: "text", Text: string(raw)}, "end_turn")
			default:
				fail(errors.New("producer was unnecessarily repeated"))
			}
		case "curate":
			if turn != 1 || job.InputSnapshot == nil {
				fail(errors.New("curator continued after committing or lacks its input"))
				return
			}
			action("curate", "curate", map[string]any{"groups": []any{}})
		default:
			fail(fmt.Errorf("unexpected role %s", job.Kind))
		}
	}))
	defer model.Close()
	conf := config.Config{
		Server: api.URL, Runtime: config.Runtime{Interval: 1, MaxWorkers: 1, MaxProjects: 1, MaxProjectWorkers: 1, HealthMode: "disabled", HealthTimeout: 10},
		Tasks:     config.Tasks{Reason: config.Task{Timeout: 30, MaxIntents: 1}, Curate: config.Task{Timeout: 30}, Explore: config.Task{Timeout: 30, ConcludeTimeout: 5}},
		Container: config.Container{Image: image, Network: testContainerNetwork(t), Namespace: fmt.Sprintf("pwnmesh-curator-retry-%d", time.Now().UnixNano()), CompletedAction: "stop"},
		Workers: []config.Worker{{Name: "controlled", Type: "go", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: 1, Env: map[string]string{
			"ANTHROPIC_BASE_URL": model.URL, "ANTHROPIC_AUTH_TOKEN": "controlled-test-only", "ANTHROPIC_MODEL": "controlled", "PWNMESH_REQUEST_TIMEOUT": "10",
		}}},
	}
	hook.configure(&conf)
	if err := conf.Validate(); err != nil {
		t.Fatal(err)
	}
	runner := &liveObservedRunner{Client: docker.New(conf.Container)}
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
	go func() { done <- dispatcher.New(conf, runner).Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("curator retry dispatcher shutdown timed out")
		}
	}()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var state board.State
	for state.Graph.Project.Status != "completed" {
		select {
		case err := <-modelErrors:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("curator infrastructure retry did not complete", ctx.Err())
		case <-tick.C:
		}
		if err := client.Do(ctx, "GET", "/projects/"+project.Project.ID+"/state", nil, &state, nil); err != nil {
			t.Fatal(err)
		}
	}
	for {
		active := false
		for _, run := range runner.snapshot() {
			active = active || run.Finished.IsZero()
		}
		if !active {
			break
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("final Worker receipt did not settle")
		}
	}
	files, err := collectLiveWorkspace(ctx, conf.Container.Namespace+"-dispatch-"+project.Project.ID, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	verified := false
	for _, fact := range state.FactRecords {
		if fact.Description == "Local fixture is ready" && !fact.Legacy && len(fact.Evidence) == 1 {
			verified = fact.Status == "valid" && fact.Evidence[0].Excerpt == proof && orchestrationEvidenceValid(fact.Evidence[0], fact.RunID, files)
		}
	}
	if !verified || !bytes.Equal(files[proofPath], []byte(proof)) || state.Curation.ThroughRevision <= 0 || state.PendingCurationRequest() != nil || len(state.Candidates) != 0 || len(state.Disputes) != 0 {
		t.Fatal("completion lost original evidence or invented semantic work")
	}
	select {
	case err := <-modelErrors:
		t.Fatal(err)
	default:
	}
	hook.verify(store, state, files)
}
