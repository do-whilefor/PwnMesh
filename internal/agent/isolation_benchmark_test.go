package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// Include mutable RawMessage tool results, not just immutable text strings.
// The largest case approaches the Worker's default 8 MiB context byte limit.
func benchmarkTranscript(bytes int) []Message {
	messages := []Message{Text("user", "Review the complete collected evidence")}
	result, _ := json.Marshal(strings.Repeat("evidence observation ", 390))
	for i := 0; i < bytes/len(result); i++ {
		id := fmt.Sprint(i)
		messages = append(messages,
			Message{Role: "assistant", Content: []Block{{Type: "tool_use", ID: id, Name: "read", Input: json.RawMessage(`{"path":"/workspace/evidence"}`)}}, Usage: &Usage{InputTokens: i * 2000}},
			Message{Role: "user", Content: []Block{{Type: "tool_result", ToolUseID: id, Content: append(json.RawMessage(nil), result...)}}},
		)
	}
	return messages
}

func BenchmarkTranscriptObservedTurn(b *testing.B) {
	for _, size := range []int{256 << 10, 2 << 20, 8 << 20} {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			loop := &Loop{History: benchmarkTranscript(size), Checkpoint: &ContextCheckpoint{Version: 1}, ObserveRequests: true,
				Provider: observedProvider{generate: func(context.Context, []Message, []Definition, Emit) (Message, error) {
					return Text("assistant", "done"), nil
				}},
				SaveState: func(history []Message, checkpoint *ContextCheckpoint) error {
					runtime.KeepAlive(history)
					runtime.KeepAlive(checkpoint)
					return nil
				},
			}
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = loop.generate(ctx, loop.History, nil, 0)
				// One request and two durable message boundaries per tool turn.
				_ = loop.saveState()
				_ = loop.saveState()
			}
		})
	}
}
