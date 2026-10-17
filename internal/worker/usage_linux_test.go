//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/worker/dag"
	eventlog "pwnmesh/internal/worker/internal/journal"
	"pwnmesh/internal/workergraph"
)

func TestExploreUsageIncludesOriginalChildrenOnceAcrossReuseAndRecovery(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	calls := 0
	usage := agent.Usage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 19, CacheWriteTokens: 37}
	o := Options{RunDir: dir, graphProvider: func(id string) (agent.Provider, error) {
		return scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
			calls++
			message := agent.Text("assistant", "Child observations retained.")
			message.Usage = &usage
			if id == "failed" {
				return message, errors.New("fixture failed after reported usage")
			}
			return message, nil
		}), nil
	}}
	spec := dag.Spec{Key: "source", Parallelism: 1, Nodes: []dag.Node{
		{ID: "success", Kind: "agent", Task: "Return an account", Resources: []string{}},
		{ID: "failed", Kind: "agent", Task: "Return an error", Resources: []string{}, Optional: true},
		{ID: "skipped", Kind: "agent", Task: "Must not run", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "success"}}, When: &dag.When{Node: "success", Status: "succeeded", Contains: "absent condition"}},
	}}
	if _, err := mixedGraphCall(t, context.Background(), j, o, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := mixedGraphCall(t, context.Background(), j, o, spec); err != nil {
		t.Fatal(err)
	}
	reuse := dag.Spec{Key: "import", Nodes: []dag.Node{{ID: "reused", Kind: "reuse", Resources: []string{}, ReuseFrom: &dag.Reuse{Key: "source", Node: "success"}}}}
	if _, err := mixedGraphCall(t, context.Background(), j, o, reuse); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("reuse repeated model calls: %d", calls)
	}
	runtime, err := restoreSession(j, o)
	if err != nil {
		t.Fatal(err)
	}
	runtime.emit(agent.Event{Type: "model_call_start", Request: &agent.RequestObservation{Kind: "turn"}})
	runtime.emit(agent.Event{Type: "model_call_end", Request: &agent.RequestObservation{Usage: &usage}})
	finish := func(runtime *sessionRuntime) *DecisionMetrics {
		t.Helper()
		result, err := runtime.finish(context.Background(), Result{Type: "result", Status: "failed", FailureKind: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "failed" || result.Metrics == nil {
			t.Fatalf("result changed: %+v", result)
		}
		return result.Metrics
	}
	first := finish(runtime)
	want := agent.Usage{InputTokens: 33, OutputTokens: 21, CacheReadTokens: 57, CacheWriteTokens: 111}
	if first.Usage != want || first.ModelCalls != 3 || first.UsageCalls != 3 || first.FailedCalls != 1 || first.UsageScope != "run_and_children" || first.UsageStatus != "reported_only" || first.ChildUsage == nil || first.ChildUsage.Sessions != 2 || first.ChildUsage.Usage.CacheWriteTokens != 74 {
		t.Fatalf("incorrect parent/child usage: %+v, children=%+v", first, first.ChildUsage)
	}
	if runtime.metrics.Usage != usage {
		t.Fatal("child subtotal mutated parent journal accumulator")
	}
	second := finish(runtime)
	if second.Usage != first.Usage || !reflect.DeepEqual(second.ChildUsage, first.ChildUsage) {
		t.Fatal("repeated finalization double counted children")
	}
	if err := runtime.journal.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := restoreSession(j, o)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	third := finish(restored)
	if third.Usage != want || !reflect.DeepEqual(third.ChildUsage, first.ChildUsage) || calls != 2 {
		t.Fatal("recovery changed usage or replayed children")
	}
}

func TestChildUsageDamageRemainsExplicitlyPartial(t *testing.T) {
	for _, mutation := range []string{"missing", "checksum", "identity", "tail"} {
		t.Run(mutation, func(t *testing.T) {
			j, dir := graphWrapperJob(t), t.TempDir()
			o := Options{RunDir: dir, graphProvider: func(string) (agent.Provider, error) {
				return scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
					message := agent.Text("assistant", "Observed work")
					message.Usage = &agent.Usage{CacheWriteTokens: 37}
					return message, nil
				}), nil
			}}
			spec := dag.Spec{Key: "source", Nodes: []dag.Node{{ID: "child", Kind: "agent", Task: "Return observations", Resources: []string{}}}}
			if _, err := mixedGraphCall(t, context.Background(), j, o, spec); err != nil {
				t.Fatal(err)
			}
			nodeDir := filepath.Join(dir, "graph-tools", "source", "nodes", "child")
			path := filepath.Join(nodeDir, "events.jsonl")
			switch mutation {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "checksum":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw[0] = ' '
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "identity":
				path = filepath.Join(nodeDir, "session.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var state graphAgentSession
				if err := json.Unmarshal(raw, &state); err != nil {
					t.Fatal(err)
				}
				state.RunID = "other-run"
				raw, _ = json.Marshal(state)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "tail":
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString("{\"type\":\"model_call_start\",\"request\":{\"kind\":\"turn\"}}\n")
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			got := withChildUsage(context.Background(), j, dir, newDecisionMetrics().DecisionMetrics)
			if got.UsageStatus != "partial" || got.ChildUsage == nil || !got.ChildUsage.Incomplete {
				t.Fatalf("damaged usage presented as complete: %+v", got)
			}
			want := 0
			if mutation == "tail" {
				want = 37
			}
			if got.Usage.CacheWriteTokens != want {
				t.Fatalf("unverified usage was counted: %+v", got.Usage)
			}
		})
	}
}

func TestChildUsageIncludesExcludedCallsButNotNewTail(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	node := dag.AgentNode{Key: "source", Dir: dir, Spec: dag.Node{ID: "child"}, InputSHA256: "fixture"}
	log, err := eventlog.Open(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := log.SyncCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	appendUsage := func(log *eventlog.Log, tokens int) {
		for _, event := range []agent.Event{
			{Type: "model_call_start", Request: &agent.RequestObservation{Kind: "turn"}},
			{Type: "model_call_end", Request: &agent.RequestObservation{Usage: &agent.Usage{CacheWriteTokens: tokens}}},
		} {
			if err := log.Append(event); err != nil {
				t.Fatal(err)
			}
		}
	}
	appendUsage(log, 37)
	log.Close()
	log, err = eventlog.Open(dir, &saved, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	saved, err = log.SyncCheckpoint()
	if err != nil || len(saved.Excluded) != 1 {
		t.Fatalf("fixture did not retain a discarded call: %+v %v", saved, err)
	}
	state := graphAgentSession{RunID: j.RunID, GraphKey: node.Key, NodeID: node.Spec.ID, GraphInputSHA256: node.InputSHA256, Log: saved}
	raw, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dir, "session.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	appendUsage(log, 999)
	got, incomplete, err := childUsage(context.Background(), j, node)
	if err != nil || !incomplete || got.ModelCalls != 1 || got.UsageCalls != 1 || got.Usage.CacheWriteTokens != 37 {
		t.Fatalf("child accounting confused exclusions and new tail: %+v incomplete=%v err=%v", got, incomplete, err)
	}
}
