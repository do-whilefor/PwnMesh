//go:build linux

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

func TestCurationRequestStagesPrivatelyAndPublishesOnlyAtCommit(t *testing.T) {
	job, runDir := draftRunJob(t), t.TempDir()
	job.Graph.Project.OrchestrationVersion = 1
	job.State.Graph = job.Graph
	job.Decision.StateVersion = board.DecisionStateVersion(*job.State)
	requests := 0
	bridge := &draftTestBridge{dir: runDir, handle: func(r GraphRequest) (any, error) {
		requests++
		if r.Op != "decision_commit" || r.Batch == nil || r.Batch.ExpectedVersion != job.Decision.StateVersion || len(r.Batch.Actions) != 1 || r.Batch.Actions[0].Op != "curation_request" || r.Batch.Actions[0].Ref != "" {
			t.Fatalf("request was not a bound private draft: %+v", r)
		}
		var payload struct {
			Sources []string `json:"sources"`
			Reason  string   `json:"reason"`
		}
		if json.Unmarshal(r.Batch.Actions[0].Payload, &payload) != nil || strings.Join(payload.Sources, ",") != "left,right" || payload.Reason != "The retained observations conflict" {
			t.Fatal("request changed cited observations or reason")
		}
		return board.DecisionReceipt{Committed: true, ChangedActions: 1, StateVersion: job.Decision.StateVersion}, nil
	}}
	metrics := newDecisionMetrics()
	options := Options{RunDir: runDir, Output: bridge, decisionEmit: metrics.observe}
	if err := ConfigureRuntimeTools(job, &options); err != nil {
		t.Fatal(err)
	}
	tool := snapshotRuntimeTool(t, options, "graph_action")
	input := json.RawMessage(`{"op":"curation_request","idempotency_key":"review","payload":{"sources":["left","right"],"reason":"The retained observations conflict"}}`)
	for n := 0; n < 2; n++ {
		if err := agent.ValidateArguments(tool.Schema, input); err != nil {
			t.Fatal(err)
		}
		if raw, err := tool.Execute(context.Background(), input); err != nil || !strings.Contains(raw, `"draft":true`) {
			t.Fatalf("request did not stage: %s %v", raw, err)
		}
	}
	if requests != 0 || len(options.decision.actions) != 1 || metrics.DraftCalls != 2 || metrics.DraftActions != 1 || metrics.Committed {
		t.Fatalf("draft request published or duplicated: requests=%d actions=%d metrics=%+v", requests, len(options.decision.actions), metrics)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"op":"commit","idempotency_key":"publish"}`)); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || !options.decision.committed || metrics.CommittedActions != 1 {
		t.Fatal("request did not use the ordinary atomic decision commit")
	}
}

func TestCurationRequestRejectsMalformedDraftWithoutReservingKey(t *testing.T) {
	many := make([]string, 33)
	for n := range many {
		many[n] = fmt.Sprintf("fact_%d", n)
	}
	tooMany, _ := json.Marshal(map[string]any{"sources": many, "reason": "Merge observations"})
	for _, input := range []string{
		`{}`, `{"sources":[],"reason":"Merge"}`, `{"sources":["f"]}`, `{"sources":["f"],"reason":" "}`,
		`{"sources":["origin"],"reason":"Merge"}`, `{"sources":["goal"],"reason":"Merge"}`,
		`{"sources":["$later"],"reason":"Merge"}`, `{"sources":["f","f"],"reason":"Merge"}`,
		`{"sources":[7],"reason":"Merge"}`, `{"sources":["f"],"reason":"Merge","action":"add"}`,
		`{"sources":["f"],"reason":"Merge","relations":[]}`, string(tooMany),
	} {
		draft := &decisionDraft{orchestration: true, request: func(context.Context, GraphRequest) (string, error) {
			t.Fatal("malformed draft reached server")
			return "", nil
		}}
		if _, err := draft.action(context.Background(), draftTestAction("curation_request", "review", input), "version"); err == nil || len(draft.actions) != 0 || draft.version != "" {
			t.Fatalf("malformed curation request reserved a key: %s %v", input, err)
		}
		if _, err := draft.action(context.Background(), draftTestAction("curation_request", "review", `{"sources":["f"],"reason":"Resolve concrete contradiction"}`), "version"); err != nil || len(draft.actions) != 1 {
			t.Fatalf("corrected request could not reuse its key: %v", err)
		}
	}
	legacy := &decisionDraft{}
	if _, err := legacy.action(context.Background(), draftTestAction("curation_request", "review", `{"sources":["f"],"reason":"Resolve contradiction"}`), "version"); err == nil {
		t.Fatal("legacy draft enabled orchestration-only curation")
	}
}
