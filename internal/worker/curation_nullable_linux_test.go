//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func TestCuratorNormalizesOnlyOptionalDisputeNullsThroughLoop(t *testing.T) {
	j, dir := curationJob(t), t.TempDir()
	version := board.DecisionStateVersion(*j.State)
	calls, commits := 0, 0
	bridge := &draftTestBridge{dir: dir, handle: func(request GraphRequest) (any, error) {
		if request.Op == "curate_receipt" {
			return board.StateActionResult{}, nil
		}
		if request.Op != "graph_action" || request.Action.Op != "curate" {
			t.Fatalf("unexpected request: %+v", request)
		}
		var payload struct {
			ThroughRevision int64                        `json:"through_revision"`
			Groups          []map[string]json.RawMessage `json:"groups"`
		}
		if err := json.Unmarshal(request.Action.Payload, &payload); err != nil || len(payload.Groups) != 2 || payload.ThroughRevision != j.curationRevision() {
			t.Fatalf("invalid normalized payload: %s, %v", request.Action.Payload, err)
		}
		for _, field := range []string{"dispute_id", "review_fact_ids", "resolution"} {
			if _, exists := payload.Groups[0][field]; exists {
				t.Fatalf("null optional %s reached the service: %s", field, request.Action.Payload)
			}
			if _, exists := payload.Groups[1][field]; !exists {
				t.Fatalf("non-null optional %s disappeared: %s", field, request.Action.Payload)
			}
		}
		if string(payload.Groups[0]["reason"]) != `"Original evidence"` || string(payload.Groups[1]["resolution"]) != `"uncertain"` {
			t.Fatal("normalization changed substantive curation input")
		}
		commits++
		return board.StateActionResult{Committed: true, StateVersion: version}, nil
	}}
	result, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Output: bridge, Provider: scenarioProvider(func(_ context.Context, _ []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls > 1 {
			t.Fatal("optional nulls forced a corrective model turn")
		}
		return draftModelCall("curate", "graph_action", `{"op":"curate","idempotency_key":"batch","payload":{"groups":[{"candidate_ids":["candidate_a"],"status":"verified","reason":"Original evidence","dispute_id":null,"review_fact_ids":null,"resolution":null},{"candidate_ids":["candidate_b"],"status":"candidate","reason":"Review remains uncertain","dispute_id":"d1","review_fact_ids":["f1"],"resolution":"uncertain"}]}}`), nil
	})})
	if err != nil || result.Status != "success" || commits != 1 || calls != 1 {
		t.Fatalf("nullable curation did not complete in one call: %+v, %v, calls=%d commits=%d", result, err, calls, commits)
	}
}

func TestCuratorNullableSchemaRejectsOtherMalformedFields(t *testing.T) {
	opts := Options{}
	if err := ConfigureRuntimeTools(curationJob(t), &opts); err != nil {
		t.Fatal(err)
	}
	var schema json.RawMessage
	for _, tool := range opts.Tools {
		if tool.Name == "graph_action" {
			schema = tool.Schema
		}
	}
	base := `{"op":"curate","idempotency_key":"batch","payload":{"groups":[{"candidate_ids":["a"],"status":"candidate","reason":"r"FIELDS}]}}`
	for _, fields := range []string{
		`,"question":null`, `,"unexpected":null`, `,"dispute_id":7`,
		`,"review_fact_ids":{}`, `,"review_fact_ids":[null]`, `,"review_fact_ids":["a",false]`,
		`,"resolution":"closed"`, `,"resolution":[]`,
	} {
		if err := agent.ValidateArguments(schema, json.RawMessage(strings.Replace(base, "FIELDS", fields, 1))); err == nil {
			t.Fatalf("invalid field passed curator schema: %s", fields)
		}
	}
	for _, payload := range []string{
		`{"groups":null}`, `{"groups":[null]}`, `{"groups":[{"candidate_ids":null,"status":"candidate","reason":"r"}]}`,
		`{"groups":[{"candidate_ids":["a"],"status":null,"reason":"r"}]}`,
		`{"groups":[{"candidate_ids":["a"],"status":"candidate","reason":null}]}`,
		`{"groups":[],"through_revision":null}`, `{"groups":[],"relations":null}`,
	} {
		raw := json.RawMessage(`{"op":"curate","idempotency_key":"batch","payload":` + payload + `}`)
		if err := agent.ValidateArguments(schema, raw); err == nil {
			t.Fatalf("required or unrelated null passed curator schema: %s", payload)
		}
	}
}
