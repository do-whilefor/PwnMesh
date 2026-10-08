package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRecentHistoryBudgetKeepsWholeToolGroups(t *testing.T) {
	messages := []Message{
		Text("user", "old context"),
		{Role: "assistant", Content: []Block{
			{Type: "tool_use", ID: "a", Name: "read", Input: json.RawMessage(`{"path":"<&>"}`)},
			{Type: "tool_use", ID: "b", Name: "read", Input: json.RawMessage(`{"path":"中文🙂"}`)},
		}},
		{Role: "user", Content: []Block{
			{Type: "tool_result", ToolUseID: "a", Content: json.RawMessage(`"one\n\\two"`)},
			{Type: "tool_result", ToolUseID: "b", Content: json.RawMessage(`{"answer":9007199254740993}`)},
		}},
		Text("assistant", "recent <&>\n中文🙂"),
	}
	for i := range messages {
		messages[i].Sequence = uint64(i + 1)
		messages[i].Usage = &Usage{InputTokens: 999999}
	}
	// Each boundary is checked against the exact complete wire suffix, not an
	// approximation based on source string lengths or retained local metadata.
	for _, start := range []int{0, 1, 3} {
		raw, err := json.Marshal(WireHistory(messages[start:]))
		if err != nil {
			t.Fatal(err)
		}
		cut, err := recentHistoryStart(messages, len(raw))
		if err != nil || cut != start {
			t.Fatalf("exact budget for suffix %d selected %d: %v", start, cut, err)
		}
		cut, err = recentHistoryStart(messages, len(raw)-1)
		if err != nil || cut <= start || cut == 2 {
			t.Fatalf("one byte less than suffix %d selected %d: %v", start, cut, err)
		}
	}
	if cut, err := recentHistoryStart(messages, 0); err != nil || cut != len(messages) {
		t.Fatalf("zero budget retained messages: %d %v", cut, err)
	}
	if cut, err := recentHistoryStart(nil, 100); err != nil || cut != 0 {
		t.Fatalf("empty history: %d %v", cut, err)
	}
	if _, err := recentHistoryStart(messages[2:3], 10000); err == nil {
		t.Fatal("orphaned tool result was accepted")
	}
	messages[2].Content[0].Content = json.RawMessage(`"broken`)
	if _, err := recentHistoryStart(messages, 10000); err == nil {
		t.Fatal("invalid tool result JSON was accepted")
	}
}

func TestRecentHistorySelectionMatchesExactWireSuffix(t *testing.T) {
	messages := []Message{
		Text("user", "old\x00 context\xff"),
		{Role: "assistant", Content: []Block{
			{Type: "thinking", Signature: "signed<&>"},
			{Type: "redacted_thinking", Data: "opaque"},
			{Type: "tool_use", ID: "read", Name: "read", Input: json.RawMessage(`{"unicode":"\u2028中文🙂"}`)},
		}},
		{Role: "user", Content: []Block{
			{Type: "tool_result", ToolUseID: "read", Content: json.RawMessage(`"quote\" and \\ slash"`)},
			{Type: "text", Text: "extra result context"},
		}},
		Text("assistant", "recent\u2029<&>"),
	}
	// Compare every byte allowance to complete encodings at legal boundaries.
	// This includes JSON escaping, invalid UTF-8 replacement, thinking blocks,
	// and a result message carrying additional text after its tool results.
	sizes := map[int]int{}
	for _, start := range []int{0, 1, 3} {
		raw, err := json.Marshal(WireHistory(messages[start:]))
		if err != nil {
			t.Fatal(err)
		}
		sizes[start] = len(raw)
	}
	for budget := -1; budget <= sizes[0]+1; budget++ {
		want := len(messages)
		for _, start := range []int{0, 1, 3} {
			if sizes[start] <= budget {
				want = start
				break
			}
		}
		got, err := recentHistoryStart(messages, budget)
		if err != nil || got != want {
			t.Fatalf("budget=%d selected=%d want=%d: %v", budget, got, want, err)
		}
	}
}

type measuringProvider struct {
	observedProvider
	measurements map[string]int
}

func requestMeasurement(messages []Message, defs []Definition) string {
	raw, err := json.Marshal(struct {
		Messages []Message    `json:"messages"`
		Tools    []Definition `json:"tools,omitempty"`
	}{WireHistory(messages), defs})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func (p *measuringProvider) InputBytes(messages []Message, defs []Definition) (int, error) {
	request := requestMeasurement(messages, defs)
	p.measurements[request]++
	return len(request), nil
}

func TestObservedRequestMeasuresOncePerPreparedTurn(t *testing.T) {
	for _, budget := range []int{0, 1 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			p := &measuringProvider{measurements: map[string]int{}}
			requests, observed := 0, 0
			loop := &Loop{Provider: p, ContextBytes: budget, ObserveRequests: true,
				Tools: []Tool{{Definition: Definition{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)},
					Execute: func(context.Context, json.RawMessage) (string, error) { return "evidence", nil }}},
				Emit: func(e Event) {
					if e.Type == "model_call_start" {
						observed = e.Request.InputBytes
					}
				},
			}
			loop.BeforeRequest = func(ctx context.Context, loop *Loop) (context.Context, error) {
				if requests == 1 {
					loop.Concluding = true // Tool definitions must be measured anew.
					return ctx, loop.AppendInstruction("use the evidence to conclude")
				}
				return nil, nil
			}
			p.generate = func(_ context.Context, messages []Message, defs []Definition, _ Emit) (Message, error) {
				request := requestMeasurement(messages, defs)
				if p.measurements[request] != 1 || observed != len(request) {
					t.Fatalf("measurement repeated or stale: count=%d observed=%d actual=%d", p.measurements[request], observed, len(request))
				}
				requests++
				if requests == 1 {
					return Message{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "call", Name: "read", Input: json.RawMessage(`{}`)}}}, nil
				}
				if len(defs) != 0 {
					t.Fatal("conclusion retained tools")
				}
				return Text("assistant", "done"), nil
			}
			if result, err := loop.Run(context.Background(), "inspect"); err != nil || result != "done" || requests != 2 || len(p.measurements) != 2 {
				t.Fatalf("run=%q err=%v requests=%d measurements=%d", result, err, requests, len(p.measurements))
			}
		})
	}
}

func TestRequestMeasurementOnlyWhenNeeded(t *testing.T) {
	for _, budget := range []int{0, 1 << 20} {
		for _, observe := range []bool{false, true} {
			t.Run(fmt.Sprintf("budget=%d/observe=%v", budget, observe), func(t *testing.T) {
				p := &measuringProvider{measurements: map[string]int{}}
				p.generate = func(context.Context, []Message, []Definition, Emit) (Message, error) {
					return Text("assistant", "done"), nil
				}
				loop := &Loop{Provider: p, ContextTokens: budget, ObserveRequests: observe}
				if _, err := loop.Run(context.Background(), "inspect"); err != nil {
					t.Fatal(err)
				}
				want := 0
				if observe || budget > 0 {
					want = 1
				}
				measurements := 0
				for _, count := range p.measurements {
					measurements += count
				}
				if measurements != want {
					t.Fatalf("measurements=%d want=%d", measurements, want)
				}
			})
		}
	}
}

func TestObservedRequestUsesPostCompactionSize(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(fmt.Sprintf("overflow=%v", overflow), func(t *testing.T) {
			p := &measuringProvider{measurements: map[string]int{}}
			loop := &Loop{Provider: p, TaskPrompt: "task", ContextBytes: 16000, SummaryBytes: 512, RecentBytes: 1024, ObserveRequests: true}
			loop.History = []Message{Text("user", "task")}
			for i := 0; i < 12; i++ {
				loop.History = append(loop.History, Text("assistant", strings.Repeat("observation ", 400)))
			}
			if overflow {
				loop.ContextBytes = 1 << 20
			}
			observed, requests, summaries := 0, 0, 0
			loop.Emit = func(e Event) {
				if e.Type == "model_call_start" && e.Request.Kind == "turn" {
					observed = e.Request.InputBytes
				}
			}
			p.summary = func(context.Context, []Message, int, Emit) (Message, error) {
				summaries++
				return Text("assistant", `{"notes":"continue from the observations","quotes":[]}`), nil
			}
			p.generate = func(_ context.Context, messages []Message, defs []Definition, _ Emit) (Message, error) {
				request := requestMeasurement(messages, defs)
				if observed != len(request) {
					t.Fatalf("stale request size after compaction: observed=%d actual=%d", observed, len(request))
				}
				requests++
				if overflow && requests == 1 {
					return Message{}, &ModelError{Kind: ErrorContextOverflow, Err: errors.New("context full")}
				}
				if p.measurements[request] != 1 || summaries != 1 || loop.Checkpoint.CompactionCount != 1 || len(messages) >= 13 {
					t.Fatalf("compacted request: measurements=%d summaries=%d", p.measurements[request], summaries)
				}
				return Text("assistant", "done"), nil
			}
			if result, err := loop.Run(context.Background(), ""); err != nil || result != "done" {
				t.Fatalf("run=%q err=%v", result, err)
			}
		})
	}
}

// Keep the old suffix scan only as a benchmark comparator for the identical
// wire-byte budget. The production path encodes each retained group once.
func benchmarkRepeatedSuffix(messages []Message, budget int) (int, error) {
	cut := len(messages)
	for cut > 0 {
		start := cut - 1
		if hasResults(messages[start]) {
			start--
		}
		raw, err := json.Marshal(WireHistory(messages[start:]))
		if err != nil {
			return 0, err
		}
		if len(raw) > budget {
			break
		}
		cut = start
	}
	return cut, nil
}

func BenchmarkRecentHistorySelection(b *testing.B) {
	for _, bytes := range []int{256 << 10, 2 << 20, 8 << 20} {
		messages := benchmarkTranscript(bytes)
		for _, algorithm := range []struct {
			name string
			find func([]Message, int) (int, error)
		}{{"repeated_suffix", benchmarkRepeatedSuffix}, {"single_pass", recentHistoryStart}} {
			b.Run(fmt.Sprintf("%dKiB/%s", bytes>>10, algorithm.name), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := algorithm.find(messages, bytes*3/4); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

type benchmarkSizingProvider struct{ observedProvider }

func (benchmarkSizingProvider) InputBytes(messages []Message, defs []Definition) (int, error) {
	raw, err := json.Marshal(struct {
		Messages []Message    `json:"messages"`
		Tools    []Definition `json:"tools,omitempty"`
	}{WireHistory(messages), defs})
	return len(raw), err
}

func BenchmarkPreparedObservedRequest(b *testing.B) {
	for _, bytes := range []int{256 << 10, 2 << 20, 8 << 20} {
		for _, reuse := range []bool{false, true} {
			b.Run(fmt.Sprintf("%dKiB/reuse=%v", bytes>>10, reuse), func(b *testing.B) {
				loop := &Loop{History: benchmarkTranscript(bytes), ObserveRequests: true, ContextBytes: bytes * 2,
					Provider: benchmarkSizingProvider{observedProvider{generate: func(context.Context, []Message, []Definition, Emit) (Message, error) {
						return Text("assistant", "done"), nil
					}}},
				}
				ctx := context.Background()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					size, err := loop.compactRequest(ctx, false)
					if err != nil {
						b.Fatal(err)
					}
					if !reuse {
						size = -1 // Before: observation repeated the same measurement.
					}
					if _, err := loop.generateMeasured(ctx, loop.History, nil, 0, size); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
