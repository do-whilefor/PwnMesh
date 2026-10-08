package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func TestGraphActionSchemaDescribesModeFields(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		fields string
	}{
		{"reason", "action assets condition depends_on description dispute_id from goal_id id latest_run_id parent_id priority reason repair sources write_paths"},
		{"explore", "assets claim description evidence observed_at reason scope sources status supersedes"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			opts := Options{Tools: []agent.Tool{}}
			if err := ConfigureRuntimeTools(Job{Kind: tc.kind}, &opts); err != nil {
				t.Fatal(err)
			}
			var schema struct {
				Properties struct {
					Payload struct {
						Properties map[string]struct {
							Type, Description string
							Enum              []string
						} `json:"properties"`
					} `json:"payload"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(opts.Tools[1].Schema, &schema); err != nil {
				t.Fatal(err)
			}
			props := schema.Properties.Payload.Properties
			var fields []string
			for name, field := range props {
				fields = append(fields, name)
				want := "string"
				switch name {
				case "from", "sources", "evidence", "depends_on", "write_paths", "assets":
					want = "array"
				case "priority":
					want = "integer"
				case "repair":
					want = "object"
				}
				if field.Type != want {
					t.Errorf("%s type = %q, want %q", name, field.Type, want)
				}
			}
			slices.Sort(fields)
			if strings.Join(fields, " ") != tc.fields {
				t.Fatalf("mode fields = %v, want %s", fields, tc.fields)
			}
			if tc.kind == "reason" {
				if !reflect.DeepEqual(props["action"].Enum, []string{"add", "achieve", "withdraw", "abandon", "priority", "retry"}) {
					t.Fatalf("missing transition discriminator guidance: %+v", props["action"])
				}
				for _, guidance := range []string{"Only goal", "and step", "Complete uses from and description without action", "the root goal is completed only by complete"} {
					if !strings.Contains(props["action"].Description, guidance) {
						t.Fatalf("missing action boundary %q: %+v", guidance, props["action"])
					}
				}
			} else if !reflect.DeepEqual(props["status"].Enum, []string{"candidate", "verified", "refuted"}) {
				t.Fatalf("candidate statuses: %+v", props["status"])
			}
		})
	}
}

func TestGraphActionSchemaPreservesOperationPayloads(t *testing.T) {
	for _, tc := range []struct{ kind, op, payload string }{
		{"reason", "goal", `{"action":"add","condition":"Check authorization","parent_id":"goal"}`},
		{"reason", "goal", `{"action":"achieve","id":"g001","reason":"Verified","sources":["fact001"]}`},
		{"reason", "goal", `{"action":"withdraw","id":"g001","reason":"No longer needed"}`},
		{"reason", "step", `{"action":"add","from":["origin"],"description":"Inspect","goal_id":"g001","priority":1000000}`},
		{"reason", "step", `{"action":"add","from":["origin"],"description":"Write report","write_paths":["/workspace/report.json"],"depends_on":["i001"]}`},
		{"reason", "step", `{"action":"abandon","id":"i001","reason":"Covered"}`},
		{"reason", "step", `{"action":"priority","id":"i001","reason":"First","priority":0}`},
		{"reason", "curation_request", `{"sources":["fact002","fact001"],"reason":"Resolve conflicting observations"}`},
		{"reason", "complete", `{"from":["fact002"],"description":"Verified proof"}`},
		{"explore", "fact", `{"description":"Observed","scope":"fixture","observed_at":"2026-09-25T01:02:03Z","evidence":[{"path":"result.txt","start_line":1,"end_line":3}]}`},
		{"explore", "candidate", `{"claim":"Observed","scope":"fixture","status":"candidate"}`},
		{"explore", "candidate", `{"claim":"Observed","scope":"fixture","status":"verified","sources":["fact001"]}`},
		{"explore", "candidate", `{"claim":"Observed","scope":"fixture","status":"refuted","sources":["fact002"],"reason":"Corrected","supersedes":"prior-note","evidence":[{"path":"retained.txt","run_id":"previous-run","excerpt":"exact text","start_line":0,"end_line":0}]}`},
	} {
		t.Run(tc.kind+"/"+tc.op+"/"+tc.payload, func(t *testing.T) {
			job := Job{Kind: tc.kind}
			if tc.kind == "reason" {
				job.Decision = &board.DecisionContext{Version: 2, StateVersion: strings.Repeat("a", 64)}
			}
			opts := Options{Tools: []agent.Tool{}}
			if err := ConfigureRuntimeTools(job, &opts); err != nil {
				t.Fatal(err)
			}
			raw := json.RawMessage(`{"op":"` + tc.op + `","idempotency_key":"probe","payload":` + tc.payload + `}`)
			if err := agent.ValidateArguments(opts.Tools[1].Schema, raw); err != nil {
				t.Fatalf("valid operation payload rejected: %v", err)
			}
		})
	}
}

func TestGraphActionSchemaRejectsMalformedScalarFields(t *testing.T) {
	for _, tc := range []struct{ kind, field, value string }{
		{"reason", "action", `[]`},
		{"reason", "description", `7`},
		{"reason", "condition", `null`},
		{"reason", "goal_id", `{}`},
		{"explore", "scope", `false`},
		{"explore", "observed_at", `7`},
		{"explore", "claim", `[]`},
		{"explore", "status", `true`},
		{"explore", "supersedes", `true`},
	} {
		t.Run(tc.kind+"/"+tc.field, func(t *testing.T) {
			schema, err := json.Marshal(orchestrationPayloadSchema(tc.kind))
			if err != nil {
				t.Fatal(err)
			}
			raw := json.RawMessage(`{"` + tc.field + `":` + tc.value + `}`)
			if err := agent.ValidateArguments(schema, raw); err == nil || !strings.Contains(err.Error(), "arguments."+tc.field+" must be") {
				t.Fatalf("missing typed rejection: %v", err)
			}
		})
	}
}

func TestGraphActionSchemaPreservesFrozenAndHistoricalEvidence(t *testing.T) {
	job := Job{Kind: "explore", RunID: "schema-evidence", GraphRPC: true, Workspace: t.TempDir()}
	opts := Options{Tools: []agent.Tool{}, RunDir: t.TempDir()}
	original := "first\nselected\nlast\n"
	source := filepath.Join(job.Workspace, "result.txt")
	if err := os.WriteFile(source, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	var submissions []json.RawMessage
	opts.Output = &draftTestBridge{dir: opts.RunDir, handle: func(request GraphRequest) (any, error) {
		submissions = append(submissions, request.Action.Payload)
		return board.StateActionResult{ID: "candidate001"}, nil
	}}
	if err := ConfigureRuntimeTools(job, &opts); err != nil {
		t.Fatal(err)
	}
	action := opts.Tools[1]
	raw := json.RawMessage(`{"op":"candidate","idempotency_key":"probe","payload":{"claim":"Observed","scope":"fixture","status":"verified","sources":["fact001"],"evidence":[{"path":"result.txt","start_line":2,"end_line":2},{"path":"retained.txt","run_id":"previous-run","excerpt":"exact text","start_line":0,"end_line":0}]}}`)
	for range 2 {
		if err := agent.ValidateArguments(action.Schema, raw); err != nil {
			t.Fatal(err)
		}
		if _, err := action.Execute(context.Background(), raw); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte("changed after submission\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if len(submissions) != 2 || string(submissions[0]) != string(submissions[1]) {
		t.Fatalf("retry changed frozen support: %s", submissions)
	}
	var payload struct {
		Sources  []string            `json:"sources"`
		Evidence []board.EvidenceRef `json:"evidence"`
	}
	if err := json.Unmarshal(submissions[0], &payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payload.Sources, []string{"fact001"}) || len(payload.Evidence) != 2 {
		t.Fatalf("support changed before server validation: %+v", payload)
	}
	fresh, historical := payload.Evidence[0], payload.Evidence[1]
	if fresh.RunID != job.RunID || fresh.Excerpt != "selected\n" || fresh.StartLine != 2 || fresh.EndLine != 2 || fresh.Path == source {
		t.Fatalf("fresh evidence was not frozen: %+v", fresh)
	}
	if retained, err := os.ReadFile(fresh.Path); err != nil || string(retained) != original {
		t.Fatalf("original evidence changed: %q %v", retained, err)
	}
	if historical != (board.EvidenceRef{Path: "retained.txt", RunID: "previous-run", Excerpt: "exact text"}) {
		t.Fatalf("historical reference changed: %+v", historical)
	}
}

func TestGraphActionFactRejectsHistoricalEvidenceWithCorrectionGuidance(t *testing.T) {
	job := Job{Kind: "explore", RunID: "consumer", GraphRPC: true, Workspace: t.TempDir()}
	opts := Options{Tools: []agent.Tool{}, RunDir: t.TempDir()}
	const original = "independently observed producer bytes\n"
	source := filepath.Join(job.Workspace, "producer.txt")
	if err := os.WriteFile(source, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	var submissions []json.RawMessage
	opts.Output = &draftTestBridge{dir: opts.RunDir, handle: func(request GraphRequest) (any, error) {
		submissions = append(submissions, request.Action.Payload)
		return board.StateActionResult{ID: "fact001"}, nil
	}}
	if err := ConfigureRuntimeTools(job, &opts); err != nil {
		t.Fatal(err)
	}
	action := opts.Tools[1]
	invalid := json.RawMessage(`{"op":"fact","idempotency_key":"observation","payload":{"description":"Checked original bytes","scope":"fixture","observed_at":"2026-10-08T10:00:00Z","evidence":[{"path":"producer.txt","run_id":"producer","excerpt":"independently observed producer bytes\n"}]}}`)
	if err := agent.ValidateArguments(action.Schema, invalid); err != nil {
		t.Fatal(err)
	}
	_, err := action.Execute(context.Background(), invalid)
	const want = "new fact evidence cannot reuse another run_id; omit run_id and excerpt and select the original local path/lines so the runtime captures evidence for this run"
	if err == nil || err.Error() != want || len(submissions) != 0 {
		t.Fatalf("historical fact reference was submitted or lacked correction guidance: calls=%d err=%v", len(submissions), err)
	}
	// Following the error can reuse this key: rejection did not freeze a
	// manifest, and the runtime captures the same source under the current run.
	corrected := json.RawMessage(`{"op":"fact","idempotency_key":"observation","payload":{"description":"Checked original bytes","scope":"fixture","observed_at":"2026-10-08T10:00:00Z","evidence":[{"path":"producer.txt"}]}}`)
	if _, err := action.Execute(context.Background(), corrected); err != nil || len(submissions) != 1 {
		t.Fatalf("corrected selection could not be submitted: calls=%d err=%v", len(submissions), err)
	}
	var payload struct {
		Evidence []board.EvidenceRef `json:"evidence"`
	}
	if err := json.Unmarshal(submissions[0], &payload); err != nil || len(payload.Evidence) != 1 {
		t.Fatalf("invalid prepared observation: %s %v", submissions[0], err)
	}
	ref := payload.Evidence[0]
	if ref.RunID != job.RunID || ref.Excerpt != original || ref.Path == source {
		t.Fatalf("runtime did not capture fresh evidence: %+v", ref)
	}
	if retained, err := os.ReadFile(ref.Path); err != nil || string(retained) != original {
		t.Fatalf("captured evidence changed original bytes: %q %v", retained, err)
	}
}
