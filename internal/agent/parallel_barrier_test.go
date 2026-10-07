package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestParallelGroupsRetainSerialBarriers(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%v", terminal), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var before, after atomic.Int32
			var barrier atomic.Bool
			beforeReady, afterReady := make(chan struct{}), make(chan struct{})
			loop := &Loop{}
			if terminal {
				loop.StopResult = func() (string, bool) { return "done", barrier.Load() }
				loop.StopResultTools = []string{"barrier"}
			}
			loop.Tools = []Tool{
				{Definition: Definition{Name: "before", Schema: json.RawMessage(`{"type":"object"}`)}, Parallel: true, Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
					if barrier.Load() {
						return "", fmt.Errorf("read crossed the serial barrier")
					}
					if before.Add(1) == 2 {
						close(beforeReady)
					}
					select {
					case <-beforeReady:
						return "read before write", nil
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}},
				{Definition: Definition{Name: "barrier", Schema: json.RawMessage(`{"type":"object"}`)}, Parallel: terminal, Execute: func(context.Context, json.RawMessage) (string, error) {
					if before.Load() != 2 || after.Load() != 0 {
						return "", fmt.Errorf("barrier crossed unfinished reads or later effects")
					}
					barrier.Store(true)
					return "committed", nil
				}},
				{Definition: Definition{Name: "after", Schema: json.RawMessage(`{"type":"object"}`)}, Parallel: true, Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
					if !barrier.Load() {
						return "", fmt.Errorf("read started before write completed")
					}
					if after.Add(1) == 2 {
						close(afterReady)
					}
					select {
					case <-afterReady:
						return "read after write", nil
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}},
			}
			var calls []Block
			for i, name := range []string{"before", "before", "barrier", "after", "after"} {
				calls = append(calls, Block{ID: fmt.Sprint(i), Name: name, Input: json.RawMessage(`{}`)})
			}
			results := loop.execute(ctx, calls, false)
			for i, result := range results {
				if result.ToolUseID != calls[i].ID || result.IsError != (terminal && i > 2) {
					t.Fatalf("parallel group or barrier failed at %d: %+v", i, results)
				}
			}
			wantAfter := int32(2)
			if terminal {
				wantAfter = 0
			}
			if before.Load() != 2 || after.Load() != wantAfter || !barrier.Load() {
				t.Fatalf("incorrect side effects: before=%d after=%d barrier=%v", before.Load(), after.Load(), barrier.Load())
			}
		})
	}
}

// The fixed delay models independent tool latency without model/provider or
// Worker construction time. It measures scheduling, not real task throughput.
func BenchmarkMixedToolBatchParallelGroups(b *testing.B) {
	for _, parallel := range []bool{false, true} {
		b.Run(fmt.Sprintf("parallel=%v", parallel), func(b *testing.B) {
			loop := &Loop{Tools: []Tool{
				{Definition: Definition{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)}, Parallel: parallel, Execute: func(context.Context, json.RawMessage) (string, error) {
					time.Sleep(5 * time.Millisecond)
					return "observation", nil
				}},
				{Definition: Definition{Name: "write", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(context.Context, json.RawMessage) (string, error) { return "committed", nil }},
			}}
			var calls []Block
			for i, name := range []string{"read", "read", "read", "read", "write", "read", "read", "read", "read"} {
				calls = append(calls, Block{ID: fmt.Sprint(i), Name: name, Input: json.RawMessage(`{}`)})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				loop.execute(context.Background(), calls, false)
			}
		})
	}
}
