package docker

import (
	"encoding/json"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/protocol"
)

func TestResultSinkKeepsOnlyFramedRPCAndFinalResult(t *testing.T) {
	request := protocol.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_graph"}
	rpc, err := json.Marshal(protocol.GraphRequestEvent{Type: "graph_request", Request: request})
	if err != nil {
		t.Fatal(err)
	}
	stream := `{"type":"message_delta","text":"model stream"}` + "\n" +
		`{"type":"tool_end","error":"a handled tool failure"}` + "\n" +
		string(rpc) + "\n" + `{"type":"result","status":"success","text":"final"}` + "\n"
	for _, size := range []int{1, 7, len(stream)} {
		calls := 0
		sink := &resultSink{graph: func(got protocol.GraphRequest) error {
			calls++
			if got.RequestID != request.RequestID || got.Op != request.Op {
				t.Fatalf("graph request changed: %+v", got)
			}
			return nil
		}}
		for offset := 0; offset < len(stream); offset += size {
			chunk := []byte(stream[offset:min(offset+size, len(stream))])
			if n, err := sink.Write(chunk); err != nil || n != len(chunk) {
				t.Fatalf("fragment of size %d: written=%d error=%v", size, n, err)
			}
		}
		if calls != 1 || !sink.found || sink.result.Status != "success" || sink.result.Text != "final" || sink.result.Error != "" || len(sink.pending) != 0 {
			t.Fatalf("stream events contaminated result: calls=%d sink=%+v", calls, sink)
		}
		if _, err := sink.Write([]byte("{\"type\":\"result\"}\n")); err == nil {
			t.Fatal("duplicate result accepted")
		}
	}
}

func TestResultSinkRetainsCumulativeUsageBeforeFinalResult(t *testing.T) {
	sink := &resultSink{runID: "run"}
	for offset, tokens := range []int{37, 83} {
		frame := protocol.UsageSnapshot{Type: "usage_snapshot", RunID: "run", JournalOffset: int64(offset + 1), Metrics: &protocol.DecisionMetrics{
			Version: 1, UsageScope: "run", ModelCalls: offset + 1, CompletedCalls: offset + 1, UsageCalls: offset + 1,
			Usage: agent.Usage{CacheWriteTokens: tokens}, UsageStatus: "partial", CostStatus: "unknown",
		}}
		raw, _ := json.Marshal(frame)
		if _, err := sink.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
		if sink.found || sink.result.Metrics == nil || sink.result.Metrics.Usage.CacheWriteTokens != tokens {
			t.Fatalf("usage snapshot was lost, added or treated as a result: %+v", sink)
		}
	}
	if _, err := sink.Write([]byte(`{"type":"result","status":"success","metrics":{"version":1,"usage_scope":"run_and_children","usage":{"cache_creation_input_tokens":101}}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if !sink.found || sink.result.Metrics.Usage.CacheWriteTokens != 101 {
		t.Fatal("final metrics did not supersede the partial snapshot")
	}
}

func TestResultSinkFinalWithoutMetricsRetainsObservedUsage(t *testing.T) {
	sink := &resultSink{runID: "run"}
	stream := `{"type":"usage_snapshot","run_id":"run","journal_offset":1,"metrics":{"version":1,"usage_scope":"run","model_calls":1,"completed_calls":1,"usage_calls":1,"usage":{"cache_creation_input_tokens":37}}}` + "\n" +
		`{"type":"result","status":"failed","error":"response delivery failed"}` + "\n"
	if _, err := sink.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	if !sink.found || sink.result.Status != "failed" || sink.result.Metrics == nil || sink.result.Metrics.Usage.CacheWriteTokens != 37 {
		t.Fatalf("final result without metrics erased reported usage: %+v", sink.result)
	}
}

func TestResultSinkRejectsInvalidUsageSnapshots(t *testing.T) {
	const valid = `{"type":"usage_snapshot","run_id":"run","journal_offset":2,"metrics":{"version":1,"usage_scope":"run","model_calls":1,"completed_calls":1,"usage_calls":1,"usage":{"cache_creation_input_tokens":37}}}`
	for _, frame := range []string{
		strings.Replace(valid, `"run_id":"run"`, `"run_id":"other"`, 1),
		strings.Replace(valid, `"journal_offset":2`, `"journal_offset":1`, 1),
		strings.Replace(valid, `"version":1`, `"version":2`, 1),
		strings.Replace(valid, `"usage_scope":"run"`, `"usage_scope":"run_and_children"`, 1),
		strings.Replace(valid, `"cache_creation_input_tokens":37`, `"cache_creation_input_tokens":-1`, 1),
		strings.Replace(valid, `"model_calls":1`, `"model_calls":0`, 1),
		strings.Replace(valid, `"model_calls":1`, `"model_calls":1,"summary_calls":-1`, 1),
		strings.Replace(valid, `"model_calls":1`, `"model_calls":1,"summary_calls":2`, 1),
		strings.Replace(valid, `"completed_calls":1`, `"completed_calls":1,"failed_calls":-1`, 1),
		strings.Replace(valid, `"completed_calls":1`, `"completed_calls":1,"failed_calls":2`, 1),
		strings.Replace(valid, `"journal_offset":2`, `"journal_offset":"bad"`, 1),
		valid[:len(valid)-1] + `,"padding":"` + strings.Repeat("x", 16<<10) + `"}`,
	} {
		sink := &resultSink{runID: "run", usageOffset: 1}
		if _, err := sink.Write([]byte(frame + "\n")); err == nil || sink.result.Metrics != nil || sink.found {
			t.Fatalf("invalid usage snapshot accepted: %v", err)
		}
	}
}

func TestResultSinkRejectsOversizedRequestAndEvent(t *testing.T) {
	for _, input := range []string{
		`{"type":"graph_request","padding":"` + strings.Repeat("x", protocol.MaxGraphRPCBytes) + `"}` + "\n",
		strings.Repeat("x", 32<<20) + "x",
	} {
		sink := &resultSink{graph: func(protocol.GraphRequest) error {
			t.Fatal("oversized graph request reached the bridge")
			return nil
		}}
		if _, err := sink.Write([]byte(input)); err == nil {
			t.Fatal("oversized frame accepted")
		}
	}
}

func BenchmarkResultSinkModelStream(b *testing.B) {
	line := []byte(`{"type":"message_delta","text":"Streaming model tokens which remain private in the worker journal, not the dispatch result."}` + "\n")
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	sink := &resultSink{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sink.Write(line); err != nil {
			b.Fatal(err)
		}
	}
}
