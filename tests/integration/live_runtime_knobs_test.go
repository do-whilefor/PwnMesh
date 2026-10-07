//go:build linux

package integration

import (
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"pwnmesh/internal/config"
)

// Defaults deliberately preserve the earlier max-effort, four-slot workload.
// Changes are explicit experiment inputs, not silent benchmark improvements.
type liveRuntimeKnobs struct {
	MaxWorkers                                int
	ProjectTimeoutSeconds                     int
	ReasonEffort, CurateEffort, ExploreEffort string
	RequireParallelWorkers                    bool
	ParallelWorkerCount                       int
	KeepContainer                             bool
}

func liveRuntimeOptions(getenv func(string) string) (liveRuntimeKnobs, error) {
	k := liveRuntimeKnobs{MaxWorkers: 4, ProjectTimeoutSeconds: 1800, ParallelWorkerCount: 2, ReasonEffort: getenv("PWNMESH_LIVE_REASON_EFFORT"), CurateEffort: getenv("PWNMESH_LIVE_CURATE_EFFORT"), ExploreEffort: getenv("PWNMESH_LIVE_EXPLORE_EFFORT")}
	if raw := getenv("PWNMESH_LIVE_MAX_WORKERS"); raw != "" {
		var err error
		k.MaxWorkers, err = strconv.Atoi(raw)
		if err != nil || k.MaxWorkers <= 0 {
			return k, fmt.Errorf("PWNMESH_LIVE_MAX_WORKERS must be a positive integer")
		}
	}
	if raw := getenv("PWNMESH_LIVE_PROJECT_TIMEOUT_SECONDS"); raw != "" {
		var err error
		k.ProjectTimeoutSeconds, err = strconv.Atoi(raw)
		if err != nil || k.ProjectTimeoutSeconds <= 0 || k.ProjectTimeoutSeconds > 86400 {
			return k, fmt.Errorf("PWNMESH_LIVE_PROJECT_TIMEOUT_SECONDS must be between 1 and 86400")
		}
	}
	switch getenv("PWNMESH_LIVE_REQUIRE_PARALLEL_WORKERS") {
	case "", "0":
	case "1":
		k.RequireParallelWorkers = true
		if k.MaxWorkers < 3 {
			return k, fmt.Errorf("parallel Worker acceptance requires two execution slots plus one reserved control slot")
		}
	default:
		return k, fmt.Errorf("PWNMESH_LIVE_REQUIRE_PARALLEL_WORKERS must be 0 or 1")
	}
	switch getenv("PWNMESH_LIVE_KEEP_CONTAINER") {
	case "", "0":
	case "1":
		k.KeepContainer = true
	default:
		return k, fmt.Errorf("PWNMESH_LIVE_KEEP_CONTAINER must be 0 or 1")
	}
	if raw := getenv("PWNMESH_LIVE_PARALLEL_WORKER_COUNT"); raw != "" {
		var err error
		k.ParallelWorkerCount, err = strconv.Atoi(raw)
		if err != nil || k.ParallelWorkerCount < 2 {
			return k, fmt.Errorf("PWNMESH_LIVE_PARALLEL_WORKER_COUNT must be an integer of at least two")
		}
	}
	// Config.Validate applies production reasoning-effort validation later.
	return k, nil
}

func (k liveRuntimeKnobs) workloadFingerprint(title, origin, goal, scope string, version int) string {
	base := liveWorkloadFingerprint(title, origin, goal, scope, version, k.RequireParallelWorkers)
	if !k.RequireParallelWorkers || k.ParallelWorkerCount == 2 {
		return base // Preserve the original two-producer experiment identity.
	}
	return liveFingerprint(map[string]any{"base_workload_sha256": base, "parallel_worker_count": k.ParallelWorkerCount})
}

func (k liveRuntimeKnobs) effectiveEfforts() map[string]string {
	result := map[string]string{"reason": k.ReasonEffort, "curate": k.CurateEffort, "explore": k.ExploreEffort}
	for role, effort := range result {
		if effort == "" {
			result[role] = "max" // The harness keeps its original Worker env fallback.
		}
	}
	return result
}

func TestLiveRuntimeOptionsPreserveDefaultsAndBindOverrides(t *testing.T) {
	env := map[string]string{}
	getenv := func(key string) string { return env[key] }
	k, err := liveRuntimeOptions(getenv)
	if err != nil || k.MaxWorkers != 4 || k.ProjectTimeoutSeconds != 1800 || k.RequireParallelWorkers || k.KeepContainer || k.ReasonEffort != "" || k.CurateEffort != "" || k.ExploreEffort != "" || !reflect.DeepEqual(k.effectiveEfforts(), map[string]string{"reason": "max", "curate": "max", "explore": "max"}) {
		t.Fatalf("defaults changed: %#v, %v", k, err)
	}
	env = map[string]string{"PWNMESH_LIVE_REASON_EFFORT": "high", "PWNMESH_LIVE_CURATE_EFFORT": "low", "PWNMESH_LIVE_EXPLORE_EFFORT": "low", "PWNMESH_LIVE_MAX_WORKERS": "3", "PWNMESH_LIVE_REQUIRE_PARALLEL_WORKERS": "1", "PWNMESH_LIVE_PROJECT_TIMEOUT_SECONDS": "3600", "PWNMESH_LIVE_KEEP_CONTAINER": "1"}
	k, err = liveRuntimeOptions(getenv)
	if err != nil || k.MaxWorkers != 3 || k.ProjectTimeoutSeconds != 3600 || !k.RequireParallelWorkers || !k.KeepContainer || !reflect.DeepEqual(k.effectiveEfforts(), map[string]string{"reason": "high", "curate": "low", "explore": "low"}) {
		t.Fatalf("overrides not retained: %#v, %v", k, err)
	}
	for _, bad := range []map[string]string{
		{"PWNMESH_LIVE_MAX_WORKERS": "0"}, {"PWNMESH_LIVE_MAX_WORKERS": "-1"}, {"PWNMESH_LIVE_MAX_WORKERS": "many"},
		{"PWNMESH_LIVE_REQUIRE_PARALLEL_WORKERS": "yes"}, {"PWNMESH_LIVE_REQUIRE_PARALLEL_WORKERS": "1", "PWNMESH_LIVE_MAX_WORKERS": "1"},
		{"PWNMESH_LIVE_REQUIRE_PARALLEL_WORKERS": "1", "PWNMESH_LIVE_MAX_WORKERS": "2"},
		{"PWNMESH_LIVE_PROJECT_TIMEOUT_SECONDS": "0"}, {"PWNMESH_LIVE_PROJECT_TIMEOUT_SECONDS": "-1"},
		{"PWNMESH_LIVE_PROJECT_TIMEOUT_SECONDS": "3600s"}, {"PWNMESH_LIVE_PROJECT_TIMEOUT_SECONDS": "86401"},
		{"PWNMESH_LIVE_PROJECT_TIMEOUT_SECONDS": "99999999999999999999"},
		{"PWNMESH_LIVE_KEEP_CONTAINER": "yes"}, {"PWNMESH_LIVE_KEEP_CONTAINER": "2"},
		{"PWNMESH_LIVE_PARALLEL_WORKER_COUNT": "1"}, {"PWNMESH_LIVE_PARALLEL_WORKER_COUNT": "0"}, {"PWNMESH_LIVE_PARALLEL_WORKER_COUNT": "many"},
	} {
		env = bad
		if _, err = liveRuntimeOptions(getenv); err == nil {
			t.Errorf("accepted invalid experiment inputs: %#v", env)
		}
	}
	baseline := liveExecutionFingerprint(config.Config{})
	if liveExecutionFingerprint(config.Config{}, 1800) != baseline || liveExecutionFingerprint(config.Config{}, 3600) == baseline {
		t.Fatal("project timeout was not bound to the experiment fingerprint")
	}
	for _, tasks := range []config.Tasks{
		{Reason: config.Task{ReasoningEffort: "high"}},
		{Curate: config.Task{ReasoningEffort: "low"}},
		{Explore: config.Task{ReasoningEffort: "low"}},
	} {
		if liveExecutionFingerprint(config.Config{Tasks: tasks}) == baseline {
			t.Fatal("role reasoning effort was not bound to the experiment fingerprint")
		}
	}
}

func TestLiveParallelCardinalityPreservesTwoWorkerBaseline(t *testing.T) {
	getenv := func(string) string { return "" }
	k, err := liveRuntimeOptions(getenv)
	if err != nil || k.ParallelWorkerCount != 2 {
		t.Fatalf("changed default parallel count: %#v, %v", k, err)
	}
	k.RequireParallelWorkers = true
	baseline := liveWorkloadFingerprint("title", "origin", "goal", "scope", 1, true)
	if k.workloadFingerprint("title", "origin", "goal", "scope", 1) != baseline {
		t.Fatal("two-worker acceptance changed baseline fingerprint")
	}
	env := map[string]string{"PWNMESH_LIVE_REQUIRE_PARALLEL_WORKERS": "1", "PWNMESH_LIVE_PARALLEL_WORKER_COUNT": "3"}
	k, err = liveRuntimeOptions(func(key string) string { return env[key] })
	if err != nil || k.ParallelWorkerCount != 3 || k.workloadFingerprint("title", "origin", "goal", "scope", 1) == baseline {
		t.Fatal("three-worker acceptance was not applied and bound to workload")
	}
	k.RequireParallelWorkers = false
	if k.workloadFingerprint("title", "origin", "goal", "scope", 1) != liveWorkloadFingerprint("title", "origin", "goal", "scope", 1) {
		t.Fatal("disabled parallel acceptance changed baseline fingerprint")
	}
}
