//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"pwnmesh/internal/agent"
)

type validationProvider func([]agent.Message) (agent.Message, error)

func (p validationProvider) Generate(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
	return p(history)
}

func TestSearchNoMatchesPreservesOtherFailures(t *testing.T) {
	for _, tool := range []string{"grep", "find"} {
		for _, tc := range []struct {
			name       string
			code       int
			cleanup    bool
			diagnostic bool
		}{
			{name: "no matches", code: 1},
			{name: "cleanup failure", code: 1, cleanup: true},
			{name: "diagnostic", code: 1, diagnostic: true},
			{name: "search failure", code: 2},
		} {
			t.Run(tool+"/"+tc.name, func(t *testing.T) {
				s := fidelitySet(t)
				bin := t.TempDir()
				// A silent no-match status must not hide failure to clean its
				// process marker. Mutate only this disposable command's marker.
				const script = `#!/bin/sh
if [ "$PWNMESH_TEST_SEARCH_CLEANUP_FAILURE" = 1 ]; then
  marker="$PWNMESH_TEST_SEARCH_RUN_DIR/group-$PWNMESH_PROCESS_TOKEN"
  rm -- "$marker" || exit 2
  mkdir -- "$marker" || exit 2
  : > "$marker/preserve"
fi
if [ "$PWNMESH_TEST_SEARCH_DIAGNOSTIC" = 1 ]; then
  printf 'search diagnostic\n' >&2
fi
exit "$PWNMESH_TEST_SEARCH_EXIT"
`
				if err := os.WriteFile(filepath.Join(bin, "rg"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
				s.Env = []string{"PWNMESH_TEST_SEARCH_RUN_DIR=" + s.RunDir, "PWNMESH_TEST_SEARCH_EXIT=" + strconv.Itoa(tc.code)}
				if tc.cleanup {
					s.Env = append(s.Env, "PWNMESH_TEST_SEARCH_CLEANUP_FAILURE=1")
				}
				if tc.diagnostic {
					s.Env = append(s.Env, "PWNMESH_TEST_SEARCH_DIAGNOSTIC=1")
				}
				text, err := callFidelityTool(t, s, context.Background(), tool, map[string]any{"pattern": "missing"})
				if tc.name == "no matches" {
					if err != nil || text != "No matches" {
						t.Fatalf("normal no-match result changed: %q, %v", text, err)
					}
					return
				}
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != tc.code || text == "No matches" {
					t.Fatalf("search failure was hidden: %q, %v", text, err)
				}
				if tc.cleanup && !errors.Is(err, syscall.ENOTEMPTY) {
					t.Fatalf("cleanup failure was lost: %v", err)
				}
				if tc.diagnostic && !strings.Contains(text, "search diagnostic") {
					t.Fatalf("search diagnostic was lost: %q", text)
				}
			})
		}
	}
}

func TestNoMatchExitRejectsJoinedCancellation(t *testing.T) {
	exit := exec.Command("sh", "-c", "exit 1").Run()
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, os.ErrPermission} {
		for _, combined := range []error{
			errors.Join(exit, cause),
			fmt.Errorf("search: %w", errors.Join(cause, errors.Join(exit))),
			errors.Join(fmt.Errorf("process: %w", exit), errors.Join(cause)),
		} {
			if onlyNoMatchExit(combined) {
				t.Fatalf("combined search failure was normalized: %v", combined)
			}
		}
	}
	if !onlyNoMatchExit(fmt.Errorf("process: %w", errors.Join(exit))) {
		t.Fatal("wrapped no-match status was not recognized")
	}
}

func TestLoopRejectsBuiltInToolSchemasBeforeSideEffects(t *testing.T) {
	s := fidelitySet(t)
	putFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
	invalid := []struct{ tool, input string }{
		{"read", `{"path":"target","offset":0}`},
		{"bash", `{"command":"printf changed > target","timeout":0}`},
		{"edit", `{"path":"target","oldText":"original","newText":"changed","extra":true}`},
		{"write", `{"path":"target","content":"changed","extra":true}`},
		{"grep", `{"pattern":"original","ignoreCase":"yes"}`},
		{"find", `{"pattern":"*","path":null}`},
		{"ls", `{"path":[]}`},
		{"write", `{}`},
	}
	calls := make([]agent.Block, len(invalid))
	for i, item := range invalid {
		calls[i] = agent.Block{Type: "tool_use", ID: fmt.Sprintf("invalid-%d", i), Name: item.tool, Input: json.RawMessage(item.input)}
	}
	executed, turns := 0, 0
	all := s.All()
	for i := range all {
		execute := all[i].Execute
		all[i].Execute = func(ctx context.Context, raw json.RawMessage) (string, error) {
			executed++
			return execute(ctx, raw)
		}
	}
	loop := agent.Loop{Tools: all, Provider: validationProvider(func(history []agent.Message) (agent.Message, error) {
		turns++
		if turns == 1 {
			return agent.Message{Role: "assistant", Content: calls}, nil
		}
		if turns != 2 {
			t.Fatal("invalid calls did not settle")
		}
		results := history[len(history)-1].Content
		if len(results) != len(calls) {
			t.Fatalf("got %d results for %d calls", len(results), len(calls))
		}
		for i, result := range results {
			if result.Type != "tool_result" || result.ToolUseID != calls[i].ID || !result.IsError {
				t.Fatalf("invalid %s call lost its paired rejection: %+v", calls[i].Name, result)
			}
		}
		return agent.Text("assistant", "done"), nil
	})}
	if _, err := loop.Run(context.Background(), "Check invalid calls."); err != nil || turns != 2 || executed != 0 {
		t.Fatalf("schema dispatch: turns=%d executions=%d err=%v", turns, executed, err)
	}
	assertFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
	if _, err := os.Stat(s.RunDir); !os.IsNotExist(err) {
		t.Fatalf("invalid calls created output artifacts: %v", err)
	}
}

func TestLoopRejectsMalformedBuiltInToolArguments(t *testing.T) {
	for _, input := range []string{`null`, `[]`, `"text"`, `{"path":`} {
		for _, corrected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/corrected=%v", input, corrected), func(t *testing.T) {
				s := fidelitySet(t)
				putFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
				calls := []agent.Block{
					{Type: "tool_use", ID: "valid-write", Name: "write", Input: json.RawMessage(`{"path":"target","content":"changed"}`)},
					{Type: "tool_use", ID: "invalid-write", Name: "write", Input: json.RawMessage(input)},
				}
				turns, executed := 0, 0
				all := s.All()
				for i := range all {
					execute := all[i].Execute
					all[i].Execute = func(ctx context.Context, raw json.RawMessage) (string, error) {
						executed++
						return execute(ctx, raw)
					}
				}
				loop := agent.Loop{Tools: all, Provider: validationProvider(func(history []agent.Message) (agent.Message, error) {
					turns++
					if turns > 2 {
						t.Fatal("malformed calls exceeded the correction allowance")
					}
					if turns == 2 {
						// The whole response is rejected before dispatch, including
						// the valid sibling. Invalid JSON must not enter history.
						if len(history) != 2 || history[1].Role != "user" || len(history[1].Content) != 1 || history[1].Content[0].Type != "text" || !strings.Contains(history[1].Text(), "none of its calls ran") {
							t.Fatalf("malformed response entered history or lost correction guidance: %+v", history)
						}
						if _, err := json.Marshal(history); err != nil {
							t.Fatalf("correction history is not serializable: %v", err)
						}
						if corrected {
							return agent.Text("assistant", "done"), nil
						}
					}
					return agent.Message{Role: "assistant", Content: calls}, nil
				})}
				_, err := loop.Run(context.Background(), "Check malformed calls.")
				var modelErr *agent.ModelError
				if corrected && err != nil || !corrected && (!errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorToolArguments) {
					t.Fatalf("unexpected correction outcome: %v", err)
				}
				if turns != 2 || executed != 0 || loop.Checkpoint.ToolArgumentRetries != 1 {
					t.Fatalf("malformed dispatch: turns=%d executions=%d checkpoint=%+v", turns, executed, loop.Checkpoint)
				}
				assertFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
				if _, err := os.Stat(s.RunDir); !os.IsNotExist(err) {
					t.Fatalf("malformed calls created output artifacts: %v", err)
				}
			})
		}
	}
}
