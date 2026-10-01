package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
	"strings"
	"testing"
	"time"
)

func TestLargeProducerHistoryStillSchedulesAndCommitsCuration(t *testing.T) {
	s, runner, store, graph := pendingCurationFixture(t)
	ctx := context.Background()
	if err := s.Client.Do(ctx, "GET", projectPath(graph.Project.ID), nil, &graph, nil); err != nil {
		t.Fatal(err)
	}
	var target *board.Intent
	for n := range graph.Intents {
		if graph.Intents[n].To == nil && graph.Intents[n].ConcludedAt == nil {
			target = &graph.Intents[n]
			break
		}
	}
	if target == nil {
		t.Fatal("missing producer Step")
	}
	producer := prepareCurationTestTask(t, s, graph, "explore", "large-observations", target)
	heartbeat := make(chan struct{}, 1)
	s.Client.HTTP = &http.Client{Timeout: 10 * time.Second, Transport: executionQueryTransport(func(request *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(request)
		if err == nil && response.StatusCode == http.StatusOK && request.URL.Path == s.leasePath(producer)+"/heartbeat" {
			select {
			case heartbeat <- struct{}{}:
			default:
			}
		}
		return response, err
	})}
	runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
		// Cross a real heartbeat before growing the state so the lease path is
		// covered even when ordinary builds finish faster than the first tick.
		select {
		case <-heartbeat:
		case <-ctx.Done():
			return worker.Result{}, ctx.Err()
		}
		last := ""
		for n := 0; n < 12; n++ {
			payload, _ := json.Marshal(map[string]any{"description": fmt.Sprintf("observation %d: ", n) + strings.Repeat("x", 8192), "scope": "local fixture", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence": []board.EvidenceRef{{RunID: job.RunID, Path: "/workspace/evidence/" + job.RunID + ".txt", Excerpt: "retained synthetic observation"}}})
			value, err := runner.graph(ctx, job, worker.GraphRequest{Op: "graph_action", Action: board.StateAction{Op: "fact", IdempotencyKey: fmt.Sprintf("large-%d", n), Payload: payload}})
			if err != nil {
				return worker.Result{}, err
			}
			last = value.(board.StateActionResult).ID
		}
		return worker.Result{Status: "success", Text: fmt.Sprintf(`{"accepted":true,"outcome":"completed","data":{"fact_id":%q}}`, last)}, nil
	}
	if outcome, err := s.runTask(ctx, producer); outcome != "success" || err != nil {
		t.Fatalf("producer: %s %v", outcome, err)
	}
	runner.run = nil
	var state board.State
	if err := s.Client.Do(ctx, "GET", projectPath(graph.Project.ID)+"/state", nil, &state, nil); err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(worker.Job{Graph: state.Graph, State: &state})
	if len(legacy) <= board.MaxCurationInputBytes {
		t.Fatal("history does not exercise the old size barrier")
	}
	if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
		t.Fatalf("large curation did not launch: %v %v", ok, err)
	}
	s.wg.Wait()
	s.reap()
	curators := 0
	for _, e := range testExecutions(t, store) {
		if e.ProjectID == graph.Project.ID && e.Kind == "curate" {
			curators++
			if e.Status != "succeeded" || len(e.Job) >= board.MaxCurationInputBytes {
				t.Fatalf("large curation failed: %s", e.Status)
			}
		}
	}
	var current board.State
	if err := s.Client.Do(ctx, "GET", projectPath(graph.Project.ID)+"/state", nil, &current, nil); err != nil {
		t.Fatal(err)
	}
	if curators != 1 || current.Curation.ThroughRevision != state.Revision || len(current.FactRecords) != len(state.FactRecords) {
		t.Fatal("curation failed to advance without losing observations")
	}
}
