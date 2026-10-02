//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
)

func TestDecisionRefreshEvidenceSurvivesCompactionBeforeRestaging(t *testing.T) {
	ctx := context.Background()
	commits := 0
	d := &decisionDraft{request: func(_ context.Context, r GraphRequest) (string, error) {
		if r.Op != "decision_commit" || r.Batch.ExpectedVersion != "current" || len(r.Batch.Actions) != 1 {
			t.Fatalf("unexpected refreshed transaction: %+v", r)
		}
		commits++
		return `{"committed":true}`, nil
	}}
	d.invalidate()
	overview := `{"state_version":"current","hints":[{"id":"h001","content":"Only the revised endpoint is authorized"}]}`
	continuation := `{"state_version":"current","content":"Retain the full overview continuation"}`
	facts := `{"state_version":"current","items":[{"id":"corrected","description":"The old endpoint returned the wrong response"}]}`
	d.observeRead("overview", overview)
	d.observeRead("overview", continuation)
	d.observeRead("facts", facts)
	if _, err := d.action(ctx, draftTestAction("step", "unseen", `{"action":"add","from":["corrected"],"description":"Preplanned action"}`), "current"); err == nil {
		t.Fatal("unseen refreshed tool results authorized a preplanned action")
	}
	// The recent read group deliberately exceeds RecentBytes, so the normal
	// model receives a summary that omits both corrections unless runtime data
	// is pinned independently of that group.
	input, _ := json.Marshal(overview + "\n" + continuation + "\n" + facts)
	summaries, requests := 0, 0
	loop := &agent.Loop{
		TaskPrompt: "Original task",
		History: []agent.Message{
			agent.Text("user", "Original task"),
			agent.Text("assistant", strings.Repeat("Earlier obsolete investigation. ", 4000)),
			draftModelCall("refresh", "read_graph", `{"section":"facts"}`),
			{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "refresh", Content: input}}},
		},
		ContextBytes: 12000, RecentBytes: 1, SummaryBytes: 300,
		StopResult: d.result,
		Tools: []agent.Tool{{Definition: agent.Definition{Name: "publish", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			if _, err := d.action(ctx, draftTestAction("step", "fresh", `{"action":"add","from":["corrected"],"description":"Inspect only the revised endpoint"}`), "current"); err != nil {
				return "", err
			}
			return d.action(ctx, draftTestAction("commit", "commit", `{}`), "current")
		}}},
		BeforeRequest: func(ctx context.Context, loop *agent.Loop) (context.Context, error) {
			d.beforeRequest(loop)
			return ctx, nil
		},
		Provider: scenarioProvider(func(_ context.Context, history []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
			if len(defs) == 0 {
				summaries++
				return agent.Text("assistant", `{"notes":"Earlier work summarized without the updated observations.","quotes":[]}`), nil
			}
			requests++
			raw, _ := json.Marshal(history)
			for _, required := range []string{"Only the revised endpoint is authorized", "Retain the full overview continuation", "The old endpoint returned the wrong response", "decision_refresh"} {
				if !strings.Contains(string(raw), required) {
					t.Fatalf("compaction erased unread refreshed evidence: %s", required)
				}
			}
			return draftModelCall("publish", "publish", `{}`), nil
		}),
	}
	if _, err := loop.Run(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if summaries != 1 || requests != 1 || commits != 1 || len(d.refreshData) != 0 {
		t.Fatalf("summaries=%d requests=%d commits=%d retained=%d", summaries, requests, commits, len(d.refreshData))
	}
	d.beforeRequest(loop)
	if len(loop.ContextData) != 0 {
		t.Fatal("already observed refresh pages stayed pinned on later requests")
	}
}

func TestDecisionRefreshOversizedEvidenceFailsBeforeModelOrCommit(t *testing.T) {
	d := &decisionDraft{}
	d.invalidate()
	d.observeRead("overview", `{"state_version":"current"}`)
	d.observeRead("facts", strings.Repeat("Required correction. ", 4000))
	loop := &agent.Loop{
		TaskPrompt: "Original task", History: []agent.Message{agent.Text("user", "Original task"), agent.Text("assistant", strings.Repeat("Older history. ", 4000))},
		ContextBytes: 12000, RecentBytes: 1000, SummaryBytes: 300,
		BeforeRequest: func(ctx context.Context, loop *agent.Loop) (context.Context, error) {
			d.beforeRequest(loop)
			return ctx, nil
		},
		Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
			t.Fatal("oversized mandatory corrections were dropped to issue a model request")
			return agent.Message{}, nil
		}),
	}
	if _, err := loop.Run(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "input budget") {
		t.Fatalf("oversized pinned corrections did not fail closed: %v", err)
	}
	if d.committed || len(d.actions) != 0 {
		t.Fatal("unread oversized corrections published a draft")
	}
}
