//go:build linux

package integration

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/dispatcher"
	"pwnmesh/internal/docker"
	"pwnmesh/internal/server"
	"pwnmesh/internal/worker"
)

type liveRunObservation struct {
	RunID         string    `json:"run_id"`
	Kind          string    `json:"kind"`
	StepID        string    `json:"step_id,omitempty"`
	Started       time.Time `json:"started"`
	Finished      time.Time `json:"finished,omitempty"`
	InputRevision int64     `json:"input_revision"`
	InputVersion  string    `json:"input_version"`
	Status        string    `json:"status,omitempty"`
	FailureKind   string    `json:"failure_kind,omitempty"`
	RunnerError   bool      `json:"runner_error,omitempty"`
}

type liveObservedRunner struct {
	*docker.Client
	mu   sync.Mutex
	runs []liveRunObservation
}

func (r *liveObservedRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	observation := liveRunObservation{RunID: job.RunID, Kind: job.Kind, Started: time.Now().UTC()}
	if job.Intent != nil {
		observation.StepID = job.Intent.ID
	}
	if job.InputSnapshot != nil {
		observation.InputRevision, observation.InputVersion = job.InputSnapshot.Revision, job.InputSnapshot.StateVersion
	} else if job.State != nil {
		observation.InputRevision, observation.InputVersion = job.State.Revision, board.DecisionStateVersion(*job.State)
	}
	r.mu.Lock()
	index := len(r.runs)
	r.runs = append(r.runs, observation)
	r.mu.Unlock()
	result, err := r.Client.Run(ctx, backend, job)
	observation.Finished, observation.Status, observation.FailureKind, observation.RunnerError = time.Now().UTC(), result.Status, result.FailureKind, err != nil
	r.mu.Lock()
	r.runs[index] = observation
	r.mu.Unlock()
	return result, err
}

func (r *liveObservedRunner) snapshot() []liveRunObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]liveRunObservation{}, r.runs...)
}

func saveLiveJSON(path string, data any) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0600)
}

// Fingerprints describe controlled inputs, not secrets or per-run identities.
// The same harness must be used on both source revisions in a timing study.
func liveFingerprint(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err) // All callers supply JSON-compatible harness metadata.
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func liveWorkloadFingerprint(title, origin, goal, scope string, version int, parallelAcceptance ...bool) string {
	inputs := map[string]any{
		"title": title, "origin": origin, "goal": goal, "validation_scope": scope,
		"orchestration_version": version, "bootstrap_enabled": false,
	}
	if len(parallelAcceptance) > 0 && parallelAcceptance[0] {
		inputs["require_parallel_workers"] = true
	}
	return liveFingerprint(inputs)
}

func liveExecutionFingerprint(c config.Config, projectTimeoutSeconds ...int) string {
	timeout := 1800
	if len(projectTimeoutSeconds) > 0 {
		timeout = projectTimeoutSeconds[0]
	}
	workers := make([]map[string]any, 0, len(c.Workers))
	for _, w := range c.Workers {
		env := map[string]string{}
		for _, key := range []string{"PWNMESH_REASONING_EFFORT", "PWNMESH_REQUEST_TIMEOUT", "PWNMESH_MAX_OUTPUT_TOKENS", "PWNMESH_CONTEXT_TOKENS", "PWNMESH_CONTEXT_TARGET_TOKENS", "PWNMESH_CONTEXT_BYTES"} {
			env[key] = w.Env[key]
		}
		workers = append(workers, map[string]any{"type": w.Type, "task_types": w.TaskTypes, "max_running": w.MaxRunning, "priority": w.Priority, "env": env})
	}
	network := c.Container.Network
	if strings.HasPrefix(network, "container:") {
		network = "container:<controller>"
	}
	return liveFingerprint(map[string]any{
		"runtime": c.Runtime, "tasks": c.Tasks, "workers": workers,
		"network": network, "cap_add": c.Container.CapAdd, "completed_action": c.Container.CompletedAction,
		"project_timeout_seconds": timeout, "observation_interval_seconds": 2,
	})
}

func TestLiveComparisonFingerprintsBindInputsWithoutRunSecrets(t *testing.T) {
	want := liveWorkloadFingerprint("title", "origin", "goal", "scope", 1)
	for _, changed := range []string{
		liveWorkloadFingerprint("other", "origin", "goal", "scope", 1),
		liveWorkloadFingerprint("title", "other", "goal", "scope", 1),
		liveWorkloadFingerprint("title", "origin", "other", "scope", 1),
		liveWorkloadFingerprint("title", "origin", "goal", "other", 1),
		liveWorkloadFingerprint("title", "origin", "goal", "scope", 0),
		liveWorkloadFingerprint("title", "origin", "goal", "scope", 1, true),
	} {
		if changed == want {
			t.Fatal("different workload shared a fingerprint")
		}
	}
	if liveWorkloadFingerprint("title", "origin", "goal", "scope", 1, false) != want {
		t.Fatal("disabled optional acceptance changed the baseline workload fingerprint")
	}
	c := config.Config{Runtime: config.Runtime{MaxWorkers: 4}, Container: config.Container{Network: "container:first", Image: "before"}, Workers: []config.Worker{{Type: "go", MaxRunning: 4, Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "first-secret", "ANTHROPIC_BASE_URL": "http://first-proxy", "PWNMESH_REQUEST_TIMEOUT": "180"}}}}
	want = liveExecutionFingerprint(c)
	c.Container.Network, c.Container.Image, c.Container.Namespace = "container:second", "after", "second"
	c.Server = "http://second-server"
	c.Workers[0].Env["ANTHROPIC_AUTH_TOKEN"] = "second-secret"
	c.Workers[0].Env["ANTHROPIC_BASE_URL"] = "http://second-proxy"
	if liveExecutionFingerprint(c) != want {
		t.Fatal("credentials or ephemeral identity changed execution fingerprint")
	}
	c.Workers[0].Env["PWNMESH_REQUEST_TIMEOUT"] = "60"
	if liveExecutionFingerprint(c) == want {
		t.Fatal("request timeout was not bound")
	}
	c.Workers[0].Env["PWNMESH_REQUEST_TIMEOUT"] = "180"
	c.Runtime.MaxWorkers++
	if liveExecutionFingerprint(c) == want {
		t.Fatal("concurrency was not bound")
	}
	c.Runtime.MaxWorkers--
	c.Tasks.Curate.Timeout = 30
	if liveExecutionFingerprint(c) == want {
		t.Fatal("curation timeout was not bound")
	}
}

// This test runs the production Server, Scheduler, Docker bridge and Worker.
// The optional proxy only observes upstream bytes; every model response is real.
// Direct mode preserves the configured hostname for provider-specific behavior.
// Local fixture data is the only task content sent to the model service.
func TestLiveContentionProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_CONTENTION_TEST") != "1" {
		t.Skip("opt in with model configuration, PWNMESH_DOCKER_TEST_IMAGE and PWNMESH_LIVE_OUTPUT")
	}
	origin, goal := liveContentionTask()
	runObservedProject(t, "Live concurrent transaction audit", origin, goal, "business_acceptance", validateLiveContention)
}

// Reuse the production lifecycle and evidence collection for independently
// validated workloads. Workload validators must not trust project completion.
func runObservedProject(t *testing.T, title, origin, goal, validationScope string, validate func(board.State, map[string][]byte) []string, orchestrationVersion ...int) {
	t.Helper()
	image, output := os.Getenv("PWNMESH_DOCKER_TEST_IMAGE"), os.Getenv("PWNMESH_LIVE_OUTPUT")
	base, token, model := os.Getenv("ANTHROPIC_BASE_URL"), os.Getenv("ANTHROPIC_AUTH_TOKEN"), os.Getenv("ANTHROPIC_DEFAULT_FABLE_MODEL")
	if selected := os.Getenv("ANTHROPIC_MODEL"); selected != "" {
		model = selected
	}
	if image == "" || output == "" || base == "" || token == "" || model == "" {
		t.Fatal("live test requires explicit image, output and model configuration")
	}
	knobs, err := liveRuntimeOptions(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	mode := 1
	if len(orchestrationVersion) > 0 {
		mode = orchestrationVersion[0]
	}
	webAddress := os.Getenv("PWNMESH_LIVE_WEB_ADDR")
	var webGate *liveWebCreationGate
	var webWait time.Duration
	if webAddress != "" {
		webWait, err = liveWebWaitDuration(os.Getenv("PWNMESH_LIVE_WEB_WAIT_SECONDS"))
		if err != nil {
			t.Fatal(err)
		}
		webGate = newLiveWebCreationGate(liveWebTask{Title: title, Origin: origin, Goal: goal, Scenario: "pentest", OrchestrationVersion: mode})
		if err = saveLiveJSON(filepath.Join(output, "web-task.json"), webGate.want); err != nil {
			t.Fatal(err)
		}
	}
	publicUpstream, err := url.Parse(base)
	if err != nil || publicUpstream == nil || (publicUpstream.Scheme != "http" && publicUpstream.Scheme != "https") || publicUpstream.Host == "" || publicUpstream.User != nil {
		t.Fatal("invalid live model upstream")
	}
	observationMode, observationReason, err := liveModelObservationMode(base, os.Getenv("PWNMESH_LIVE_DIRECT_MODEL"))
	if err != nil {
		t.Fatal(err)
	}
	workerBase, workerToken := base, token
	observations := &liveProxyRecorder{}
	if observationMode == "proxy" {
		proxy, recorder, err := newLiveModelProxy(base, token)
		if err != nil {
			t.Fatal("invalid live proxy configuration")
		}
		defer proxy.Close()
		workerBase, workerToken, observationMode = proxy.URL, "local-observation-proxy", "proxy"
		observations = recorder
	} else {
		t.Logf("model observation mode=direct reason=%s; provider URL preserved, proxy HTTP observations unavailable", observationReason)
	}
	store, err := board.Open(filepath.Join(output, "project.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	apiMetrics := &auditAPIRecorder{}
	handler := apiMetrics.wrap(server.New(store))
	if webGate != nil {
		handler = webGate.wrap(handler)
	}
	api, err := newLiveObservedAPI(handler, webAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	namespace := fmt.Sprintf("pwnmesh-live-contention-%d", time.Now().UnixNano())
	c := config.Config{
		Server:    api.URL,
		Runtime:   config.Runtime{Interval: 1, MaxWorkers: knobs.MaxWorkers, MaxProjects: 1, MaxProjectWorkers: knobs.MaxWorkers, HealthMode: "disabled", HealthTimeout: 30},
		Tasks:     config.Tasks{Reason: config.Task{Timeout: 300, MaxIntents: 3, ReasoningEffort: knobs.ReasonEffort}, Curate: config.Task{Timeout: 300, ReasoningEffort: knobs.CurateEffort}, Explore: config.Task{Timeout: 0, ConcludeTimeout: 60, ReasoningEffort: knobs.ExploreEffort}},
		Container: config.Container{Image: image, Network: testContainerNetwork(t), Namespace: namespace, CompletedAction: "stop"},
		Workers: []config.Worker{{Name: "live", Type: "go", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: knobs.MaxWorkers, Env: map[string]string{
			"ANTHROPIC_BASE_URL": workerBase, "ANTHROPIC_AUTH_TOKEN": workerToken, "ANTHROPIC_MODEL": model,
			"PWNMESH_REASONING_EFFORT": "max", "PWNMESH_REQUEST_TIMEOUT": "180", "PWNMESH_MAX_OUTPUT_TOKENS": "384000",
			"PWNMESH_CONTEXT_TOKENS": "920000", "PWNMESH_CONTEXT_TARGET_TOKENS": "250000", "PWNMESH_CONTEXT_BYTES": "8388608",
		}}},
	}
	if err = c.Validate(); err != nil {
		t.Fatal(err)
	}
	runner := &liveObservedRunner{Client: docker.New(c.Container)}
	defer runner.Close()
	client := &dispatcher.Client{Base: api.URL}
	var graph board.Graph
	started := time.Now().UTC()
	var webCreation liveWebCreation
	if webGate == nil {
		if err = client.Do(context.Background(), "POST", "/projects", map[string]any{"title": title, "origin": origin, "goal": goal, "bootstrap_enabled": false, "orchestration_version": mode}, &graph, nil); err != nil {
			t.Fatal(err)
		}
	} else {
		var projects []board.Summary
		if err = client.Do(context.Background(), "GET", "/projects", nil, &projects, nil); err != nil || len(projects) != 0 {
			t.Fatal("Web acceptance requires an empty project store", err)
		}
		ready := map[string]any{"state": "waiting_for_web_creation", "api_url": api.URL, "listen_address": webAddress, "task_file": "web-task.json", "ready_at": webGate.snapshot().ReadyAt, "wait_seconds": webWait.Seconds(), "project_timeout_seconds": knobs.ProjectTimeoutSeconds, "creation_source": "external_http_only_no_harness_create", "timing_basis": "project starts at the observed POST /projects request; earlier browser preparation is excluded"}
		if err = saveLiveJSON(filepath.Join(output, "web-ready.json"), ready); err != nil {
			t.Fatal(err)
		}
		t.Logf("Web ready at %s; create the exact web-task.json project within %s", api.URL, webWait)
		waitCtx, waitCancel := context.WithTimeout(context.Background(), webWait)
		webCreation, err = webGate.wait(waitCtx)
		waitCancel()
		if saveErr := saveLiveJSON(filepath.Join(output, "web-creation.json"), webCreation); saveErr != nil {
			t.Fatal(saveErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		graph, started = webCreation.Graph, webCreation.RequestStarted
		if err = client.Do(context.Background(), "GET", "/projects", nil, &projects, nil); err != nil || len(projects) != 1 || projects[0].ID != graph.Project.ID {
			t.Fatal("expected exactly the observed original Web project", err)
		}
		var current board.Graph
		if err = client.Do(context.Background(), "GET", "/projects/"+graph.Project.ID, nil, &current, nil); err != nil {
			t.Fatal(err)
		}
		if err = matchLiveWebProject(current, webGate.want); err != nil {
			t.Fatal(err)
		}
		if err = webGate.activate(); err != nil {
			t.Fatal(err)
		}
	}
	pid := graph.Project.ID
	container := namespace + "-dispatch-" + pid
	publicUpstream.User, publicUpstream.RawQuery, publicUpstream.Fragment = nil, "", ""
	manifest := map[string]any{"project_id": pid, "started": started, "model": model, "upstream": publicUpstream.String(), "reasoning_effort": "max", "reasoning_effort_by_role": knobs.effectiveEfforts(), "request_timeout_seconds": 180, "decision_timeout_seconds": 300, "max_workers": c.Runtime.MaxWorkers, "max_project_workers": c.Runtime.MaxProjectWorkers, "worker_max_running": c.Workers[0].MaxRunning, "require_parallel_workers": knobs.RequireParallelWorkers, "source_commit": os.Getenv("PWNMESH_SOURCE_COMMIT"), "image": image, "namespace": namespace, "healthcheck": "disabled", "cost_status": "unknown_no_verified_account_pricing", "scope": "synthetic local files; real model, scheduler and Docker workers"}
	manifest["http_observation_mode"] = observationMode
	manifest["http_observation_reason"] = observationReason
	manifest["orchestration_version"] = mode
	manifest["workload_title"], manifest["validation_scope"] = title, validationScope
	manifest["comparison_protocol_version"] = 1
	manifest["workload_sha256"] = knobs.workloadFingerprint(title, origin, goal, validationScope, mode)
	manifest["parallel_worker_count"] = knobs.ParallelWorkerCount
	if webGate != nil {
		manifest["creation_source"] = webCreation.Source
		manifest["browser_preparation_seconds_excluded"] = started.Sub(webCreation.ReadyAt).Seconds()
		manifest["web_ready_at"], manifest["web_creation_response_at"] = webCreation.ReadyAt, webCreation.ResponseAt
		manifest["scenario"] = webGate.want.Scenario
		// The existing API-created workload omitted scenario. Bind the new
		// Web scenario explicitly without changing historical fingerprints.
		manifest["workload_sha256"] = liveFingerprint(map[string]any{"base_workload_sha256": manifest["workload_sha256"], "scenario": webGate.want.Scenario, "creation_source": webCreation.Source})
	}
	manifest["project_timeout_seconds"] = knobs.ProjectTimeoutSeconds
	manifest["keep_container"] = knobs.KeepContainer
	manifest["container_name"] = container
	manifest["execution_knobs_sha256"] = liveExecutionFingerprint(c, knobs.ProjectTimeoutSeconds)
	if err = saveLiveJSON(filepath.Join(output, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), started.Add(time.Duration(knobs.ProjectTimeoutSeconds)*time.Second))
	done := make(chan struct{})
	var schedulerErr error
	manifest["dispatcher_started"] = time.Now().UTC()
	if err = saveLiveJSON(filepath.Join(output, "manifest.json"), manifest); err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() { schedulerErr = dispatcher.New(c, runner).Run(ctx); close(done) }()
	completed := false
	defer func() {
		if !completed {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
			_ = client.Do(stopCtx, "PUT", "/projects/"+pid+"/status", map[string]string{"status": "stopped"}, nil, nil)
			stopCancel()
		}
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("scheduler shutdown exceeded 30 seconds")
		}
		collectCtx, collectCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer collectCancel()
		collectionStarted := time.Now()
		var state board.State
		if err := client.Do(collectCtx, "GET", "/projects/"+pid+"/state", nil, &state, nil); err != nil {
			t.Error(err)
		}
		_ = saveLiveJSON(filepath.Join(output, "state.json"), state)
		var events []board.StateEvent
		if err := client.Do(collectCtx, "GET", "/projects/"+pid+"/state/events?after=0", nil, &events, nil); err != nil {
			t.Error(err)
		} else if err := saveLiveJSON(filepath.Join(output, "state-events.json"), events); err != nil {
			t.Error(err)
		}
		if executions, err := testExecutions(collectCtx, store, namespace); err != nil {
			t.Error(err)
		} else if err := saveLiveJSON(filepath.Join(output, "executions.json"), executions); err != nil {
			t.Error(err)
		}
		if err := saveLiveJSON(filepath.Join(output, "runs.json"), runner.snapshot()); err != nil {
			t.Error(err)
		}
		if err := saveLiveJSON(filepath.Join(output, "http-observations.json"), observations.Snapshot()); err != nil {
			t.Error(err)
		}
		if err := saveLiveJSON(filepath.Join(output, "api-observations.json"), apiMetrics.snapshot()); err != nil {
			t.Error(err)
		}
		files, archiveErr := collectLiveWorkspace(collectCtx, container, output)
		if archiveErr != nil {
			t.Error(archiveErr)
		}
		if files != nil {
			if err := retainCurationSnapshots(collectCtx, store, pid, files, output); err != nil {
				t.Error(err)
			}
		}
		failures := validate(state, files)
		if knobs.RequireParallelWorkers {
			parallel, parallelFailures := validateLiveParallelWorkers(state, files, knobs.ParallelWorkerCount)
			failures = append(failures, parallelFailures...)
			if err := saveLiveJSON(filepath.Join(output, "parallel-validation.json"), parallel); err != nil {
				t.Error(err)
			}
		}
		manifest["validation_finished"] = time.Now().UTC()
		manifest["collection_and_validation_seconds"] = time.Since(collectionStarted).Seconds()
		manifest["time_to_acceptance_seconds"] = time.Since(started).Seconds()
		manifest["acceptance_passed"] = completed && len(failures) == 0
		if err := saveLiveJSON(filepath.Join(output, "validation.json"), map[string]any{"validation_scope": validationScope, "project_completed": completed, "failures": failures, "passed": completed && len(failures) == 0}); err != nil {
			t.Error(err)
		}
		if len(failures) > 0 {
			t.Errorf("%s: %v", validationScope, failures)
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		cleanupStarted := time.Now()
		cleanupState := "deleted"
		if knobs.KeepContainer {
			// Stop execution but retain the exact Linux workspace for inspection.
			cleanupState = "stopped"
		}
		if err := runner.Cleanup(cleanupCtx, pid, cleanupState); err != nil {
			t.Error(err)
		}
		manifest["cleanup_seconds"] = time.Since(cleanupStarted).Seconds()
		_ = saveLiveJSON(filepath.Join(output, "manifest.json"), manifest)
	}()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	last := ""
	lastNotice := time.Time{}
	for {
		select {
		case <-done:
			t.Fatalf("scheduler stopped before project completion: %v", schedulerErr)
		case <-ctx.Done():
			t.Fatalf("live project exceeded its %d second acceptance limit", knobs.ProjectTimeoutSeconds)
		case <-tick.C:
			var state board.State
			if err = client.Do(ctx, "GET", "/projects/"+pid+"/state", nil, &state, nil); err != nil {
				t.Fatal(err)
			}
			statuses := []string{}
			for _, step := range state.Steps {
				statuses = append(statuses, step.ID+":"+step.Status)
			}
			progress := fmt.Sprintf("status=%s revision=%d facts=%d steps=%v", state.Graph.Project.Status, state.Revision, len(state.FactRecords), statuses)
			if progress != last || time.Since(lastNotice) > 30*time.Second {
				t.Logf("elapsed=%.1fs %s", time.Since(started).Seconds(), progress)
				_ = saveLiveJSON(filepath.Join(output, "progress.json"), map[string]any{"at": time.Now().UTC(), "elapsed_seconds": time.Since(started).Seconds(), "progress": progress, "runs": runner.snapshot()})
				_ = saveLiveJSON(filepath.Join(output, "http-observations.json"), observations.Snapshot())
				last, lastNotice = progress, time.Now()
			}
			if state.Graph.Project.Status == "completed" {
				completed = true
				manifest["completed_observed"] = time.Now().UTC()
				manifest["project_wall_seconds"] = time.Since(started).Seconds()
				_ = saveLiveJSON(filepath.Join(output, "manifest.json"), manifest)
				// Allow the final receipt to settle before cancellation/export.
				for n := 0; n < 20; n++ {
					active := false
					for _, run := range runner.snapshot() {
						active = active || run.Finished.IsZero()
					}
					if !active {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
				return
			}
			if state.Graph.Project.Status != "active" {
				t.Fatalf("project stopped before completion: %s", state.Graph.Project.Status)
			}
		}
	}
}

func collectLiveWorkspace(ctx context.Context, container, output string) (map[string][]byte, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+url.PathEscape(container)+"/archive?path=%2Fworkspace", nil)
	if err != nil {
		return nil, err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("workspace archive HTTP %d", response.StatusCode)
	}
	return retainLiveWorkspace(response.Body, output)
}

// The original archive is authoritative when the output filesystem cannot
// represent every Linux filename (for example, names differing only in case).
func retainLiveWorkspace(source io.Reader, output string) (files map[string][]byte, err error) {
	if err = os.MkdirAll(output, 0700); err != nil {
		return nil, err
	}
	archive, err := os.OpenFile(filepath.Join(output, "workspace.tar"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	stream := io.TeeReader(source, archive)
	defer func() {
		// tar.Reader stops at its end markers, before any remaining padding.
		// Also retain the unread archive when extraction fails partway through.
		_, drainErr := io.Copy(io.Discard, stream)
		err = errors.Join(err, drainErr, archive.Close())
	}()
	files = map[string][]byte{}
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return files, err
		}
		name := filepath.Clean(header.Name)
		if !strings.HasPrefix(name, "workspace/") || strings.Contains(name, "..") || header.Typeflag != tar.TypeReg {
			continue
		}
		if header.Size > 64<<20 {
			return files, fmt.Errorf("oversized retained file %q", name)
		}
		raw, err := io.ReadAll(reader)
		if err != nil {
			return files, err
		}
		target := filepath.Join(output, name)
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return files, err
		}
		if err = os.WriteFile(target, raw, 0600); err != nil {
			return files, err
		}
		files["/"+name] = raw
	}
	return files, nil
}
