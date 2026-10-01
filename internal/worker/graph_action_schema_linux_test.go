package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func TestCandidateSchemaAllowsTentativeNotesAndExplicitRevisions(t *testing.T) {
	schema, err := json.Marshal(orchestrationPayloadSchema("explore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{
		`{"claim":"The response may depend on the session","scope":"fixture","sources":["f1"],"reason":"One observed response; needs a comparison"}`,
		`{"claim":"The response may depend on the session","scope":"fixture","status":"refuted","sources":["f2"],"reason":"A fresh comparison contradicts it","supersedes":"c1"}`,
	} {
		if err := agent.ValidateArguments(schema, json.RawMessage(payload)); err != nil {
			t.Fatalf("tentative note/revision is not expressible: %v", err)
		}
	}
	for _, payload := range []string{`{"supersedes":[]}`, `{"supersedes":null}`, `{"status":"certain"}`} {
		if err := agent.ValidateArguments(schema, json.RawMessage(payload)); err == nil {
			t.Fatalf("malformed note accepted: %s", payload)
		}
	}
}

func TestGraphActionRejectsMalformedCollectionsBeforeExecution(t *testing.T) {
	for _, tc := range []struct {
		name, kind, op, payload, path string
	}{
		{"from object", "reason", "step", `{"action":"add","from":{"item":"origin"},"description":"Inspect"}`, "from"},
		{"from null", "reason", "step", `{"action":"add","from":null}`, "from"},
		{"from element", "reason", "step", `{"action":"add","from":[{"item":"origin"}]}`, "from[0]"},
		{"priority string", "reason", "step", `{"action":"add","from":["origin"],"description":"Inspect","priority":"high"}`, "priority"},
		{"priority fraction", "reason", "step", `{"action":"priority","id":"i001","reason":"First","priority":1.5}`, "priority"},
		{"priority negative", "reason", "step", `{"action":"priority","id":"i001","reason":"First","priority":-1}`, "priority"},
		{"priority excessive", "reason", "step", `{"action":"priority","id":"i001","reason":"First","priority":1000001}`, "priority"},
		{"sources object", "reason", "goal", `{"action":"achieve","sources":{"item":"fact001"}}`, "sources"},
		{"sources element", "explore", "candidate", `{"sources":[7]}`, "sources[0]"},
		{"evidence object", "explore", "fact", `{"evidence":{"path":"result.txt"}}`, "evidence"},
		{"evidence null", "explore", "candidate", `{"evidence":null}`, "evidence"},
		{"evidence element", "explore", "fact", `{"evidence":["result.txt"]}`, "evidence[0]"},
		{"evidence path", "explore", "fact", `{"evidence":[{"path":7}]}`, "evidence[0].path"},
		{"evidence run", "explore", "candidate", `{"evidence":[{"run_id":false}]}`, "evidence[0].run_id"},
		{"evidence excerpt", "explore", "candidate", `{"evidence":[{"excerpt":[]}]}`, "evidence[0].excerpt"},
		{"evidence start", "explore", "fact", `{"evidence":[{"start_line":"1"}]}`, "evidence[0].start_line"},
		{"evidence end", "explore", "fact", `{"evidence":[{"end_line":1.5}]}`, "evidence[0].end_line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{RunDir: t.TempDir(), Tools: []agent.Tool{}}
			requests := 0
			opts.Output = &draftTestBridge{dir: opts.RunDir, handle: func(GraphRequest) (any, error) {
				requests++
				return board.DecisionReceipt{}, nil
			}}
			job := Job{Kind: tc.kind, RunID: "schema-test", GraphRPC: true}
			if tc.kind == "reason" {
				job.Decision = &board.DecisionContext{Version: 2, StateVersion: strings.Repeat("a", 64)}
			}
			if err := ConfigureRuntimeTools(job, &opts); err != nil {
				t.Fatal(err)
			}
			executed := 0
			for n := range opts.Tools {
				if opts.Tools[n].Name == "graph_action" {
					execute := opts.Tools[n].Execute
					opts.Tools[n].Execute = func(ctx context.Context, raw json.RawMessage) (string, error) {
						executed++
						return execute(ctx, raw)
					}
				}
			}
			calls := 0
			loop := agent.Loop{Tools: opts.Tools, Provider: scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				if calls == 1 {
					return draftModelCall("malformed", "graph_action", `{"op":"`+tc.op+`","idempotency_key":"probe","payload":`+tc.payload+`}`), nil
				}
				results := history[len(history)-1].Content
				if calls != 2 || len(results) != 1 || !results[0].IsError || !strings.Contains(string(results[0].Content), "arguments.payload."+tc.path+" must be") {
					t.Fatalf("missing typed rejection: %+v", results)
				}
				return agent.Text("assistant", "done"), nil
			})}
			if _, err := loop.Run(context.Background(), "Inspect"); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || executed != 0 || requests != 0 || (opts.decision != nil && len(opts.decision.actions) != 0) {
				t.Fatalf("malformed input reached execution: calls=%d executed=%d requests=%d draft=%+v", calls, executed, requests, opts.decision)
			}
		})
	}
}

func TestGraphActionCorrectedArrayReusesKeyAndPreviews(t *testing.T) {
	requests := 0
	opts, _, action := draftTestTools(t, func(request GraphRequest) (any, error) {
		requests++
		if request.Op != "decision_preview" || request.Batch == nil || len(request.Batch.Actions) != 1 || request.Batch.Actions[0].Ref != "probe" {
			t.Fatalf("unexpected preview: %+v", request)
		}
		var payload struct {
			From []string `json:"from"`
		}
		if err := json.Unmarshal(request.Batch.Actions[0].Payload, &payload); err != nil || len(payload.From) != 1 || payload.From[0] != "origin" {
			t.Fatalf("array changed before preview: %+v %v", payload, err)
		}
		return board.DecisionReceipt{}, nil
	})
	calls := 0
	loop := agent.Loop{Tools: []agent.Tool{action}, Provider: scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls > 1 {
			results := history[len(history)-1].Content
			if len(results) != 1 || results[0].IsError != (calls == 2) {
				t.Fatalf("unexpected tool result at turn %d: %+v", calls, results)
			}
		}
		switch calls {
		case 1:
			return draftModelCall("bad", "graph_action", `{"op":"step","idempotency_key":"probe","payload":{"action":"add","from":{"item":"origin"},"description":"Inspect"}}`), nil
		case 2:
			return draftModelCall("corrected", "graph_action", `{"op":"step","idempotency_key":"probe","payload":{"action":"add","from":["origin"],"description":"Inspect"}}`), nil
		case 3:
			return draftModelCall("preview", "graph_action", `{"op":"preview","idempotency_key":"preview","payload":{}}`), nil
		default:
			return agent.Text("assistant", "done"), nil
		}
	})}
	if _, err := loop.Run(context.Background(), "Inspect"); err != nil || calls != 4 || requests != 1 || len(opts.decision.actions) != 1 {
		t.Fatalf("corrected draft could not preview: calls=%d requests=%d draft=%+v err=%v", calls, requests, opts.decision, err)
	}
}

func TestGraphActionCollectionSchemaPreservesSupportedPayloads(t *testing.T) {
	opts := Options{Tools: []agent.Tool{}}
	if err := ConfigureRuntimeTools(Job{Kind: "explore"}, &opts); err != nil {
		t.Fatal(err)
	}
	action := opts.Tools[1]
	for _, payload := range []string{
		`{}`,
		`{"from":[],"sources":[],"evidence":[]}`,
		`{"from":["origin"],"sources":["fact001","fact002"]}`,
		`{"evidence":[{"path":"result.txt"}]}`,
		`{"evidence":[{"path":"retained.txt","run_id":"previous-run","excerpt":"exact text","start_line":0,"end_line":0}]}`,
		`{"evidence":[{"path":"result.txt","start_line":1,"end_line":3}]}`,
		`{"claim":"Observed","scope":"fixture","status":"verified","reason":"Correction","supersedes":"prior-note","sources":["fact001"]}`,
	} {
		raw := json.RawMessage(`{"op":"candidate","idempotency_key":"probe","payload":` + payload + `}`)
		if err := agent.ValidateArguments(action.Schema, raw); err != nil {
			t.Errorf("supported shape rejected: %s: %v", payload, err)
		}
	}
}

func TestPlanningFieldsDistinguishSourceFactsFromGoals(t *testing.T) {
	for _, schema := range []map[string]any{graphActionPayloadSchema("reason"), orchestrationPayloadSchema("reason")} {
		properties := schema["properties"].(map[string]any)
		from := properties["from"].(map[string]any)["description"].(string)
		goal := properties["goal_id"].(map[string]any)["description"].(string)
		if !strings.Contains(from, "Step inputs: published, effective Fact IDs or origin") || !strings.Contains(from, "Complete: published, effective Fact IDs only, never origin") || !strings.Contains(from, "goal is a user constraint, never a source") || !strings.Contains(goal, "Never put this ID in from") {
			t.Fatalf("planning fields conflate source evidence with assignment: from=%q goal_id=%q", from, goal)
		}
	}
}

func TestEvidenceSelectionSchemasDescribeRuntimeLimits(t *testing.T) {
	schemas := map[string]map[string]any{}
	for _, kind := range []string{"explore", "curate"} {
		schemas["legacy_"+kind] = graphActionPayloadSchema(kind)
		if kind != "curate" {
			schemas["orchestration_"+kind] = orchestrationPayloadSchema(kind)
		}
	}
	var finish map[string]any
	if err := json.Unmarshal((&stepFinish{}).tool().Schema, &finish); err != nil {
		t.Fatal(err)
	}
	schemas["finish_step"] = finish["properties"].(map[string]any)["fact"].(map[string]any)
	for name, schema := range schemas {
		t.Run(name, func(t *testing.T) {
			evidence := schema["properties"].(map[string]any)["evidence"].(map[string]any)
			description, _ := evidence["items"].(map[string]any)["description"].(string)
			for _, requirement := range []string{
				"UTF-8", "nonempty",
				fmt.Sprintf("at most %d MiB", maxEvidenceFileBytes/(1<<20)),
				fmt.Sprintf("at most %d bytes", maxEvidenceExcerptBytes),
				"Omit both line bounds for the whole file",
				"start_line and end_line together (1-based, inclusive)",
			} {
				if !strings.Contains(description, requirement) {
					t.Errorf("evidence schema omits runtime requirement %q: %q", requirement, description)
				}
			}
		})
	}
}

func TestFactScopeSchemasPreserveAssignedValuesAndAllowFreeScopes(t *testing.T) {
	schemas := map[string]map[string]any{
		"legacy_explore":        graphActionPayloadSchema("explore"),
		"legacy_curate":         graphActionPayloadSchema("curate"),
		"orchestration_explore": orchestrationPayloadSchema("explore"),
	}
	var finish map[string]any
	if err := json.Unmarshal((&stepFinish{}).tool().Schema, &finish); err != nil {
		t.Fatal(err)
	}
	schemas["finish_step"] = finish["properties"].(map[string]any)["fact"].(map[string]any)
	for name, schema := range schemas {
		t.Run(name, func(t *testing.T) {
			scope := schema["properties"].(map[string]any)["scope"].(map[string]any)
			description, _ := scope["description"].(string)
			for _, requirement := range []string{"task-specified scope value exactly", "additional explanation in description"} {
				if !strings.Contains(description, requirement) {
					t.Errorf("fact scope omits %q: %q", requirement, description)
				}
			}
			rawSchema, err := json.Marshal(schema)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{"free form local observation", "tenant/demo:route α"} {
				payload, _ := json.Marshal(map[string]any{
					"description": "Observed result", "scope": value, "observed_at": "2026-10-01T00:00:00Z",
					"evidence": []map[string]string{{"path": "result.txt"}},
				})
				if err := agent.ValidateArguments(rawSchema, payload); err != nil {
					t.Errorf("scope guidance restricted an otherwise valid free scope %q: %v", value, err)
				}
			}
		})
	}
}

func TestGraphActionOptionalPayloadOnlyForDraftControls(t *testing.T) {
	for _, op := range []string{"preview", "commit", "reset"} {
		for _, suffix := range []string{"", `,"payload":{}`} {
			t.Run(op+suffix, func(t *testing.T) {
				requests := 0
				opts, _, action := draftTestTools(t, func(request GraphRequest) (any, error) {
					requests++
					if request.Op != "decision_"+op {
						t.Fatalf("unexpected request: %+v", request)
					}
					return board.DecisionReceipt{Committed: op == "commit", StateVersion: strings.Repeat("a", 64)}, nil
				})
				raw := json.RawMessage(`{"op":"` + op + `","idempotency_key":"control"` + suffix + `}`)
				if err := agent.ValidateArguments(action.Schema, raw); err != nil {
					t.Fatal(err)
				}
				if _, err := action.Execute(context.Background(), raw); err != nil {
					t.Fatal(err)
				}
				wantRequests := 1
				if op == "reset" {
					wantRequests = 0
				}
				if requests != wantRequests || opts.decision.committed != (op == "commit") {
					t.Fatalf("wrong control result: requests=%d draft=%+v", requests, opts.decision)
				}
			})
		}
	}
	for _, op := range []string{"goal", "step", "curation_request", "complete"} {
		t.Run(op, func(t *testing.T) {
			opts, _, action := draftTestTools(t, func(GraphRequest) (any, error) {
				t.Fatal("missing business payload reached the board")
				return nil, nil
			})
			raw := json.RawMessage(`{"op":"` + op + `","idempotency_key":"missing"}`)
			if _, err := action.Execute(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "payload must be an object") {
				t.Fatalf("missing business payload was accepted: %v", err)
			}
			if len(opts.decision.actions) != 0 || opts.decision.version != "" {
				t.Fatal("missing payload changed the draft")
			}
		})
	}
	for _, kind := range []string{"reason", "explore"} {
		opts := Options{Tools: []agent.Tool{}}
		if err := ConfigureRuntimeTools(Job{Kind: kind}, &opts); err != nil {
			t.Fatal(err)
		}
		if err := agent.ValidateArguments(opts.Tools[1].Schema, json.RawMessage(`{"op":"step","idempotency_key":"missing"}`)); err == nil || !strings.Contains(err.Error(), "missing argument arguments.payload") {
			t.Fatalf("non-batch %s no longer requires payload: %v", kind, err)
		}
	}
}
