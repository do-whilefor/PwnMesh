//go:build linux

package workergraph

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

var clonedInputBenchmarkSink Input

// Keep the previous implementation here for a same-process comparison. This
// isolates dependency-copy cost from model latency and durable checkpoint I/O.
func BenchmarkCloneInputFanIn(b *testing.B) {
	for _, count := range []int{1, 16, 64} {
		input := Input{NodeID: "join", Attempt: 1, Initial: json.RawMessage(`{"task":"benchmark"}`), Value: json.RawMessage(`{"scope":"join"}`)}
		value, _ := json.Marshal(map[string]string{"stdout": strings.Repeat("x", 8000), "output_path": "/workspace/stdout.log"})
		for i := 0; i < count; i++ {
			input.Dependencies = append(input.Dependencies, NodeState{ID: fmt.Sprintf("source-%d", i), Kind: "agent", Status: "succeeded", Attempt: 1,
				InputSHA256: strings.Repeat("a", 64), DefinitionSHA256: strings.Repeat("b", 64),
				StartedAt: time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC), FinishedAt: time.Date(2026, 1, 2, 3, 4, 6, 7, time.UTC),
				Output: Output{Value: value, Artifacts: []Artifact{{Path: fmt.Sprintf("/workspace/source-%d/stdout.log", i), SHA256: strings.Repeat("c", 64)}}},
			})
		}
		for _, method := range []string{"json", "typed"} {
			b.Run(fmt.Sprintf("dependencies=%d/%s", count, method), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(input.Initial) + len(input.Value) + count*len(value)))
				if method == "json" {
					for i := 0; i < b.N; i++ {
						raw, err := json.Marshal(input)
						if err != nil {
							b.Fatal(err)
						}
						var copied Input
						if err := json.Unmarshal(raw, &copied); err != nil {
							b.Fatal(err)
						}
						clonedInputBenchmarkSink = copied
					}
				} else {
					for i := 0; i < b.N; i++ {
						clonedInputBenchmarkSink = cloneInput(input)
					}
				}
			})
		}
	}
}
