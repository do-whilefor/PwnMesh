//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/agent"
)

func TestFinishHandoffPreservesIndependentReadToolParallelism(t *testing.T) {
	j := graphWrapperJob(t)
	j.ResultContractVersion = 2
	dir := t.TempDir()
	path := filepath.Join(j.Workspace, "observation.txt")
	if err := os.WriteFile(path, []byte("observed result\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started, release := make(chan string, 3), make(chan struct{})
	var calls atomic.Int32
	var tools []agent.Tool
	for _, name := range []string{"read_a", "read_b"} {
		tools = append(tools, agent.Tool{Definition: agent.Definition{Name: name, Schema: json.RawMessage(`{"type":"object"}`)}, Parallel: true,
			Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
				calls.Add(1)
				started <- name
				select {
				case <-release:
					return "observed result", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}})
	}
	turns := 0
	provider := scenarioProvider(func(_ context.Context, _ []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turns++
		if turns == 1 {
			return agent.Message{Role: "assistant", Content: []agent.Block{
				{Type: "tool_use", ID: "a", Name: "read_a", Input: json.RawMessage(`{}`)},
				{Type: "tool_use", ID: "b", Name: "read_b", Input: json.RawMessage(`{}`)},
			}}, nil
		}
		if turns == 2 {
			return agent.Message{Role: "assistant", Content: []agent.Block{
				{Type: "tool_use", ID: "finish", Name: "finish_step", Input: finishStepInput(t, path)},
				{Type: "tool_use", ID: "after", Name: "read_a", Input: json.RawMessage(`{}`)},
			}}, nil
		}
		return agent.Message{}, errors.New("model called after explicit handoff")
	})
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := runTestWorker(ctx, j, Options{RunDir: dir, Provider: provider, Tools: tools})
		done <- outcome{result, err}
	}()
	seen := map[string]bool{}
	for range 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-ctx.Done():
			t.Fatal("Worker serialized independent read tools merely because finish_step was available")
		}
	}
	close(release)
	var result outcome
	select {
	case result = <-done:
	case <-ctx.Done():
		t.Fatal("Worker did not finish after its explicit handoff")
	}
	if len(seen) != 2 || result.err != nil || result.result.Status != "success" || turns != 2 || calls.Load() != 2 {
		t.Fatalf("read batch or terminal batch violated contract: seen=%v result=%+v turns=%d tool_calls=%d", seen, result, turns, calls.Load())
	}
	refs := finalEvidenceRefs(t, j, result.result)
	if len(refs) != 1 || refs[0].Excerpt != "observed result\n" {
		t.Fatalf("explicit finish lost frozen evidence after the parallel reads: %+v", refs)
	}
}
