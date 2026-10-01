//go:build linux

package workergraph

import (
	"context"
	"encoding/json"
	"testing"
)

func TestLaterNodesKeepTheOriginalInitialInputSnapshot(t *testing.T) {
	options := fixtureOptions(t, 1)
	options.Input = json.RawMessage(`{"scope":"original"}`)
	definition := Definition{Version: "input-isolation", Nodes: []Node{
		{ID: "first", Kind: "function", Run: func(context.Context, Input) (Output, error) {
			copy(options.Input, []byte(`{"scope":"modified"}`))
			return Output{}, nil
		}},
		{ID: "second", Kind: "function", DependsOn: []Dependency{{ID: "first"}}, Run: func(_ context.Context, input Input) (Output, error) {
			if string(input.Initial) != `{"scope":"original"}` {
				t.Errorf("later node received a changed initial input: %s", input.Initial)
			}
			return Output{}, nil
		}},
	}}
	checkpoint, err := Run(context.Background(), definition, options)
	if err != nil || checkpoint.Status != "succeeded" || checkpoint.InputSHA256 != hash(json.RawMessage(`{"scope":"original"}`)) {
		t.Fatalf("graph lost its initial input binding: %+v %v", checkpoint, err)
	}
}
