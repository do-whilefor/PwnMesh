package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParallelToolBatchBoundsActiveCallsAndSettlesQueuedCancellation(t *testing.T) {
	const limit, total = 16, 49
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%v", cancelled), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			started, release := make(chan struct{}, total), make(chan struct{}, total)
			defer close(release)
			var active, peak, invoked atomic.Int32
			starts, ends := map[string]int{}, map[string]int{}
			loop := &Loop{Emit: func(event Event) {
				switch event.Type {
				case "tool_start":
					starts[event.ToolID]++
				case "tool_end":
					ends[event.ToolID]++
				}
			}}
			loop.Tools = []Tool{{Definition: Definition{Name: "read", Schema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}`)}, Parallel: true, Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
				invoked.Add(1)
				current := active.Add(1)
				defer active.Add(-1)
				for previous := peak.Load(); current > previous && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
				}
				started <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return "", ctx.Err()
				}
				var input struct{ ID int }
				if err := json.Unmarshal(raw, &input); err != nil {
					return "", err
				}
				if input.ID == 17 {
					panic("fixture panic")
				}
				return fmt.Sprint(input.ID), nil
			}}}
			calls := make([]Block, total)
			for n := range calls {
				calls[n] = Block{ID: fmt.Sprint(n), Name: "read", Input: json.RawMessage(fmt.Sprintf(`{"id":%d}`, n))}
			}
			done := make(chan []Block, 1)
			go func() { done <- loop.execute(ctx, calls, false) }()
			for range limit {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("parallel tools did not fill the available slots")
				}
			}
			// Every active callback is blocked. An additional start therefore
			// proves unbounded fanout; this window measures no tool latency.
			select {
			case <-started:
				t.Fatal("more than 16 native tools started before a slot was released")
			case <-time.After(25 * time.Millisecond):
			}
			if cancelled {
				cancel()
			} else {
				for range total {
					release <- struct{}{}
				}
			}
			var results []Block
			select {
			case results = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("parallel batch did not settle every tool call")
			}
			wantInvoked := int32(total)
			if cancelled {
				wantInvoked = limit
			}
			if len(results) != total || peak.Load() != limit || active.Load() != 0 || invoked.Load() != wantInvoked {
				t.Fatalf("incorrect bounded execution: results=%d peak=%d active=%d invoked=%d", len(results), peak.Load(), active.Load(), invoked.Load())
			}
			for n, result := range results {
				id := fmt.Sprint(n)
				if result.ToolUseID != id || result.IsError != (cancelled || n == 17) || starts[id] != 1 || ends[id] != 1 {
					t.Fatalf("call %s lost its ordered receipt or events: %+v starts=%d ends=%d", id, result, starts[id], ends[id])
				}
				var content string
				if err := json.Unmarshal(result.Content, &content); err != nil || !result.IsError && content != id || !cancelled && n == 17 && !strings.Contains(content, "tool panicked") {
					t.Fatalf("call %s returned the wrong receipt: %s", id, result.Content)
				}
			}
		})
	}
}

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
