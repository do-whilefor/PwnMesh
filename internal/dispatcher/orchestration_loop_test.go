package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/server"
	"pwnmesh/internal/worker"
)

// Every scripted role sees the same immutable input and graph bridge as a real
// Worker. Only the test's final assertions inspect retained execution records.
type orchestrationLoopRunner struct {
	batchProtocolRunner
	loseFinal bool
}

func (r *orchestrationLoopRunner) Run(ctx context.Context, _ config.Worker, job worker.Job) (worker.Result, error) {
	r.mu.Lock()
	r.jobs = append(r.jobs, job)
	r.mu.Unlock()
	if !job.GraphRPC || job.ResultContractVersion != 2 || job.Graph.Project.OrchestrationVersion != 1 {
		return worker.Result{}, errors.New("orchestration did not retain the registered protocol")
	}
	switch job.Kind {
	case "reason":
		return r.plan(ctx, job)
	case "curate":
		return r.curate(ctx, job)
	case "explore":
		return r.observe(ctx, job)
	default:
		return worker.Result{}, fmt.Errorf("unexpected role %q", job.Kind)
	}
}

func (r *orchestrationLoopRunner) page(ctx context.Context, job worker.Job, section string, out any) error {
	version := ""
	if job.Decision != nil {
		version = job.Decision.StateVersion
	}
	op := "read_graph"
	if job.Kind == "curate" && job.InputSnapshot != nil {
		op, version = "read_snapshot", job.InputSnapshot.StateVersion
	}
	value, err := r.graph(ctx, job, worker.GraphRequest{Op: op, Section: section, ExpectedVersion: version, Limit: 50})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var page struct {
		Items json.RawMessage `json:"items"`
	}
	if err = json.Unmarshal(raw, &page); err != nil {
		return err
	}
	return json.Unmarshal(page.Items, out)
}

func (r *orchestrationLoopRunner) plan(ctx context.Context, job worker.Job) (worker.Result, error) {
	if job.Decision == nil || job.Decision.Version != 2 {
		return worker.Result{}, errors.New("missing current decision input")
	}
	var steps []board.Step
	var disputes []board.Dispute
	var candidates []board.Candidate
	for _, query := range []struct {
		section string
		out     any
	}{{"steps", &steps}, {"disputes", &disputes}, {"candidates", &candidates}} {
		if err := r.page(ctx, job, query.section, query.out); err != nil {
			return worker.Result{}, err
		}
	}
	actions := []board.DecisionAction{}
	appendAction := func(op string, payload any) {
		raw, _ := json.Marshal(payload)
		actions = append(actions, board.DecisionAction{Op: op, Payload: raw})
	}
	if len(steps) == 0 {
		for _, description := range []string{"Observe the fixture: producer A", "Observe the fixture: producer B"} {
			appendAction("step", map[string]any{"action": "add", "from": []string{"origin"}, "description": description})
		}
	} else {
		for _, dispute := range disputes {
			if dispute.Status == "resolved" || len(dispute.ReviewStepIDs) != 0 {
				continue
			}
			sources := []string{}
			for _, candidate := range candidates {
				for _, id := range dispute.CandidateIDs {
					if candidate.ID == id {
						sources = append(sources, candidate.Sources...)
					}
				}
			}
			appendAction("step", map[string]any{"action": "add", "from": sources, "description": "Independently replay both producer observations", "dispute_id": dispute.ID})
		}
		if len(disputes) == 1 && disputes[0].Status == "resolved" {
			open := false
			for _, step := range steps {
				open = open || (step.Status != "completed" && step.Status != "abandoned")
			}
			if !open {
				appendAction("complete", map[string]any{"from": disputes[0].ReviewFactIDs, "description": "Independent replay resolved the original contradictory judgments"})
			}
		}
	}
	batch := &board.DecisionBatch{ExpectedVersion: job.Decision.StateVersion, Actions: actions}
	if _, err := r.graph(ctx, job, worker.GraphRequest{Op: "decision_preview", Batch: batch}); err != nil {
		return worker.Result{}, err
	}
	if _, err := r.graph(ctx, job, worker.GraphRequest{Op: "decision_commit", Batch: batch}); err != nil {
		return worker.Result{}, err
	}
	return worker.Result{Status: "success", Text: `{"accepted":true,"data":{"decided":true}}`}, nil
}

func (r *orchestrationLoopRunner) observe(ctx context.Context, job worker.Job) (worker.Result, error) {
	if job.Intent == nil {
		return worker.Result{}, errors.New("observation has no authorized Step")
	}
	status, observation := "verified", "HTTP/1.1 401 Unauthorized"
	if strings.Contains(job.Intent.Description, "producer B") {
		status, observation = "refuted", "HTTP/1.1 200 OK"
	}
	// The new review execution explicitly observes both original claims and
	// produces its own evidence instead of recycling a producer's result.
	if strings.Contains(job.Intent.Description, "Independently") {
		var view struct {
			Steps []board.Step `json:"steps"`
		}
		if json.Unmarshal(job.InputView, &view) != nil {
			return worker.Result{}, errors.New("review has no bounded input view")
		}
		bound := false
		for _, step := range view.Steps {
			bound = bound || (step.ID == job.Intent.ID && step.DisputeID != "")
		}
		if !bound {
			return worker.Result{}, errors.New("review input omitted its dispute binding")
		}
		observation = "Independent replay: unauthenticated request 401; authenticated control 200"
	}
	payload, _ := json.Marshal(map[string]any{"description": observation, "scope": "local fixture", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence": []board.EvidenceRef{{RunID: job.RunID, Path: "/workspace/evidence/" + job.RunID + ".txt", Excerpt: observation}}})
	value, err := r.graph(ctx, job, worker.GraphRequest{Op: "graph_action", Action: board.StateAction{Op: "fact", IdempotencyKey: job.RunID + ":fact", Payload: payload}})
	if err != nil {
		return worker.Result{}, err
	}
	fact := value.(board.StateActionResult)
	payload, _ = json.Marshal(map[string]any{"claim": "Anonymous access is denied", "scope": "local fixture", "status": status, "sources": []string{fact.ID}, "reason": "Candidate interpretation of this run's recorded response"})
	if _, err = r.graph(ctx, job, worker.GraphRequest{Op: "graph_action", Action: board.StateAction{Op: "candidate", IdempotencyKey: job.RunID + ":candidate", Payload: payload}}); err != nil {
		return worker.Result{}, err
	}
	final, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact_id": fact.ID}})
	return worker.Result{Status: "success", Text: string(final)}, nil
}

func (r *orchestrationLoopRunner) curate(ctx context.Context, job worker.Job) (worker.Result, error) {
	if job.State != nil || job.Intent != nil || job.InputSnapshot == nil || len(job.InputView) == 0 {
		return worker.Result{}, errors.New("curator lost its frozen snapshot input")
	}
	state := &board.State{Revision: job.InputSnapshot.Revision}
	for _, query := range []struct {
		section string
		out     any
	}{{"candidates", &state.Candidates}, {"disputes", &state.Disputes}, {"facts", &state.FactRecords}, {"steps", &state.Steps}} {
		if err := r.page(ctx, job, query.section, query.out); err != nil {
			return worker.Result{}, err
		}
	}
	groups := []board.CurateGroup{}
	if len(state.Candidates) != 0 {
		group := board.CurateGroup{Status: "verified", Reason: "Preserve every producer and compare their source observations", Question: "Does an unauthenticated request get rejected when replayed with an authenticated negative control?"}
		for _, candidate := range state.Candidates {
			group.CandidateIDs = append(group.CandidateIDs, candidate.ID)
		}
		for _, dispute := range state.Disputes {
			group.DisputeID = dispute.ID
			for _, fact := range state.FactRecords {
				for _, reviewStep := range dispute.ReviewStepIDs {
					for _, step := range state.Steps {
						if step.ID == reviewStep && step.Status == "completed" && fact.SourceStepID == reviewStep {
							group.ReviewFactIDs = append(group.ReviewFactIDs, fact.ID)
						}
					}
				}
			}
			if len(group.ReviewFactIDs) != 0 {
				group.Resolution, group.Reason = "resolved", "The independent run directly replayed the anonymous request and authenticated control"
			}
		}
		groups = append(groups, group)
	}
	payload, _ := json.Marshal(board.CuratePayload{ThroughRevision: state.Revision, Groups: groups})
	value, err := r.graph(ctx, job, worker.GraphRequest{Op: "graph_action", Action: board.StateAction{Op: "curate", IdempotencyKey: job.RunID + ":curate", ExpectedVersion: job.InputSnapshot.StateVersion, Payload: payload}})
	if err != nil {
		return worker.Result{}, err
	}
	if !value.(board.StateActionResult).Committed {
		return worker.Result{}, errors.New("curation had no durable receipt")
	}
	if r.loseFinal {
		return worker.Result{}, errors.New("synthetic curator crash after persisted curation")
	}
	return worker.Result{Status: "success", Text: `{"accepted":true,"data":{"curated":true}}`}, nil
}

type curationLostResponseTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	lost int
}

func (t *curationLostResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err == nil && response.StatusCode == http.StatusOK && request.Header.Get("X-PwnMesh-Lease") == "curate" && strings.HasSuffix(request.URL.Path, "/state/actions") {
		t.mu.Lock()
		t.lost++
		t.mu.Unlock()
		_ = response.Body.Close()
		response.Body = io.NopCloser(strings.NewReader(""))
	}
	return response, err
}

func TestOrchestrationIndependentReviewClosesThroughProductionScheduler(t *testing.T) {
	for _, mode := range []string{"normal", "curator_final_lost", "curation_response_lost"} {
		t.Run(mode, func(t *testing.T) {
			store, err := board.Open(filepath.Join(t.TempDir(), "orchestration.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			api := httptest.NewServer(server.New(store))
			defer api.Close()
			runner := &orchestrationLoopRunner{loseFinal: mode == "curator_final_lost"}
			cfg := config.Config{
				Server:    api.URL,
				Runtime:   config.Runtime{Interval: 1, MaxWorkers: 1, MaxProjects: 1, MaxProjectWorkers: 1, HealthTimeout: 5, HealthMode: "disabled"},
				Tasks:     config.Tasks{Reason: config.Task{MaxIntents: 3}, Explore: config.Task{ConcludeTimeout: 60}},
				Container: config.Container{Image: "scripted", Network: "bridge", CompletedAction: "stop"},
				Workers:   []config.Worker{{Name: "scripted", Type: "go", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: 1, Env: map[string]string{"ANTHROPIC_BASE_URL": "http://unused.invalid", "ANTHROPIC_AUTH_TOKEN": "synthetic", "ANTHROPIC_MODEL": "synthetic"}}},
			}
			if err = cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			scheduler := New(cfg, runner)
			lost := &curationLostResponseTransport{base: http.DefaultTransport}
			if mode == "curation_response_lost" {
				scheduler.Client.HTTP = &http.Client{Transport: lost, Timeout: 5 * time.Second}
			}
			var graph board.Graph
			if err = scheduler.Client.Do(context.Background(), "POST", "/projects", map[string]any{"title": "Independent review", "origin": "Two conflicting synthetic observations", "goal": "Independently resolve the anonymous access dispute", "orchestration_version": 1}, &graph, nil); err != nil {
				t.Fatal(err)
			}
			state := finishBatchFixture(t, scheduler, graph.Project.ID)
			if len(state.Candidates) != 3 || len(state.Disputes) != 1 || state.Disputes[0].Status != "resolved" || len(state.Disputes[0].ReviewStepIDs) != 1 || len(state.Disputes[0].ReviewFactIDs) != 1 {
				t.Fatalf("original candidates or independent review were lost: %+v", state)
			}
			if state.Candidates[0].Status != "verified" || state.Candidates[1].Status != "refuted" || len(state.Findings) != 1 || state.Findings[0].Status != "verified" {
				t.Fatal("curation overwrote producers or did not publish the reviewed conclusion")
			}
			reviewID := state.Disputes[0].ReviewStepIDs[0]
			producerRuns, reviewRuns := map[string]bool{}, map[string]bool{}
			curations := 0
			for _, execution := range testExecutions(t, store) {
				if execution.ProjectID != graph.Project.ID {
					continue
				}
				if execution.Status != "succeeded" {
					t.Fatalf("durable role result did not recover: %s %s", execution.Kind, execution.Status)
				}
				if execution.Kind == "curate" {
					curations++
				}
				if execution.Kind == "explore" {
					if execution.Intent == reviewID {
						reviewRuns[execution.ID] = true
					} else {
						producerRuns[execution.ID] = true
					}
				}
			}
			if len(producerRuns) != 2 || len(reviewRuns) != 1 || curations < 2 {
				t.Fatalf("unexpected execution counts: producers=%d reviews=%d curations=%d", len(producerRuns), len(reviewRuns), curations)
			}
			for run := range reviewRuns {
				if producerRuns[run] {
					t.Fatal("independent review reused an original producer run")
				}
			}
			if mode == "curation_response_lost" && lost.lost == 0 {
				t.Fatal("curation response loss was not exercised")
			}
		})
	}
}

func TestLegacyProjectRemainsReadableWithoutScheduling(t *testing.T) {
	scheduler, runner, _, graph, store := batchSchedulerFixture(t, 1)
	// Historical rows predate the current project-creation boundary.
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		_, err := tx.Exec("DELETE FROM xloom_project_orchestration WHERE project_id=?", graph.Project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	retryTicks(t, scheduler, 1)
	var state board.State
	if err := scheduler.Client.Do(context.Background(), "GET", projectPath(graph.Project.ID)+"/state", nil, &state, nil); err != nil {
		t.Fatal(err)
	}
	if state.Graph.Project.OrchestrationVersion != 0 || len(runner.jobs) != 0 || len(testExecutions(t, store)) != 0 {
		t.Fatal("historical project was migrated or scheduled")
	}
}
