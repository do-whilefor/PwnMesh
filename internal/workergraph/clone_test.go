//go:build linux

package workergraph

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestClonedInputOwnsAllMutableSlices(t *testing.T) {
	newInput := func() Input {
		return Input{NodeID: "join", Attempt: 1,
			Initial: json.RawMessage(" {\"initial\": true} "), Value: json.RawMessage(`{"task":"join"}`),
			Dependencies: []NodeState{{ID: "source", Kind: "agent", Status: "failed", Attempt: 1,
				DefinitionSHA256: "definition", InputSHA256: "input", Error: "original failure", Reason: "original reason",
				StartedAt: time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC), FinishedAt: time.Date(2026, 1, 2, 3, 4, 6, 7, time.UTC),
				ReadyAt: time.Date(2026, 1, 2, 3, 4, 4, 5, time.UTC), ConditionDurationMS: 1, RunDurationMS: 2, VerifyDurationMS: 3,
				ReconcileDurationMS: 4, RecoveryVerifyDurationMS: 5,
				Output: Output{Value: json.RawMessage(" {\"result\": \"<raw>\"} "), Artifacts: []Artifact{{Path: "/evidence", SHA256: "digest"}}},
			}},
		}
	}
	mutate := func(input Input) {
		input.Initial[1] = '['
		input.Value[0] = '['
		input.Dependencies[0].ID = "changed"
		input.Dependencies[0].Error = "changed"
		input.Dependencies[0].Output.Value[1] = '['
		input.Dependencies[0].Output.Artifacts[0].Path = "/changed"
		input.Dependencies[0].Output.Artifacts[0].SHA256 = "changed"
	}
	for _, direction := range []string{"callback", "owner"} {
		t.Run(direction, func(t *testing.T) {
			original, want := newInput(), newInput()
			copied := cloneInput(original)
			if !reflect.DeepEqual(copied, want) {
				t.Fatal("cloning changed raw input bytes or node metadata")
			}
			if direction == "callback" {
				mutate(copied)
				if !reflect.DeepEqual(original, want) {
					t.Fatal("callback mutation escaped its owned input")
				}
			} else {
				mutate(original)
				if !reflect.DeepEqual(copied, want) {
					t.Fatal("owner mutation changed an existing input snapshot")
				}
			}
		})
	}
}

func TestClonePreservesNilAndEmptySlices(t *testing.T) {
	for _, input := range []Input{
		{},
		{Initial: json.RawMessage{}, Value: json.RawMessage{}, Dependencies: []NodeState{}},
		{Dependencies: []NodeState{{Output: Output{}}}},
		{Dependencies: []NodeState{{Output: Output{Value: json.RawMessage{}, Artifacts: []Artifact{}}}}},
	} {
		if copied := cloneInput(input); !reflect.DeepEqual(copied, input) {
			t.Fatalf("nil or empty slice changed: want %#v; got %#v", input, copied)
		}
	}
}

func TestCloneKeepsMalformedOutputAvailableForValidation(t *testing.T) {
	state := NodeState{ID: "failed", Error: "original failure", Reason: "original reason",
		Output: Output{Value: json.RawMessage(`{"unfinished":`), Artifacts: []Artifact{{Path: "/evidence", SHA256: "digest"}}}}
	copied := cloneNodeState(state)
	if !reflect.DeepEqual(copied, state) || validateOutput(copied.Output) == nil {
		t.Fatal("cloning hid malformed output or discarded failure metadata")
	}
	copied.Output.Value[0] = '['
	copied.Output.Artifacts[0].Path = "/changed"
	if string(state.Output.Value) != `{"unfinished":` || state.Output.Artifacts[0].Path != "/evidence" {
		t.Fatal("malformed output shares mutable storage")
	}
}
