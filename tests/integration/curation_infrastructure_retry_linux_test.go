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
	"reflect"
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
	store, err := openFixtureStore(t, filepath.Join(t.TempDir(), "curation-retry.db"))
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
	type reasonPlan struct {
		op, key    string
		payload    map[string]any
		assessment board.RootAssessment
	}
	plans := map[string]reasonPlan{}
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
		job, err := scriptedJob(r.Context(), store, project.Project.ID, run)
		if err != nil {
			fail(err)
			return
		}
		if hook.beforeModel(w, r, job) {
			return
		}
		turns[run]++
		turn := turns[run]
		reply := scriptedModelReply{w: w, turn: turn}
		previous := ""
		if turn > 1 {
			previous, err = scriptedToolResult(request.Messages, fmt.Sprintf("call-%d", turn-1), false)
			if err != nil {
				fail(err)
				return
			}
		}
		switch job.Kind {
		case "reason":
			closure := job.Decision != nil && job.Decision.ClosureProtocol == 1
			if closure && turn == 3 {
				plan := plans[run]
				var result struct {
					Assessment board.RootAssessment `json:"assessment"`
				}
				if err := json.Unmarshal([]byte(previous), &result); err != nil || !reflect.DeepEqual(result.Assessment, plan.assessment) {
					fail(errors.New("main Agent did not read its root assessment before planning"))
					return
				}
				input := map[string]any{"op": plan.op, "idempotency_key": plan.key, "payload": plan.payload}
				if plan.assessment.Status == "missing" {
					input["gap_id"] = plan.assessment.Gaps[0].ID
				}
				reply.call("graph_action", input)
				return
			}
			phase := turn
			if closure && turn > 3 {
				phase--
			}
			switch phase {
			case 1:
				reply.call("read_graph", map[string]any{"section": "steps", "limit": 20})
			case 2:
				var page struct {
					Items []board.Step `json:"items"`
				}
				if err := json.Unmarshal([]byte(previous), &page); err != nil {
					fail(err)
					return
				}
				var plan reasonPlan
				if len(page.Items) == 0 {
					plan = reasonPlan{
						op: "step", key: "producer", payload: map[string]any{"action": "add", "from": []string{"origin"}, "description": "Write and retain the local fixture evidence."},
						assessment: board.RootAssessment{Status: "missing", From: []string{}, Description: "The required local fixture evidence has not been produced.", Gaps: []board.RequirementGap{{ID: "evidence", InputIDs: []string{"goal"}, Description: "Write and retain the fixture evidence required by the original goal."}}},
					}
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
						plan = reasonPlan{
							op: "curation_request", key: "review-evidence", payload: map[string]any{"sources": []string{*page.Items[0].Result}, "reason": "Review the retained fixture evidence before completing the curation recovery workload."},
							assessment: board.RootAssessment{Status: "missing", From: []string{*page.Items[0].Result}, Description: "The evidence is retained, but the original goal also requires curation.", Gaps: []board.RequirementGap{{ID: "curation", InputIDs: []string{"goal"}, Description: "Curate the retained evidence as required by the original goal."}}},
						}
					} else {
						completing[run] = true
						plan = reasonPlan{
							op: "complete", key: "finish", payload: map[string]any{"from": []string{*page.Items[0].Result}, "description": "The local evidence is retained and curated."},
							assessment: board.RootAssessment{Status: "satisfied", From: []string{*page.Items[0].Result}, Description: "The required local evidence is retained and its curation has committed; no business work remains."},
						}
					}
				} else {
					fail(fmt.Errorf("unexpected business Steps: %+v", page.Items))
					return
				}
				if closure {
					plans[run] = plan
					reply.call("assess_root", plan.assessment)
				} else {
					reply.action(plan.op, plan.key, plan.payload)
				}
			case 3:
				var draft struct{ Draft bool }
				if json.Unmarshal([]byte(previous), &draft) != nil || !draft.Draft {
					fail(errors.New("missing private main-Agent draft"))
					return
				}
				if completing[run] {
					reply.action("preview", "preview", map[string]any{})
				} else {
					reply.action("commit", "commit", map[string]any{})
				}
			case 4:
				var receipt board.DecisionReceipt
				if json.Unmarshal([]byte(previous), &receipt) != nil || receipt.CompletionReview == nil {
					fail(errors.New("missing final completion review"))
					return
				}
				reply.action("commit", "commit", map[string]any{})
			default:
				fail(errors.New("main Agent continued after its durable commit"))
			}
		case "explore":
			switch turn {
			case 1:
				reply.call("write", map[string]any{"path": proofPath, "content": proof})
			case 2:
				raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": map[string]any{
					"description": "Local fixture is ready", "scope": "controlled curator retry", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence": []map[string]any{{"path": proofPath}},
				}}})
				reply.respond(agent.Block{Type: "text", Text: string(raw)}, "end_turn")
			default:
				fail(errors.New("producer was unnecessarily repeated"))
			}
		case "curate":
			if turn != 1 || job.InputSnapshot == nil {
				fail(errors.New("curator continued after committing or lacks its input"))
				return
			}
			reply.action("curate", "curate", map[string]any{"groups": []any{}})
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
	files, err := collectLiveWorkspace(ctx, runner.ContainerName(project.Project.ID), t.TempDir())
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
