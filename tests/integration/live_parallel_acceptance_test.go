//go:build linux

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

type liveModelInterval struct{ start, end time.Time }

type liveParallelWorker struct {
	StepID     string `json:"step_id"`
	RunID      string `json:"run_id"`
	ModelCalls int    `json:"successful_model_calls"`
}

type liveParallelReceipt struct {
	Passed         bool                 `json:"passed"`
	Workers        []liveParallelWorker `json:"workers"`
	OverlapSeconds float64              `json:"model_overlap_seconds"`
	Failures       []string             `json:"failures"`
	Basis          string               `json:"basis"`
}

// Opt-in stricter acceptance for the two independent report-producing Steps.
// Successful work, immutable identity and actual model intervals are required;
// separate Steps, available slots or overlapping containers alone are not proof.
func validateLiveParallelWorkers(state board.State, files map[string][]byte, expectedWorkers ...int) (liveParallelReceipt, []string) {
	expected := 2
	if len(expectedWorkers) == 1 {
		expected = expectedWorkers[0]
	}
	receipt := liveParallelReceipt{Workers: []liveParallelWorker{}, Failures: []string{}, Basis: fmt.Sprintf("Exactly %d successful independent ordinary Steps with distinct immutable jobs and accepted results; positive union of intervals with at least two successful main-turn model calls active in their durable journals. No transcript content is exported.", expected)}
	if expected < 2 || len(expectedWorkers) > 1 {
		receipt.Failures = append(receipt.Failures, "parallel acceptance requires an expected Worker count of at least two")
		return receipt, receipt.Failures
	}
	facts := map[string]board.FactRecord{}
	for _, fact := range state.FactRecords {
		facts[fact.ID] = fact
	}
	var intervals [][]liveModelInterval
	seenRuns := map[string]bool{}
	for _, step := range state.Steps {
		if step.DisputeID != "" || len(step.DependsOn) != 0 || staleRepairCompletionMarker(state, step) {
			continue
		}
		if step.Worker == nil {
			receipt.Failures = append(receipt.Failures, "independent Step has no Worker: "+step.ID)
			continue
		}
		job, ok := orchestrationSuccessfulRun(state.Graph.Project, step, *step.Worker, facts, files)
		if !ok {
			receipt.Failures = append(receipt.Failures, "independent Step lacks a successful bound result: "+step.ID)
			continue
		}
		if seenRuns[job.RunID] {
			receipt.Failures = append(receipt.Failures, "independent Steps reused a Worker run: "+step.ID)
			continue
		}
		seenRuns[job.RunID] = true
		calls, err := liveSuccessfulModelIntervals(files["/workspace/.pwnmesh/runs/"+job.RunID+"/events.jsonl"])
		if err != nil || len(calls) == 0 {
			receipt.Failures = append(receipt.Failures, "independent Step lacks complete successful model intervals: "+step.ID)
			continue
		}
		receipt.Workers = append(receipt.Workers, liveParallelWorker{StepID: step.ID, RunID: job.RunID, ModelCalls: len(calls)})
		intervals = append(intervals, calls)
	}
	if len(receipt.Workers) != expected {
		receipt.Failures = append(receipt.Failures, fmt.Sprintf("parallel acceptance requires exactly %d independent successful Worker runs", expected))
	} else {
		receipt.OverlapSeconds = liveParallelOverlapSeconds(intervals)
		if receipt.OverlapSeconds <= 0 {
			receipt.Failures = append(receipt.Failures, "independent Workers had no successful model request overlap")
		}
	}
	receipt.Passed = len(receipt.Failures) == 0
	return receipt, receipt.Failures
}

func liveParallelOverlapSeconds(workers [][]liveModelInterval) float64 {
	type edge struct {
		at    time.Time
		delta int
	}
	var edges []edge
	for _, calls := range workers {
		// Each Worker's journal was checked for nonoverlapping main calls.
		for _, call := range calls {
			edges = append(edges, edge{call.start, 1}, edge{call.end, -1})
		}
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].at.Before(edges[j].at) })
	var previous time.Time
	active, seconds := 0, 0.0
	for _, event := range edges {
		if active >= 2 {
			seconds += event.at.Sub(previous).Seconds()
		}
		active += event.delta
		previous = event.at
	}
	return seconds
}

func liveSuccessfulModelIntervals(raw []byte) ([]liveModelInterval, error) {
	var calls []liveModelInterval
	var pending *agent.Event
	var lastEnd time.Time
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event agent.Event
		if json.Unmarshal(line, &event) != nil {
			return nil, fmt.Errorf("invalid event journal")
		}
		if event.Type != "model_call_start" && event.Type != "model_call_end" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, event.At)
		if err != nil || at.IsZero() || event.Request == nil || (event.Request.Kind != "turn" && event.Request.Kind != "summary") {
			return nil, fmt.Errorf("invalid model observation")
		}
		if event.Type == "model_call_start" {
			if pending != nil || at.Before(lastEnd) {
				return nil, fmt.Errorf("overlapping main-stream model starts")
			}
			pending = &event
			continue
		}
		if pending == nil || pending.Request.Kind != event.Request.Kind {
			return nil, fmt.Errorf("unpaired model end")
		}
		start, _ := time.Parse(time.RFC3339Nano, pending.At)
		if !at.After(start) {
			return nil, fmt.Errorf("invalid model interval")
		}
		if event.Request.Kind == "turn" && !event.Request.Failed {
			calls = append(calls, liveModelInterval{start: start, end: at})
		}
		lastEnd, pending = at, nil
	}
	if pending != nil {
		return nil, fmt.Errorf("incomplete model interval")
	}
	return calls, nil
}

func TestLiveParallelAcceptanceRequiresActualBoundModelOverlap(t *testing.T) {
	journal := func(start, end int, failed bool) []byte {
		var raw []byte
		for i, second := range []int{start, end} {
			event := agent.Event{Type: []string{"model_call_start", "model_call_end"}[i], At: fmt.Sprintf("2026-09-28T00:00:%02dZ", second), Request: &agent.RequestObservation{Kind: "turn", Failed: failed}}
			line, _ := json.Marshal(event)
			raw = append(raw, append(line, '\n')...)
		}
		return raw
	}
	for _, test := range []struct {
		name string
		edit func(*board.State, map[string][]byte)
		pass bool
	}{
		{"overlap", func(_ *board.State, _ map[string][]byte) {}, true},
		{"serial", func(_ *board.State, f map[string][]byte) {
			f["/workspace/.pwnmesh/runs/producer-b/events.jsonl"] = journal(11, 20, false)
		}, false},
		{"touching", func(_ *board.State, f map[string][]byte) {
			f["/workspace/.pwnmesh/runs/producer-b/events.jsonl"] = journal(10, 20, false)
		}, false},
		{"failed_request", func(_ *board.State, f map[string][]byte) {
			f["/workspace/.pwnmesh/runs/producer-b/events.jsonl"] = journal(5, 15, true)
		}, false},
		{"missing_journal", func(_ *board.State, f map[string][]byte) {
			delete(f, "/workspace/.pwnmesh/runs/producer-b/events.jsonl")
		}, false},
		{"wrong_job", func(_ *board.State, f map[string][]byte) {
			f["/workspace/.pwnmesh/runs/producer-b/job.json"] = f["/workspace/.pwnmesh/runs/producer-a/job.json"]
		}, false},
		{"dependent", func(s *board.State, _ map[string][]byte) { s.Steps[1].DependsOn = []string{"i1"} }, false},
		{"invalid_fact", func(s *board.State, _ map[string][]byte) { s.FactRecords[1].SupportInvalid = true }, false},
		{"incomplete", func(_ *board.State, f map[string][]byte) {
			raw := journal(5, 15, false)
			f["/workspace/.pwnmesh/runs/producer-b/events.jsonl"] = bytes.SplitAfterN(raw, []byte{'\n'}, 2)[0]
		}, false},
		{"unpaired_end", func(_ *board.State, f map[string][]byte) {
			raw := journal(5, 15, false)
			f["/workspace/.pwnmesh/runs/producer-b/events.jsonl"] = bytes.SplitAfterN(raw, []byte{'\n'}, 2)[1]
		}, false},
		{"duplicate_start", func(_ *board.State, f map[string][]byte) {
			raw := journal(5, 15, false)
			f["/workspace/.pwnmesh/runs/producer-b/events.jsonl"] = append(bytes.SplitAfterN(raw, []byte{'\n'}, 2)[0], raw...)
		}, false},
		{"backwards", func(_ *board.State, f map[string][]byte) {
			f["/workspace/.pwnmesh/runs/producer-b/events.jsonl"] = journal(15, 5, false)
		}, false},
		{"summary_not_work", func(_ *board.State, f map[string][]byte) {
			f["/workspace/.pwnmesh/runs/producer-b/events.jsonl"] = bytes.ReplaceAll(journal(5, 15, false), []byte(`"turn"`), []byte(`"summary"`))
		}, false},
		{"shadow_not_work", func(_ *board.State, f map[string][]byte) {
			f["/workspace/.pwnmesh/runs/producer-b/events.jsonl"] = bytes.ReplaceAll(journal(5, 15, false), []byte(`"model_call_`), []byte(`"replan_model_call_`))
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, files := orchestrationDeliveryFixture(t)
			files["/workspace/.pwnmesh/runs/producer-a/events.jsonl"] = journal(0, 10, false)
			files["/workspace/.pwnmesh/runs/producer-b/events.jsonl"] = journal(5, 15, false)
			files["/workspace/.pwnmesh/runs/review-c/events.jsonl"] = journal(0, 30, false)
			test.edit(&state, files)
			receipt, failures := validateLiveParallelWorkers(state, files)
			if receipt.Passed != test.pass || (len(failures) == 0) != test.pass {
				t.Fatalf("passed=%v: %#v", test.pass, receipt)
			}
			if test.pass && (receipt.OverlapSeconds != 5 || len(receipt.Workers) != 2) {
				t.Fatalf("wrong interval intersection or included dependent review: %#v", receipt)
			}
		})
	}
}

func TestLiveParallelAcceptanceThreeWorkersKeepsExactCardinalityAndUnion(t *testing.T) {
	for _, test := range []struct {
		name      string
		expected  int
		bounds    [3][2]int
		duplicate bool
		want      float64
		pass      bool
	}{
		{"three_overlapping_union", 3, [3][2]int{{0, 10}, {5, 15}, {0, 30}}, false, 15, true},
		{"one_pair_overlaps", 3, [3][2]int{{0, 10}, {5, 15}, {20, 30}}, false, 5, true},
		{"serial", 3, [3][2]int{{0, 10}, {10, 20}, {20, 30}}, false, 0, false},
		{"original_two_worker_contract", 2, [3][2]int{{0, 10}, {5, 15}, {0, 30}}, false, 0, false},
		{"missing_fourth", 4, [3][2]int{{0, 10}, {5, 15}, {0, 30}}, false, 0, false},
		{"duplicate_run", 3, [3][2]int{{0, 10}, {5, 15}, {0, 30}}, true, 15, false},
		{"invalid_expected_count", 1, [3][2]int{{0, 10}, {5, 15}, {0, 30}}, false, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, files := orchestrationDeliveryFixture(t)
			state.Steps[2].DisputeID, state.Steps[2].DependsOn = "", nil
			for n, run := range []string{"producer-a", "producer-b", "review-c"} {
				var raw []byte
				for i, second := range test.bounds[n] {
					event := agent.Event{Type: []string{"model_call_start", "model_call_end"}[i], At: fmt.Sprintf("2026-09-28T00:00:%02dZ", second), Request: &agent.RequestObservation{Kind: "turn"}}
					line, _ := json.Marshal(event)
					raw = append(raw, append(line, '\n')...)
				}
				files["/workspace/.pwnmesh/runs/"+run+"/events.jsonl"] = raw
			}
			if test.duplicate {
				state.Steps = append(state.Steps, state.Steps[0])
			}
			receipt, failures := validateLiveParallelWorkers(state, files, test.expected)
			if receipt.Passed != test.pass || (len(failures) == 0) != test.pass || receipt.OverlapSeconds != test.want {
				t.Fatalf("expected pass=%v overlap=%g: %#v", test.pass, test.want, receipt)
			}
			if test.pass && len(receipt.Workers) != 3 {
				t.Fatalf("lost a bound independent Worker: %#v", receipt)
			}
		})
	}
}
