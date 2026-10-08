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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/docker"
	"pwnmesh/internal/worker"
)

func TestDockerInterruptedRunResumesWithoutRepeatingSideEffects(t *testing.T) {
	testDockerRunResumesWithoutRepeatingSideEffects(t, false)
}

func TestDockerWorkerSIGKILLResumesWithoutRepeatingSideEffects(t *testing.T) {
	testDockerRunResumesWithoutRepeatingSideEffects(t, true)
}

func testDockerRunResumesWithoutRepeatingSideEffects(t *testing.T, hardCrash bool) {
	t.Helper()
	image := os.Getenv("PWNMESH_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set PWNMESH_DOCKER_TEST_IMAGE for real interruption acceptance")
	}
	const runID = "interrupted-run"
	const projectID = "interruption"
	const runDir = "/workspace/.pwnmesh/runs/" + runID
	var mu sync.Mutex
	requests := 0
	var modelErrors []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var input struct {
			Messages []agent.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			modelErrors = append(modelErrors, err.Error())
			http.Error(w, "bad controlled request", 400)
			return
		}
		requests++
		respond := func(block agent.Block, stop string) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"role": "assistant", "content": []agent.Block{block}, "stop_reason": stop})
		}
		tool := func(command string) {
			raw, _ := json.Marshal(map[string]any{"command": command, "timeout": 60})
			respond(agent.Block{Type: "tool_use", ID: fmt.Sprintf("action-%d", requests), Name: "bash", Input: raw}, "tool_use")
		}
		switch requests {
		case 1:
			tool("printf 'completed-once\\n' >> " + runDir + "/side-effects.txt")
		case 2:
			if len(input.Messages) < 3 || input.Messages[len(input.Messages)-1].Content[0].IsError {
				modelErrors = append(modelErrors, "first side effect did not complete")
			}
			tool("sleep 60 & echo $! > " + runDir + "/child.pid; echo $$ > " + runDir + "/shell.pid; wait")
		case 3:
			if len(input.Messages) != 5 {
				modelErrors = append(modelErrors, fmt.Sprintf("restored unexpected history length: %d", len(input.Messages)))
			} else {
				last := input.Messages[4]
				if len(last.Content) != 1 || last.Content[0].ToolUseID != "action-2" || !last.Content[0].IsError {
					modelErrors = append(modelErrors, "uncertain long-running action was replayed or not settled")
				}
			}
			result, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": map[string]any{
				"description": "A completed synthetic side effect was preserved across infrastructure interruption.", "scope": "synthetic interruption", "observed_at": time.Now().UTC().Format(time.RFC3339),
				"evidence": []map[string]string{{"path": runDir + "/side-effects.txt"}},
			}}})
			respond(agent.Block{Type: "text", Text: string(result)}, "end_turn")
		default:
			modelErrors = append(modelErrors, fmt.Sprintf("unexpected model request %d", requests))
			http.Error(w, "extra request", 400)
		}
	}))
	defer model.Close()
	c := config.Container{Socket: "/var/run/docker.sock", Image: image, Network: testContainerNetwork(t), Namespace: fmt.Sprintf("pwnmesh-interrupt-%d", time.Now().UnixNano()), CompletedAction: "stop"}
	runner := docker.New(c)
	runner.SetGraphHandler(func(_ context.Context, j worker.Job, r worker.GraphRequest) (any, error) {
		if r.Op != "read_updates" {
			return nil, fmt.Errorf("unexpected interruption fixture graph request %s", r.Op)
		}
		cursor := board.ExecuteUpdateCursor{ProjectID: projectID, StepID: j.Intent.ID, RunID: runID}
		if r.Updates != nil {
			cursor = *r.Updates
		}
		return board.ExecuteUpdates{Version: 1, ExecuteUpdateCursor: cursor, FromRevision: cursor.Revision, ToRevision: cursor.Revision, StateVersion: strings.Repeat("a", 64), Complete: true}, nil
	})
	defer runner.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := runner.Cleanup(ctx, projectID, "deleted"); err != nil {
			t.Error(err)
		}
	}()
	intent := board.Intent{ID: "i001", From: []string{"origin"}, Description: "Preserve a completed synthetic side effect across an execution interruption."}
	job := worker.Job{RunID: runID, Kind: "explore", WorkerType: "go", GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", Budget: config.Task{Timeout: 120, ConcludeTimeout: 20}, Intent: &intent, Graph: board.Graph{Project: board.Project{ID: projectID, OrchestrationVersion: 1, Title: "Synthetic interruption", Status: "active"}, Facts: []board.Fact{{ID: "origin", Description: "Local synthetic test only"}, {ID: "goal", Description: "Resume without repeating completed writes"}}, Intents: []board.Intent{intent}}}
	backend := config.Worker{Name: "controlled", Type: "go", Env: map[string]string{"ANTHROPIC_BASE_URL": model.URL, "ANTHROPIC_AUTH_TOKEN": "controlled-test-only", "ANTHROPIC_MODEL": "controlled", "PWNMESH_REQUEST_TIMEOUT": "15"}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runCtx, interrupt := context.WithCancelCause(ctx)
	defer interrupt(worker.ErrInterrupted)
	done := make(chan error, 1)
	go func() { _, err := runner.Run(runCtx, backend, job); done <- err }()
	container := c.Namespace + "-dispatch-" + projectID
	type checkpoint struct {
		RecoveryCount     int             `json:"recovery_count"`
		RepairCount       int             `json:"repair_count"`
		StartedAt         time.Time       `json:"started_at"`
		ExecutionDeadline time.Time       `json:"execution_deadline"`
		History           []agent.Message `json:"history"`
	}
	readSession := func() (checkpoint, error) {
		raw, err := dockerExec(ctx, container, []string{"bash", "-c", `test ! -e "$1/cancelled" && cat "$1/session.json"`, "check", runDir})
		var saved checkpoint
		if err == nil {
			err = json.Unmarshal([]byte(raw), &saved)
		}
		return saved, err
	}
	end := time.Now().Add(25 * time.Second)
	for {
		processes, err := inspectExecutionProcesses(ctx, container, runDir)
		if err == nil && len(processes) == 2 && !processes[0].dead() && !processes[1].dead() {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("execution ended before interrupted tool: %v", err)
		default:
		}
		if time.Now().After(end) {
			t.Fatalf("long-running tool never started: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	before, err := readSession()
	if err != nil {
		t.Fatal(err)
	}
	if len(before.History) != 4 {
		t.Fatalf("tool intent was not durably checkpointed: %+v", before)
	}
	launchToken, err := dockerExec(ctx, container, []string{"cat", runDir + "/launch-token"})
	if err != nil || len(launchToken) != 32 {
		t.Fatalf("missing per-launch identity: %q %v", launchToken, err)
	}
	if hardCrash {
		killOwnedWorker(t, ctx, container, runDir, launchToken)
	} else {
		interrupt(worker.ErrInterrupted)
	}
	select {
	case err := <-done:
		if hardCrash {
			if err == nil || !strings.Contains(err.Error(), "worker process exited with code 137") || runCtx.Err() != nil {
				t.Fatalf("SIGKILL did not reach the crash recovery path: %v (context: %v)", err, runCtx.Err())
			}
		} else if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected interruption outcome: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("container worker did not stop")
	}
	processes, err := inspectExecutionProcesses(ctx, container, runDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, process := range processes {
		if !process.dead() {
			t.Fatalf("owned process survived interruption: %+v", process)
		}
	}
	if _, err := readSession(); err != nil {
		t.Fatal("interruption persisted cancellation or lost checkpoint", err)
	}
	// Recreate a Docker exec that starts only after its original launch was
	// interrupted. It must fail before touching history or making a model call.
	if _, err := dockerExec(ctx, container, []string{"env", "PWNMESH_MODEL_BRIDGE=dispatcher-v1", "PWNMESH_LAUNCH_TOKEN=" + launchToken, "/usr/local/bin/pwnmesh", "worker", "--job", runDir + "/job.json"}); err == nil || !strings.Contains(err.Error(), "interrupted or superseded") {
		t.Fatalf("late interrupted launch was not rejected: %v", err)
	}
	result, err := runner.Run(ctx, backend, job)
	if err != nil || result.Status != "success" || result.Conclude {
		t.Fatal(result, err)
	}
	newToken, err := dockerExec(ctx, container, []string{"cat", runDir + "/launch-token"})
	if err != nil || len(newToken) != 32 || newToken == launchToken {
		t.Fatalf("recovery did not mint a new launch identity: %q %v", newToken, err)
	}
	after, err := readSession()
	if err != nil {
		t.Fatal(err)
	}
	if after.RecoveryCount != 1 || after.RepairCount != 0 || !after.StartedAt.Equal(before.StartedAt) || !after.ExecutionDeadline.Equal(before.ExecutionDeadline) {
		t.Fatalf("resume reset allowance or original deadline: before=%+v after=%+v", before, after)
	}
	count, err := dockerExec(ctx, container, []string{"cat", runDir + "/side-effects.txt"})
	if err != nil || strings.TrimSpace(count) != "completed-once" {
		t.Fatalf("completed side effect replayed: %q %v", count, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 3 || len(modelErrors) > 0 {
		t.Fatalf("model history mismatch: requests=%d errors=%v", requests, modelErrors)
	}
}

// Resolve only the worker registered in this test's unique container/run. The
// PID's start time, exact argv and launch token must all agree before sending
// SIGKILL; neither the container nor a process group is used as the target.
func killOwnedWorker(t *testing.T, ctx context.Context, container, runDir, launchToken string) {
	t.Helper()
	raw, err := dockerExec(ctx, container, []string{"cat", runDir + "/worker.pid"})
	if err != nil {
		t.Fatal("read test worker identity", err)
	}
	var identity struct {
		PID   int    `json:"pid"`
		Start string `json:"start"`
	}
	if err := json.Unmarshal([]byte(raw), &identity); err != nil || identity.PID <= 1 {
		t.Fatalf("invalid test worker identity: %q %v", raw, err)
	}
	if _, err := strconv.ParseUint(identity.Start, 10, 64); err != nil {
		t.Fatal("invalid test worker start time", err)
	}
	const script = `set -euo pipefail
pid=$1
expected_start=$2
run_dir=$3
expected_token=$4
case "$pid" in ''|*[!0-9]*) exit 2;; esac
test "$pid" -gt 1
IFS= read -r stat < "/proc/$pid/stat"
stat=${stat##*) }
read -r -a fields <<< "$stat"
test "${fields[19]}" = "$expected_start"
mapfile -d '' -t args < "/proc/$pid/cmdline"
test "${#args[@]}" -eq 4
test "${args[0]}" = /usr/local/bin/pwnmesh
test "${args[1]}" = worker
test "${args[2]}" = --job
test "${args[3]}" = "$run_dir/job.json"
token_found=false
while IFS= read -r -d '' variable; do
  if test "$variable" = "PWNMESH_LAUNCH_TOKEN=$expected_token"; then token_found=true; fi
done < "/proc/$pid/environ"
test "$token_found" = true
IFS= read -r stat < "/proc/$pid/stat"
stat=${stat##*) }
read -r -a fields <<< "$stat"
test "${fields[19]}" = "$expected_start"
kill -KILL -- "$pid"`
	if _, err := dockerExec(ctx, container, []string{"bash", "-c", script, "pwnmesh-owned-worker-crash", strconv.Itoa(identity.PID), identity.Start, runDir, launchToken}); err != nil {
		t.Fatal("refused or failed to kill the registered test worker", err)
	}
}
