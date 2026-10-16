package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

func TestDisabledToolsRejectCallsAndResumeSettledResults(t *testing.T) {
	for _, reason := range []string{"tool execution is disabled", "use retained context only"} {
		for _, parallel := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/parallel=%t", reason, parallel), func(t *testing.T) {
				var executions atomic.Int32
				tool := Tool{Definition: Definition{Name: "effect", Schema: json.RawMessage(`{"type":"object"}`)}, Parallel: parallel,
					Execute: func(context.Context, json.RawMessage) (string, error) {
						executions.Add(1)
						return "unexpected side effect", nil
					}}
				interrupted := errors.New("interrupted after persisted rejection")
				requests := 0
				provider := observedProvider{generate: func(_ context.Context, history []Message, defs []Definition, _ Emit) (Message, error) {
					requests++
					if len(defs) != 0 {
						t.Fatalf("disabled policy exposed tools: %+v", defs)
					}
					if requests == 1 {
						return Message{Role: "assistant", Content: []Block{
							{Type: "tool_use", ID: "first", Name: "effect", Input: json.RawMessage(`{}`)},
							{Type: "tool_use", ID: "second", Name: "effect", Input: json.RawMessage(`{}`)},
						}}, nil
					}
					results := history[len(history)-1].Content
					if len(history) != 3 || len(results) != 2 {
						t.Fatalf("rejected calls were lost or duplicated: %+v", history)
					}
					for n, id := range []string{"first", "second"} {
						if results[n].Type != "tool_result" || results[n].ToolUseID != id || !results[n].IsError {
							t.Fatalf("rejected tool result lost its identity: %+v", results[n])
						}
						var detail string
						if json.Unmarshal(results[n].Content, &detail) != nil || detail != reason {
							t.Fatalf("policy explanation lost: %+v", results[n])
						}
					}
					if requests == 2 {
						return Message{}, interrupted
					}
					return Text("assistant", "final answer"), nil
				}}
				var saved []byte
				loop := Loop{Provider: provider, Tools: []Tool{tool}, ToolsDisabledReason: reason,
					SaveState: func(history []Message, _ *ContextCheckpoint) (err error) {
						saved, err = json.Marshal(history)
						return err
					}}
				if _, err := loop.Run(context.Background(), "Produce the final answer"); !errors.Is(err, interrupted) {
					t.Fatalf("did not stop after settled rejection: %v", err)
				}
				resumed := Loop{Provider: provider, Tools: []Tool{tool}, ToolsDisabledReason: loop.ToolsDisabledReason}
				if err := json.Unmarshal(saved, &resumed.History); err != nil {
					t.Fatal(err)
				}
				result, err := resumed.Run(context.Background(), "")
				if err != nil || result != "final answer" || requests != 3 || executions.Load() != 0 {
					t.Fatalf("resume invoked a forbidden tool: result=%q err=%v requests=%d executions=%d", result, err, requests, executions.Load())
				}
			})
		}
	}
}
