package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func TestStepRetryToolsAndPromptDescribeAuthorization(t *testing.T) {
	const protocol = 1
	j := Job{Kind: "reason", GraphRPC: true, Graph: board.Graph{Project: board.Project{OrchestrationVersion: protocol}}}
	opts := Options{}
	if err := ConfigureRuntimeTools(j, &opts); err != nil {
		t.Fatal(err)
	}
	action := opts.Tools[1]
	err := agent.ValidateArguments(action.Schema, json.RawMessage(`{"op":"step","idempotency_key":"retry","payload":{"action":"retry","id":"failed-step","latest_run_id":"failed-run","reason":"The prerequisite is now ready"}}`))
	if err != nil {
		t.Fatalf("protocol %d has incorrect retry schema: %v", protocol, err)
	}
	prompt, err := Prompt(j, false, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Use step retry") || !strings.Contains(action.Description, "retry authorizes one new execution") {
		t.Fatalf("protocol %d advertises the wrong retry contract", protocol)
	}
}

func TestStepRetryDraftStagesOnceAndUsesNormalBatchReceipt(t *testing.T) {
	requests := 0
	d := &decisionDraft{orchestration: true, request: func(_ context.Context, request GraphRequest) (string, error) {
		requests++
		if request.Batch == nil || request.Batch.ExpectedVersion != "original" || len(request.Batch.Actions) != 1 || request.Batch.Actions[0].Ref != "" {
			t.Fatalf("retry changed identity or invented a new Step: %+v", request)
		}
		var payload map[string]string
		if err := json.Unmarshal(request.Batch.Actions[0].Payload, &payload); err != nil || payload["action"] != "retry" || payload["id"] != "failed-step" || payload["latest_run_id"] != "failed-run" || payload["reason"] != "The prerequisite is now ready" || len(payload) != 4 {
			t.Fatalf("retry contract changed before publication: %+v %v", payload, err)
		}
		switch request.Op {
		case "decision_preview":
			return `{"committed":false}`, nil
		case "decision_commit":
			return `{"committed":true}`, nil
		default:
			t.Fatalf("retry escaped the batch protocol: %+v", request)
			return "", nil
		}
	}}
	action := draftTestAction("step", "retry", `{"action":"retry","id":"failed-step","latest_run_id":"failed-run","reason":"The prerequisite is now ready"}`)
	for range 2 {
		if reply, err := d.action(context.Background(), action, "original"); err != nil || strings.Contains(reply, "$retry") {
			t.Fatalf("retry failed to stage or created a new Step alias: %s %v", reply, err)
		}
	}
	if requests != 0 || len(d.actions) != 1 {
		t.Fatal("retry published before commit or reserved duplicate actions")
	}
	if _, err := d.action(context.Background(), draftTestAction("preview", "preview", ""), "later"); err != nil || d.committed {
		t.Fatalf("retry preview was treated as a committed plan: %v", err)
	}
	if _, err := d.action(context.Background(), draftTestAction("commit", "commit", ""), "later"); err != nil || !d.committed || requests != 2 {
		t.Fatalf("retry did not use the normal commit receipt: %v requests=%d", err, requests)
	}
}

func TestStepRetryDraftRejectsMissingFieldsAndInputChanges(t *testing.T) {
	for _, payload := range []string{
		`{"action":"retry","id":"failed-step","latest_run_id":"failed-run"}`,
		`{"action":"retry","latest_run_id":"failed-run","reason":"Try again"}`,
		`{"action":"retry","id":"failed-step","reason":"Try again"}`,
		`{"action":"retry","id":"failed-step","latest_run_id":" ","reason":"Try again"}`,
		`{"action":"retry","id":"failed-step","latest_run_id":"failed-run","reason":" "}`,
		`{"action":"retry","id":"failed-step","latest_run_id":"failed-run","reason":"Try again","from":["origin"]}`,
		`{"action":"retry","id":"failed-step","latest_run_id":"failed-run","reason":"Try again","depends_on":[]}`,
		`{"action":"retry","id":"failed-step","latest_run_id":"failed-run","reason":"Try again","description":"Changed task"}`,
		`{"action":"retry","id":"failed-step","latest_run_id":"failed-run","reason":"Try again","priority":0}`,
		`{"action":"retry","id":"failed-step","latest_run_id":"failed-run","reason":"Try again","dispute_id":"different"}`,
		`{"action":"retry","id":"failed-step","latest_run_id":"failed-run","reason":"Try again","run_id":"model-chosen"}`,
	} {
		t.Run(payload, func(t *testing.T) {
			d := &decisionDraft{orchestration: true}
			if _, err := d.action(context.Background(), draftTestAction("step", "correctable", payload), "bad"); err == nil || !strings.Contains(err.Error(), "draft unchanged") {
				t.Fatalf("invalid retry was accepted: %v", err)
			}
			if len(d.actions) != 0 || len(d.keys) != 0 || d.version != "" {
				t.Fatal("invalid retry reserved its key or input version")
			}
			if _, err := d.action(context.Background(), draftTestAction("step", "correctable", `{"action":"retry","id":"failed-step","latest_run_id":"failed-run","reason":"The prerequisite is now ready"}`), "good"); err != nil {
				t.Fatal(err)
			}
		})
	}
	legacy := &decisionDraft{}
	if _, err := legacy.action(context.Background(), draftTestAction("step", "retry", `{"action":"retry","id":"failed-step","latest_run_id":"failed-run","reason":"Try again"}`), "original"); err == nil {
		t.Fatal("legacy draft accepted main-agent retry authorization")
	}
}
