package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type stopResultProvider struct {
	calls   int
	message Message
}

func TestStopResultDeclaredReadOnlyBatchRemainsParallel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started, release := make(chan string, 2), make(chan struct{})
	defer close(release)
	loop := &Loop{StopResult: func() (string, bool) { return "", false }, StopResultTools: []string{"finish"}}
	for _, name := range []string{"read-a", "read-b"} {
		loop.Tools = append(loop.Tools, Tool{Definition: Definition{Name: name, Schema: json.RawMessage(`{"type":"object"}`)}, Parallel: true,
			Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
				started <- name
				select {
				case <-release:
					return name, nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}})
	}
	results := make(chan []Block, 1)
	go func() {
		results <- loop.execute(ctx, []Block{{ID: "a", Name: "read-a", Input: json.RawMessage(`{}`)}, {ID: "b", Name: "read-b", Input: json.RawMessage(`{}`)}}, false)
	}()
	seen := map[string]bool{}
	for range 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-ctx.Done():
			t.Fatal("independent read-only tools did not start together before release")
		}
	}
	if len(seen) != 2 {
		t.Fatal("did not execute distinct reads")
	}
	// Release through cancellation so the test can also assert all outstanding
	// IDs settle without requiring another model call or leaking goroutines.
	cancel()
	out := <-results
	if len(out) != 2 || out[0].ToolUseID != "a" || out[1].ToolUseID != "b" || !out[0].IsError || !out[1].IsError {
		t.Fatalf("parallel cancellation left unsettled results: %+v", out)
	}
}

func TestStopResultDeclaredTerminalBatchStillSerialAndStops(t *testing.T) {
	committed := false
	effects := []string{}
	provider := &stopResultProvider{message: Message{Role: "assistant", Content: []Block{
		{Type: "tool_use", ID: "before", Name: "read", Input: json.RawMessage(`{}`)},
		{Type: "tool_use", ID: "finish", Name: "finish", Input: json.RawMessage(`{}`)},
		{Type: "tool_use", ID: "after", Name: "read", Input: json.RawMessage(`{}`)},
	}}}
	loop := &Loop{Provider: provider, StopResult: func() (string, bool) { return "finished", committed }, StopResultTools: []string{"finish"}}
	for _, name := range []string{"read", "finish"} {
		// Even an incorrectly Parallel-marked terminal tool remains a barrier
		// based on its explicit terminal declaration.
		loop.Tools = append(loop.Tools, Tool{Definition: Definition{Name: name, Schema: json.RawMessage(`{"type":"object"}`)}, Parallel: true,
			Execute: func(context.Context, json.RawMessage) (string, error) {
				effects = append(effects, name)
				committed = committed || name == "finish"
				return name, nil
			}})
	}
	result, err := loop.Run(context.Background(), "Read then finish")
	if err != nil || result != "finished" || provider.calls != 1 || strings.Join(effects, ",") != "read,finish" {
		t.Fatalf("terminal declaration lost serial/stop behavior: %q %v effects=%v calls=%d", result, err, effects, provider.calls)
	}
	last := loop.History[len(loop.History)-1].Content
	if len(last) != 3 || !last[2].IsError || !strings.Contains(string(last[2].Content), "not executed") {
		t.Fatalf("post-terminal call was not settled as skipped: %+v", last)
	}
}

func (p *stopResultProvider) Generate(context.Context, []Message, []Definition, Emit) (Message, error) {
	p.calls++
	if p.calls != 1 {
		return Message{}, errors.New("model called after the authoritative commit")
	}
	return p.message, nil
}

func TestStopResultSettlesToolGroupWithoutMoreSideEffectsOrModelCalls(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "normal"
		if cancelled {
			name = "lease_cancelled_after_commit"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			const authoritative = `{"accepted":true,"data":{"decided":true}}`
			committed := false
			executed := []string{}
			provider := &stopResultProvider{message: Message{Role: "assistant", StopReason: "tool_use", Content: []Block{
				{Type: "tool_use", ID: "before", Name: "read", Input: json.RawMessage(`{}`)},
				{Type: "tool_use", ID: "commit", Name: "commit", Input: json.RawMessage(`{}`)},
				{Type: "tool_use", ID: "after", Name: "write", Input: json.RawMessage(`{}`)},
			}}}
			makeTool := func(name string, effect func()) Tool {
				return Tool{Definition: Definition{Name: name, Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}, Parallel: true, Execute: func(context.Context, json.RawMessage) (string, error) {
					executed = append(executed, name)
					if effect != nil {
						effect()
					}
					return name + " finished", nil
				}}
			}
			followUp := make(chan string, 1)
			followUp <- "Continue with more work"
			var saved []Message
			var events []Event
			loop := &Loop{
				Provider: provider,
				Tools: []Tool{makeTool("read", nil), makeTool("commit", func() {
					committed = true
					if cancelled {
						cancel()
					}
				}), makeTool("write", nil)},
				FollowUp:   followUp,
				StopResult: func() (string, bool) { return authoritative, committed },
				SaveState: func(history []Message, _ *ContextCheckpoint) error {
					saved = append([]Message{}, history...)
					return nil
				},
				Emit: func(event Event) { events = append(events, event) },
				OnTurnEnd: func(context.Context, *Loop, Message) (context.Context, string, error) {
					return nil, "", errors.New("result repair/continuation ran after the authoritative commit")
				},
			}
			result, err := loop.Run(ctx, "Create and commit one bounded plan")
			if err != nil || result != authoritative || provider.calls != 1 || strings.Join(executed, ",") != "read,commit" {
				t.Fatalf("runtime failed to stop at commit: result=%q err=%v calls=%d effects=%v", result, err, provider.calls, executed)
			}
			if len(followUp) != 1 {
				t.Fatal("commit consumed queued follow-up work")
			}
			if len(saved) == 0 || saved[len(saved)-1].Role != "user" {
				t.Fatal("tool results were not durably settled before returning")
			}
			results := saved[len(saved)-1].Content
			if len(results) != 3 {
				t.Fatalf("pending tool group was left unsettled: %+v", results)
			}
			for i, id := range []string{"before", "commit", "after"} {
				if results[i].Type != "tool_result" || results[i].ToolUseID != id || results[i].IsError != (id == "after") {
					t.Fatalf("wrong result identity or execution status: %+v", results[i])
				}
			}
			if !strings.Contains(string(results[2].Content), "not executed") {
				t.Fatal("skipped side effect was not explicitly marked as unexecuted")
			}
			if err := (&Loop{History: saved}).RepairHistory(); err != nil {
				t.Fatalf("stopping after commit left an unrecoverable transcript: %v", err)
			}
			foundResult := false
			for _, event := range events {
				if event.Type == "runtime_result" && event.Text == authoritative {
					foundResult = true
				}
			}
			if !foundResult {
				t.Fatal("authoritative result was not recorded in the event stream")
			}
		})
	}
}
