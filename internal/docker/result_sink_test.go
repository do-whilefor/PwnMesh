package docker

import (
	"encoding/json"
	"strings"
	"testing"

	"pwnmesh/internal/worker"
)

func TestResultSinkKeepsOnlyFramedRPCAndFinalResult(t *testing.T) {
	request := worker.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_graph"}
	rpc, err := json.Marshal(worker.GraphRequestEvent{Type: "graph_request", Request: request})
	if err != nil {
		t.Fatal(err)
	}
	stream := `{"type":"message_delta","text":"model stream"}` + "\n" +
		`{"type":"tool_end","error":"a handled tool failure"}` + "\n" +
		string(rpc) + "\n" + `{"type":"result","status":"success","text":"final"}` + "\n"
	for _, size := range []int{1, 7, len(stream)} {
		calls := 0
		sink := &resultSink{graph: func(got worker.GraphRequest) error {
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

func TestResultSinkRejectsOversizedRequestAndEvent(t *testing.T) {
	for _, input := range []string{
		`{"type":"graph_request","padding":"` + strings.Repeat("x", worker.MaxGraphRPCBytes) + `"}` + "\n",
		strings.Repeat("x", 32<<20) + "x",
	} {
		sink := &resultSink{graph: func(worker.GraphRequest) error {
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
