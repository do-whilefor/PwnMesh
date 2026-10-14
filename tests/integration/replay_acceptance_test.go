//go:build linux

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
	"pwnmesh/internal/workergraph"
)

// Revalidate original live evidence after a validator change without another
// model call or any mutation of the retained state and workspace archive.
func TestRetainedAssetGraphProject(t *testing.T) {
	dir := os.Getenv("PWNMESH_ASSET_GRAPH_REPLAY_DIR")
	if dir == "" {
		t.Skip("opt in with the directory containing retained state.json and workspace.tar")
	}
	state, files, stateSHA256, workspaceSHA256 := readRetainedLiveEvidence(t, dir)
	t.Logf("validation_scope=asset_graph_worker_trace_and_frozen_input workspace_sha256=%s state_sha256=%s", workspaceSHA256, stateSHA256)
	if failures := validateAssetGraphDelivery(state, files); len(failures) != 0 {
		t.Fatal(failures)
	}
}

func TestRetainedReleaseAuditProject(t *testing.T) {
	dir := os.Getenv("PWNMESH_RELEASE_AUDIT_REPLAY_DIR")
	if dir == "" {
		t.Skip("opt in with retained state.json and workspace.tar directory")
	}
	state, files, _, _ := readRetainedLiveEvidence(t, dir)
	if failures := validateReleaseAuditDelivery(state, files); len(failures) != 0 {
		t.Fatal(failures)
	}
	requestsRaw, err := os.ReadFile(filepath.Join(dir, "release-http-requests.json"))
	if os.IsNotExist(err) {
		// Archives produced before HTTP instrumentation can still be checked
		// for content/graph integrity, but do not prove the network attempts.
		t.Log("HTTP attempt verification unavailable: original harness did not retain request observations")
		return
	}
	var requests map[string]int
	if err != nil || json.Unmarshal(requestsRaw, &requests) != nil {
		t.Fatal("cannot read retained fixture HTTP observations", err)
	}
	if failures := validateReleaseAuditHTTPRequests(requests); len(failures) != 0 {
		t.Fatal(failures)
	}
}

// This is a separate acceptance scope. It never rewrites validation.json or
// promotes a workload that violated the original fixed-key contract to PASS.
func TestRetainedReleaseAuditRecoveryProject(t *testing.T) {
	dir := os.Getenv("PWNMESH_RELEASE_AUDIT_RECOVERY_DIR")
	if dir == "" {
		t.Skip("opt in to independent recovery-path acceptance on retained evidence")
	}
	state, files, stateSHA256, workspaceSHA256 := readRetainedLiveEvidence(t, dir)
	result := validateReleaseAuditRecoveryDelivery(state, files)
	result.StateSHA256 = stateSHA256
	result.WorkspaceSHA256 = workspaceSHA256
	if original, readErr := os.ReadFile(filepath.Join(dir, "validation.json")); readErr == nil {
		result.OriginalValidationSHA256 = fmt.Sprintf("%x", sha256.Sum256(original))
	}
	requestsRaw, err := os.ReadFile(filepath.Join(dir, "release-http-requests.json"))
	var requests map[string]int
	if err != nil || json.Unmarshal(requestsRaw, &requests) != nil {
		result.Failures = append(result.Failures, "recovery acceptance requires retained fixture HTTP observations")
	} else {
		failures := validateReleaseAuditHTTPRequests(requests)
		result.Failures = append(result.Failures, failures...)
		result.HTTPAttemptsVerified = len(failures) == 0
	}
	result.Passed = len(result.Failures) == 0
	if err = saveLiveJSON(filepath.Join(dir, "recovery-validation.json"), result); err != nil {
		t.Fatal(err)
	}
	t.Logf("original_strict_passed=%v independent_recovery_passed=%v paths=%+v", result.OriginalStrictPassed, result.Passed, result.Paths)
	if !result.Passed {
		t.Fatal(result.Failures)
	}
}

func TestRetainedReleaseAuditReuseProject(t *testing.T) {
	dir := os.Getenv("PWNMESH_RELEASE_AUDIT_REUSE_REPLAY_DIR")
	if dir == "" {
		t.Skip("opt in with retained state.json, workspace.tar and release-http-requests.json")
	}
	state, files, _, _ := readRetainedLiveEvidence(t, dir)
	requestRaw, err := os.ReadFile(filepath.Join(dir, "release-http-requests.json"))
	if err != nil {
		t.Fatal(err)
	}
	var requests map[string]int
	if err = json.Unmarshal(requestRaw, &requests); err != nil {
		t.Fatal(err)
	}
	failures := append(validateReleaseAuditDeliveryWithGraphs(state, files, validateReleaseAuditReuseGraph), validateReleaseAuditReuseHTTP(requests)...)
	failures = append(failures, validateReleaseAuditReuseWrites(state, files)...)
	if len(failures) != 0 {
		t.Fatal(failures)
	}
}

// Recheck producer separation, curation and actual successful model overlap
// from retained evidence, without another model call or changing that evidence.
func TestRetainedScenarioSkillsReviewProject(t *testing.T) {
	dir := os.Getenv("PWNMESH_SCENARIO_REVIEW_REPLAY_DIR")
	if dir == "" {
		t.Skip("opt in with the directory containing retained state.json and workspace.tar")
	}
	readJSON := func(name string, value any) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, value); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
	}
	var manifest struct{ Scenario string }
	var oracle scenarioReviewOracle
	var observations struct{ Statuses, Requests map[string]int }
	readJSON("manifest.json", &manifest)
	readJSON("fixture-oracle.json", &oracle)
	readJSON("fixture-observations.json", &observations)
	if (manifest.Scenario != "audit" && manifest.Scenario != "pentest") || len(oracle.Source) == 0 || oracle.Nonce == "" || len(oracle.Bodies) != 4 {
		t.Fatal("missing retained scenario or synthetic fixture oracle")
	}
	fixture := &scenarioSkillFixture{seen: observations.Statuses, reads: observations.Requests, source: oracle.Source, nonce: oracle.Nonce, bodies: oracle.Bodies}
	state, files, stateSHA256, workspaceSHA256 := readRetainedLiveEvidence(t, dir)
	var job worker.Job
	run := orchestrationRunID(state.Curation.RunID)
	if json.Unmarshal(files["/workspace/.pwnmesh/runs/"+run+"/job.json"], &job) == nil && job.InputSnapshot != nil {
		id := job.InputSnapshot.ID
		if decoded, err := hex.DecodeString(id); err != nil || len(decoded) != sha256.Size {
			t.Fatal("invalid retained curation snapshot ID")
		}
		raw, err := os.ReadFile(filepath.Join(dir, "input-snapshots", id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		files[curationSnapshotKey(id)] = raw
	}
	t.Logf("validation_scope=scenario_skills_review_and_parallel workspace_sha256=%s state_sha256=%s", workspaceSHA256, stateSHA256)
	failures := validateScenarioReview(state, files, manifest.Scenario, scenarioSkillBundle(t, manifest.Scenario), fixture)
	parallel, parallelFailures := validateLiveParallelWorkers(state, files, 2)
	failures = append(failures, parallelFailures...)
	t.Logf("independent_workers=%d model_overlap_seconds=%.6f", len(parallel.Workers), parallel.OverlapSeconds)
	if len(failures) != 0 {
		t.Fatal(failures)
	}
}

// Three paired runs isolate command scheduling from provider latency. They
// measure the real runtime tool in the same Docker image as the live workload;
// this is not an estimate of whole-project speedup.
func TestWorkerCommandGraphTiming(t *testing.T) {
	out := os.Getenv("PWNMESH_WORKER_GRAPH_TIMING_OUTPUT")
	if out == "" {
		t.Skip("opt in with an output path for controlled scheduling measurements")
	}
	var measurements []map[string]any
	join := `python3 - <<'PY'
import json, os, sys
deps = {d['id']: d for d in json.load(open(os.environ['PWNMESH_DEPENDENCIES']))}
values = []
for name in ('left', 'right'):
    path = deps[name]['output']['value']['output_path']
    sys.stderr.write(name + ' stdout=' + path + '\n')
    values.extend(json.load(open(path)))
print(json.dumps({'values': values, 'sum': sum(values)}))
PY`
	for repeat := 1; repeat <= 3; repeat++ {
		// Alternate order to avoid always giving one setting the warm run.
		order := []int{1, 2}
		if repeat%2 == 0 {
			order = []int{2, 1}
		}
		for _, parallelism := range order {
			workspace := t.TempDir()
			run := fmt.Sprintf("trial-%d-p%d", repeat, parallelism)
			dir := filepath.Join(workspace, ".pwnmesh", "runs", run)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			j := worker.Job{RunID: run, Kind: "explore", Workspace: workspace, Graph: board.Graph{Project: board.Project{ID: "timing", OrchestrationVersion: 1}}}
			o := worker.Options{RunDir: dir}
			if err := worker.ConfigureRuntimeTools(j, &o); err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(o.Tools, func(tool agent.Tool) bool { return tool.Name == "run_graph" })
			if i < 0 {
				t.Fatal("command graph is not exposed by the production Worker")
			}
			raw, _ := json.Marshal(map[string]any{"key": "sample-sum", "parallelism": parallelism, "nodes": []map[string]any{
				{"id": "left", "command": "sleep 1; printf '[2,3]\\n'", "resources": []string{}},
				{"id": "right", "command": "sleep 1; printf '[5]\\n'", "resources": []string{}},
				{"id": "sum", "depends_on": []workergraph.Dependency{{ID: "left"}, {ID: "right"}}, "command": join, "resources": []string{}},
			}})
			started := time.Now()
			result, err := o.Tools[i].Execute(context.Background(), raw)
			wall := time.Since(started).Seconds()
			if err != nil {
				t.Fatalf("graph tool failed: %s: %v", result, err)
			}
			base := filepath.Join(dir, "graph-tools", "sample-sum") + "/"
			cpBytes, err := os.ReadFile(base + "graph.json")
			if err != nil {
				t.Fatal(err)
			}
			var cp workergraph.Checkpoint
			if err = json.Unmarshal(cpBytes, &cp); err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{}
			for _, id := range []string{"left", "right", "sum"} {
				for _, name := range []string{"stdout.log", "stderr.log", "dependencies.json"} {
					path := base + "nodes/" + id + "/" + name
					files[path], err = os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			failures := validateCommandGraphCheckpoint(cp, run, base, files)
			if parallelism == 1 {
				if !slices.Contains(failures, "independent input commands did not overlap") {
					t.Fatal("serial configuration unexpectedly overlapped")
				}
				failures = slices.DeleteFunc(failures, func(s string) bool { return s == "independent input commands did not overlap" })
			}
			if len(failures) != 0 {
				t.Fatal(failures)
			}
			for _, id := range []string{"left", "right", "sum"} {
				want := ""
				if id == "sum" {
					want = "left stdout=" + base + "nodes/left/stdout.log\nright stdout=" + base + "nodes/right/stdout.log\n"
				}
				if actual := string(files[base+"nodes/"+id+"/stderr.log"]); actual != want {
					t.Fatalf("%s stderr = %q, want %q", id, actual, want)
				}
			}
			measurements = append(measurements, map[string]any{"repeat": repeat, "parallelism": parallelism, "wall_seconds": wall, "checkpoint": cp, "passed": true})
			t.Logf("repeat=%d parallelism=%d tool_wall_seconds=%.3f", repeat, parallelism, wall)
		}
	}
	if err := saveLiveJSON(out, map[string]any{"scope": "controlled command graph only; three matched pairs, not project or model latency", "measurements": measurements}); err != nil {
		t.Fatal(err)
	}
}

// Recheck the original live archive without contacting a model or modifying
// retained evidence. Use exactly the same delivery validator as the live run.
func TestRetainedWorkerMixedGraphProject(t *testing.T) {
	dir := os.Getenv("PWNMESH_WORKER_MIXED_GRAPH_REPLAY_DIR")
	if dir == "" {
		t.Skip("opt in with the directory containing retained state.json and workspace.tar")
	}
	state, files, stateSHA256, workspaceSHA256 := readRetainedLiveEvidence(t, dir)
	t.Logf("validation_scope=worker_dynamic_mixed_graph_with_isolated_loops workspace_sha256=%s state_sha256=%s", workspaceSHA256, stateSHA256)
	if failures := validateWorkerMixedGraphDelivery(state, files); len(failures) != 0 {
		t.Fatal(failures)
	}
}
