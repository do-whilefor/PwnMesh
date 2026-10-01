//go:build linux

package workergraph

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Compare complete dispatch-input preparation in one process, including the
// owned callback copy. Checkpoint I/O and model latency are intentionally absent.
func BenchmarkNodeInputFanIn(b *testing.B) {
	for _, count := range []int{1, 16, 63} {
		initial := json.RawMessage(`{"task":"benchmark"}`)
		node := Node{ID: "join", Input: json.RawMessage(`{"scope":"join"}`)}
		state := NodeState{ID: "join", Attempt: 1}
		states := make(map[string]*NodeState, count)
		value, _ := json.Marshal(map[string]string{"stdout": strings.Repeat("x", 8000)})
		for i := 0; i < count; i++ {
			id := fmt.Sprintf("source-%d", i)
			node.DependsOn = append(node.DependsOn, Dependency{ID: id})
			states[id] = &NodeState{ID: id, Kind: "function", Status: "succeeded", Attempt: 1,
				Output: Output{Value: value, Artifacts: []Artifact{{Path: "/workspace/" + id + "/stdout.log", SHA256: strings.Repeat("a", 64)}}}}
		}
		for _, method := range []string{"eager", "borrowed"} {
			b.Run(fmt.Sprintf("dependencies=%d/%s", count, method), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					var input Input
					if method == "eager" {
						// The previous scheduler made an owned snapshot here and
						// another one at each callback invocation below.
						input = Input{NodeID: node.ID, Attempt: state.Attempt,
							Initial: append(json.RawMessage(nil), initial...), Value: append(json.RawMessage(nil), node.Input...)}
						for _, dep := range node.DependsOn {
							input.Dependencies = append(input.Dependencies, cloneNodeState(*states[dep.ID]))
						}
					} else {
						input = nodeInput(node, state, initial, states)
					}
					clonedInputBenchmarkSink = cloneInput(input)
				}
			})
		}
	}
}
