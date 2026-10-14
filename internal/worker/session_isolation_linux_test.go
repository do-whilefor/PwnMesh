//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/process"
)

func TestSessionReleasePreservesCancellationMeaning(t *testing.T) {
	t.Setenv("PWNMESH_LAUNCH_TOKEN", "")
	for _, tc := range []struct {
		name   string
		cause  error
		marked bool
	}{
		{name: "completed"},
		{name: "interrupted", cause: ErrInterrupted},
		{name: "cancelled", cause: context.Canceled, marked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			parent, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			ctx, release, err := acquireSession(parent, dir)
			if err != nil {
				t.Fatal(err)
			}
			if tc.cause != nil {
				cancel(tc.cause)
			}
			release()
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("private context survived release: %v", ctx.Err())
			}
			if process.Cancelled(dir) != tc.marked {
				t.Fatalf("hard-stop marker changed cancellation meaning for %s", tc.name)
			}
			if _, err := os.Stat(filepath.Join(dir, "worker.pid")); !os.IsNotExist(err) {
				t.Fatalf("worker marker survived release: %v", err)
			}
			unlock, err := process.Lock(dir)
			if err != nil {
				t.Fatalf("session retained its run lock: %v", err)
			}
			unlock()
		})
	}
}

func TestSessionRestoreFailureReleasesOwnership(t *testing.T) {
	t.Setenv("PWNMESH_LAUNCH_TOKEN", "")
	j, dir := outcomeJob(t, "explore"), t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runSession(context.Background(), j, Options{RunDir: dir}); err == nil || !strings.Contains(err.Error(), "invalid saved session") {
		t.Fatalf("expected malformed saved session: %v", err)
	}
	if process.Cancelled(dir) {
		t.Fatal("restore failure became a hard cancellation")
	}
	if _, err := os.Stat(filepath.Join(dir, "worker.pid")); !os.IsNotExist(err) {
		t.Fatalf("worker marker survived restore failure: %v", err)
	}
	unlock, err := process.Lock(dir)
	if err != nil {
		t.Fatalf("restore failure retained its run lock: %v", err)
	}
	unlock()
}

func TestRuntimeToolClosuresOwnTheirRunWhenCallerReusesSpareCapacity(t *testing.T) {
	base := make([]agent.Tool, 0, 16)
	var options []Options
	for _, marker := range []string{"only-agent-a", "only-agent-b"} {
		job := outcomeJob(t, "explore")
		job.RunID = marker
		job.Graph.Facts[0].Description = marker
		o := Options{Tools: base, RunDir: t.TempDir()}
		o.Output = sessionTestBridge(job, o.RunDir)
		if err := ConfigureRuntimeTools(job, &o); err != nil {
			t.Fatal(err)
		}
		options = append(options, o)
	}
	for i, marker := range []string{"only-agent-a", "only-agent-b"} {
		var read *agent.Tool
		for n := range options[i].Tools {
			if options[i].Tools[n].Name == "read_graph" {
				read = &options[i].Tools[n]
			}
		}
		if read == nil {
			t.Fatal("missing graph reader")
		}
		page, err := read.Execute(context.Background(), json.RawMessage(`{"section":"facts","ids":["origin"],"limit":1}`))
		if err != nil || !strings.Contains(page, marker) {
			t.Fatalf("runtime closure was replaced by another run: expected=%s page=%s err=%v", marker, page, err)
		}
	}
}

func TestConcurrentWorkersKeepPromptsToolsAndRecoveryIdentitySeparate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base := make([]agent.Tool, 0, 16)
	jobs := []Job{outcomeJob(t, "explore"), outcomeJob(t, "explore")}
	jobs[1].Workspace = jobs[0].Workspace
	dirs := []string{t.TempDir(), t.TempDir()}
	var ready atomic.Int32
	started := make(chan struct{})
	var wg sync.WaitGroup
	for i := range jobs {
		marker := fmt.Sprintf("private-agent-%d", i)
		jobs[i].RunID = marker
		jobs[i].Intent.Description = marker
		jobs[i].Graph.Facts[0].Description = marker
		wg.Add(1)
		go func() {
			defer wg.Done()
			turns := 0
			provider := scenarioProvider(func(ctx context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				turns++
				transcript, _ := json.Marshal(history)
				other := fmt.Sprintf("private-agent-%d", 1-i)
				if !strings.Contains(string(transcript), marker) || strings.Contains(string(transcript), other) {
					return agent.Message{}, errors.New("model received another Worker's private context")
				}
				if turns == 1 {
					if ready.Add(1) == 2 {
						close(started)
					}
					select {
					case <-started:
					case <-ctx.Done():
						return agent.Message{}, ctx.Err()
					}
					return agent.Message{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "read", Name: "read_graph", Input: json.RawMessage(`{"section":"facts","ids":["origin"],"limit":1}`)}}}, nil
				}
				if turns != 2 || !strings.Contains(string(history[len(history)-1].Content[0].Content), marker) {
					return agent.Message{}, errors.New("runtime tool read another Worker's input")
				}
				return agent.Text("assistant", completedOutput), nil
			})
			result, err := runTestWorker(ctx, jobs[i], Options{RunDir: dirs[i], Tools: base, Provider: provider})
			if err != nil || result.Status != "success" || turns != 2 {
				t.Errorf("Worker %d failed: %+v %v turns=%d", i, result, err, turns)
			}
		}()
	}
	wg.Wait()
	for i := range jobs {
		state := outcomeSession(t, dirs[i])
		raw, _ := json.Marshal(state.History)
		if state.Identity.RunID != jobs[i].RunID || !strings.Contains(string(raw), jobs[i].RunID) || strings.Contains(string(raw), jobs[1-i].RunID) {
			t.Fatalf("retained session includes foreign history: %+v", state)
		}
	}
	provider := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		t.Error("cross-run recovery reached the provider")
		return agent.Message{}, errors.New("wrong run")
	})
	if result, err := runTestWorker(ctx, jobs[0], Options{RunDir: dirs[1], Tools: base, Provider: provider}); err != nil || result.Status != "failed" || result.FailureKind != "graph_checkpoint" || !strings.Contains(result.Error, "identity") {
		t.Fatalf("cross-run recovery was not rejected: %+v %v", result, err)
	}
}
