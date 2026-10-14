//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	eventlog "pwnmesh/internal/worker/internal/journal"
	"reflect"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func TestDecisionMetricsSeparateDraftsFromCommittedReceipts(t *testing.T) {
	m := newDecisionMetrics()
	m.observe(agent.Event{Type: "tool_start", ToolName: "graph_action"})
	m.observe(agent.Event{Type: "tool_end", ToolName: "graph_action"})
	m.observe((decisionOperation{Op: "draft", Actions: 2}).event())
	m.observe((decisionOperation{Op: "draft", Failed: true}).event())
	m.observe((decisionOperation{Op: "decision_preview", Committed: true, Actions: 2}).event())
	j := Job{Kind: "reason", Decision: &board.DecisionContext{Version: 2}}
	finish := func() DecisionMetrics { return m.finish(j, Result{Status: "success"}, time.Time{}, time.Time{}) }
	if got := finish(); got.Outcome != "draft_only_observed" || got.Committed || got.DraftCalls != 2 || got.DraftActions != 2 || got.DraftFailures != 1 || got.PreviewCalls != 1 {
		t.Fatalf("draft or preview misreported as publication: %+v", got)
	}
	m.observe((decisionOperation{Op: "read_graph", Failed: true, StateChanged: true}).event())
	m.observe((decisionOperation{Op: "decision_preview", Failed: true, StateChanged: true}).event())
	m.observe((decisionOperation{Op: "decision_commit", Failed: true, ElapsedMS: 37}).event())
	if got := finish(); got.Committed || got.StateChanged != 2 || got.CommitCalls != 1 || got.CommitFailures != 1 || got.CommitDurationMS != 37 || got.PreviewFailures != 1 {
		t.Fatalf("uncertain commit metrics: %+v", got)
	}
	m.observe((decisionOperation{Op: "decision_receipt", Failed: true}).event())
	m.observe((decisionOperation{Op: "decision_receipt", Committed: true, Actions: 2}).event())
	m.observe((decisionOperation{Op: "decision_receipt", Committed: true, Actions: 2}).event())
	if got := finish(); got.Outcome != "actions_committed" || !got.Committed || got.CommittedActions != 2 || got.ReceiptCalls != 3 || got.ReceiptFailures != 1 {
		t.Fatalf("receipt recovery must confirm one batch: %+v", got)
	}
}

func TestDecisionMetricsRequireCurrentReceiptAuthority(t *testing.T) {
	m := newDecisionMetrics()
	m.observe((decisionOperation{Op: "decision_commit", Committed: true, ElapsedMS: 4}).event())
	batch := Job{Kind: "reason", Decision: &board.DecisionContext{Version: 2}}
	if got := m.finish(batch, Result{Status: "success"}, time.Time{}, time.Time{}); got.Outcome != "no_op_committed" || !got.Committed || got.CommittedActions != 0 {
		t.Fatalf("empty batch receipt: %+v", got)
	}
	for _, decision := range []*board.DecisionContext{nil, {Version: 1}, {Version: 2}} {
		uncommitted := newDecisionMetrics()
		uncommitted.observe(agent.Event{Type: "tool_start", ToolName: "graph_action"})
		uncommitted.observe(agent.Event{Type: "tool_end", ToolName: "graph_action"})
		if got := uncommitted.finish(Job{Kind: "reason", Decision: decision}, Result{Status: "success"}, time.Time{}, time.Time{}); got.Outcome != "unknown" || got.Committed {
			t.Fatalf("tool success or retired protocol inferred publication: %+v", got)
		}
	}
	before := m
	event := (decisionOperation{Op: "decision_commit", Committed: true, Actions: 9}).event()
	event.Type = "replan_" + event.Type
	m.observe(event)
	m.observe(agent.Event{Type: "decision_operation", Text: "bad JSON"})
	if !reflect.DeepEqual(m, before) {
		t.Fatalf("shadow or malformed operation changed decision metrics: %+v", m)
	}
}

func TestDecisionMetricsPreserveCuratorRejection(t *testing.T) {
	job := curationJob(t)
	for _, status := range []string{"success", "failed"} {
		for _, action := range []bool{false, true} {
			metrics := newDecisionMetrics()
			if action {
				metrics.observe(agent.Event{Type: "tool_start", ToolName: "graph_action"})
			}
			result := Result{Status: status, Text: `{"accepted":false,"reason":"No supported curation"}`}
			got := metrics.finish(job, result, time.Time{}, time.Time{})
			want := "unknown"
			if status == "success" && !action {
				want = "rejected"
			}
			if got.Outcome != want || got.Committed {
				t.Fatalf("curation status=%s action=%v: %+v", status, action, got)
			}
		}
	}
}

func TestDecisionMetricsCountCompletionEvidenceOutsideView(t *testing.T) {
	view := json.RawMessage(`{"goals":[],"steps":[]}`)
	assessment := &board.CompletionReview{
		Acceptance:  "not_checked",
		UserInputs:  []board.Fact{{ID: "goal", Description: "Check <all> required cases"}},
		FactRecords: []board.FactRecord{{ID: "f001", Description: "Observed result"}},
	}
	raw, err := json.Marshal(assessment)
	if err != nil {
		t.Fatal(err)
	}
	for _, packet := range []*board.CompletionReview{nil, assessment} {
		j := Job{Kind: "reason", Decision: &board.DecisionContext{
			Version: 2, Mode: "completion", BaselineBytes: 4096, View: view,
			CompletionAssessment: packet,
		}}
		want := len(view)
		if packet != nil {
			want += len(raw)
		}
		got := newDecisionMetrics().finish(j, Result{}, time.Time{}, time.Time{})
		if got.SelectedViewBytes != want || got.BaselineViewBytes != 4096 || got.ViewMode != "completion" {
			t.Fatalf("completion evidence missing from input accounting: got %+v, want %d bytes", got, want)
		}
	}
}

func TestDecisionMetricsReplayJournalReceipts(t *testing.T) {
	dir := t.TempDir()
	want := newDecisionMetrics()
	journal, err := eventlog.Open(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []decisionOperation{
		{Op: "draft", Actions: 3},
		{Op: "decision_preview"},
		{Op: "read_graph", Failed: true, StateChanged: true},
		{Op: "decision_commit", Failed: true, ElapsedMS: 53},
		{Op: "decision_receipt", Committed: true, Actions: 2},
		{Op: "decision_receipt", Committed: true, Actions: 2},
	} {
		event := operation.event()
		if err := journal.Append(event); err != nil {
			t.Fatal(err)
		}
		want.observe(event)
	}
	checkpoint, err := journal.SyncCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	metrics := newDecisionMetrics()
	recovered, err := eventlog.Open(dir, &checkpoint, metrics.observe)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if !reflect.DeepEqual(metrics, want) || !metrics.Committed || metrics.CommittedActions != 2 {
		t.Fatalf("replay changed observations: got %+v; want %+v", metrics, want)
	}
}

func TestDecisionMetricsIgnoreRetiredEventsDuringLiveExecutionAndRecovery(t *testing.T) {
	shadow := []agent.Event{
		{Type: "replan_model_call_start", Request: &agent.RequestObservation{Kind: "turn", InputBytes: 70, InputBasis: "fixture"}},
		{Type: "replan_model_call_end", Request: &agent.RequestObservation{Usage: &agent.Usage{InputTokens: 9}}},
		{Type: "replan_tool_start", ToolName: "read_graph"},
		{Type: "replan_tool_start", ToolName: "graph_action"},
		{Type: "replan_tool_end", ToolName: "graph_action"},
		{Type: "replan_decision_operation", Text: `{"op":"decision_commit","committed":true,"actions":9}`},
	}
	live := newDecisionMetrics()
	for _, event := range shadow {
		live.observe(event)
	}
	if !reflect.DeepEqual(live, newDecisionMetrics()) {
		t.Fatalf("live metrics adapted retired events: %+v", live)
	}
	job, dir := outcomeJob(t, "explore"), t.TempDir()
	runtime, err := restoreSession(job, Options{RunDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range shadow {
		if err := runtime.journal.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	current := agent.Event{Type: "model_call_start", Request: &agent.RequestObservation{Kind: "turn", InputBytes: 25, InputBasis: "fixture"}}
	runtime.emit(current)
	if runtime.metrics.ModelCalls != 1 || runtime.metrics.InitialInputBytes != 25 {
		t.Fatalf("runtime did not own live observations: %+v", runtime.metrics)
	}
	if err := runtime.save(nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.journal.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := restoreSession(job, Options{RunDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	metrics := restored.metrics
	if !reflect.DeepEqual(metrics, runtime.metrics) || metrics.ModelCalls != 1 || metrics.TotalInputBytes != 25 || metrics.Usage.InputTokens != 0 || metrics.GraphReads != 0 || metrics.GraphActions != 0 || metrics.Committed {
		t.Fatalf("recovery interpreted retired events or changed live accounting: %+v", metrics)
	}
	after, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil || string(before) != string(after) {
		t.Fatalf("metrics recovery rewrote the retained journal: %v", err)
	}
	if err := restored.journal.Close(); err != nil {
		t.Fatal(err)
	}
	restored.emit(current)
	if restored.logErr == nil || !reflect.DeepEqual(restored.metrics, metrics) {
		t.Fatal("failed journal append changed live metrics")
	}
}

func TestJournalRejectsInvalidCheckpoints(t *testing.T) {
	for _, mode := range []string{"checksum", "short_log", "record_boundary", "malformed_record", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			journal, err := eventlog.Open(dir, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.Append(agent.Event{Type: "model_call_start", Request: &agent.RequestObservation{Kind: "turn"}}); err != nil {
				t.Fatal(err)
			}
			checkpoint, err := journal.SyncCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			saved, want := &checkpoint, ""
			switch mode {
			case "checksum":
				checkpoint.SHA256 = strings.Repeat("0", 64)
				want = "checksum mismatch"
			case "short_log":
				checkpoint.Offset++
				want = "shorter than its committed checkpoint"
			case "record_boundary":
				checkpoint.Offset--
				want = "not at a record boundary"
			case "malformed_record":
				if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(strings.Repeat("!", int(checkpoint.Offset)-1)+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				want = "invalid complete event record"
			case "unbound":
				saved, want = nil, "without a bound session"
			}
			recovered, err := eventlog.Open(dir, saved, nil)
			if recovered != nil {
				recovered.Close()
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("invalid journal was not rejected: got %v, want %q", err, want)
			}
		})
	}
}

type decisionMetricsOutput func([]byte) (int, error)

func (write decisionMetricsOutput) Write(raw []byte) (int, error) { return write(raw) }

func TestDecisionMetricsRuntimeReceiptCounts(t *testing.T) {
	for _, receipt := range []board.DecisionReceipt{{}, {ChangedActions: 2}, {Results: []board.StateActionResult{{ID: "retained-result"}}}} {
		changed := receipt.ChangedActions
		dir, version := t.TempDir(), strings.Repeat("a", 64)
		m := newDecisionMetrics()
		o := Options{RunDir: dir}
		o.Output = decisionMetricsOutput(func(raw []byte) (int, error) {
			var event GraphRequestEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				return 0, err
			}
			// The current bridge supplies ChangedActions. Retained Results must
			// never override that authoritative count, including an explicit zero.
			receipt.Committed, receipt.StateVersion = true, version
			result, _ := json.Marshal(receipt)
			response, _ := json.Marshal(GraphResponse{RequestID: event.Request.RequestID, Result: result})
			err := os.WriteFile(filepath.Join(dir, "graph-response-"+event.Request.RequestID+".json"), response, 0600)
			return len(raw), err
		})
		j := Job{Kind: "reason", GraphRPC: true, Decision: &board.DecisionContext{Version: 2, StateVersion: version}}
		oRuntime := configureRoleTools(t, j, &o)
		oRuntime.emit = m.observe
		batch := board.DecisionBatch{ExpectedVersion: version, Actions: []board.DecisionAction{}}
		for n := 0; n < changed; n++ {
			payload, _ := json.Marshal(map[string]any{"action": "add", "from": []string{"origin"}, "description": strings.Repeat("Fixture", n+1)})
			batch.Actions = append(batch.Actions, board.DecisionAction{Op: "step", Payload: payload})
		}
		if _, err := oRuntime.decision.request(context.Background(), GraphRequest{Op: "decision_commit", Batch: &batch}); err != nil {
			t.Fatal(err)
		}
		got := m.finish(j, Result{Status: "success"}, time.Time{}, time.Time{})
		wantOutcome := "no_op_committed"
		if changed > 0 {
			wantOutcome = "actions_committed"
		}
		if !got.Committed || got.CommittedActions != changed || got.CommitCalls != 1 || got.Outcome != wantOutcome {
			t.Fatalf("compact receipt lost action count %d: %+v", changed, got)
		}
	}
}

func TestDecisionMetricsRuntimeOperationsAndLostCommit(t *testing.T) {
	dir, version := t.TempDir(), strings.Repeat("a", 64)
	m := newDecisionMetrics()
	conflict, committed := true, false
	o := Options{RunDir: dir}
	o.Output = decisionMetricsOutput(func(raw []byte) (int, error) {
		var event GraphRequestEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return 0, err
		}
		response := GraphResponse{RequestID: event.Request.RequestID}
		switch event.Request.Op {
		case "read_graph":
			if conflict && event.Request.Section == "facts" {
				response.Error, conflict = "state_changed: updated evidence", false
			} else {
				response.Result, _ = json.Marshal(map[string]any{"section": event.Request.Section, "offset": 0, "total": 0, "state_version": version, "items": []any{}})
			}
		case "decision_preview":
			response.Result, _ = json.Marshal(board.DecisionReceipt{StateVersion: version})
		case "decision_commit":
			committed = true
			response.Error = "response lost after publish"
		case "decision_receipt":
			response.Result, _ = json.Marshal(board.DecisionReceipt{Committed: committed, StateVersion: version, ChangedActions: 1})
		default:
			t.Fatalf("unexpected RPC: %s", event.Request.Op)
		}
		body, err := json.Marshal(response)
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, "graph-response-"+event.Request.RequestID+".json"), body, 0600)
		}
		return len(raw), err
	})
	j := Job{Kind: "reason", GraphRPC: true, Decision: &board.DecisionContext{Version: 2, StateVersion: version}}
	oRuntime := configureRoleTools(t, j, &o)
	oRuntime.emit = m.observe
	call := func(name, input string, wantError bool) {
		t.Helper()
		for _, tool := range o.Tools {
			if tool.Name != name {
				continue
			}
			_, err := tool.Execute(context.Background(), json.RawMessage(input))
			if (err != nil) != wantError {
				t.Fatalf("%s %s: error = %v", name, input, err)
			}
			return
		}
		t.Fatalf("tool %s missing", name)
	}
	stage := `{"op":"step","idempotency_key":"first","payload":{"action":"add","from":["origin"],"description":"Inspect evidence"}}`
	call("graph_action", stage, false)
	call("graph_action", stage, false) // An idempotent draft is not a second action.
	call("graph_action", `{"op":"preview","idempotency_key":"preview","payload":{}}`, false)
	if m.DraftCalls != 2 || m.DraftActions != 1 || m.PreviewCalls != 1 || m.CommitCalls != 0 || m.Committed {
		t.Fatalf("staged work was confused with published work: %+v", m)
	}
	call("read_graph", `{"section":"facts"}`, true)
	call("read_graph", `{"section":"overview"}`, false)
	call("read_graph", `{"section":"facts"}`, false)
	oRuntime.decision.beforeRequest(&agent.Loop{})
	call("graph_action", stage, false)
	call("graph_action", `{"op":"commit","idempotency_key":"commit","payload":{}}`, true)
	if m.Committed || m.StateChanged != 1 || m.CommitCalls != 1 || m.CommitFailures != 1 {
		t.Fatalf("lost commit response cannot prove publication: %+v", m)
	}
	if _, err := oRuntime.decision.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.Committed || m.CommittedActions != 1 || m.ReceiptCalls != 1 || m.DraftActions != 2 {
		t.Fatalf("receipt recovery or unchanged-action counting: %+v", m)
	}
}

func TestExecutePersistsRequestObservationsWithoutDoubleCountingUsage(t *testing.T) {
	j, runDir := outcomeJob(t, "explore"), t.TempDir()
	turns, toolCalls := 0, 0
	provider := scenarioProvider(func(_ context.Context, _ []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		turns++
		m := agent.Text("assistant", completedOutput)
		if turns == 1 {
			m = progressCall(1)
		}
		m.Usage = &agent.Usage{InputTokens: 19, OutputTokens: 7, CacheReadTokens: 3}
		return m, nil
	})
	result, err := runTestWorker(context.Background(), j, Options{RunDir: runDir, Provider: provider, Tools: []agent.Tool{progressTool(&toolCalls, false)}})
	if err != nil || result.Status != "success" || turns != 2 || toolCalls != 1 {
		t.Fatalf("execute failed: result=%+v err=%v turns=%d tools=%d", result, err, turns, toolCalls)
	}
	raw, err := os.ReadFile(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	metrics := newDecisionMetrics()
	requestStarts, requestEnds, messages := 0, 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e agent.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		metrics.observe(e)
		switch e.Type {
		case "model_call_start", "model_call_end":
			if e.At == "" || e.Request == nil || e.Request.Kind != "turn" {
				t.Fatalf("missing Execute request observation: %+v", e)
			}
			if e.Type == "model_call_start" {
				requestStarts++
			} else {
				requestEnds++
			}
		case "message_end":
			if e.Message != nil && e.Message.Role == "assistant" && e.Message.Usage != nil {
				messages++
			}
		}
	}
	wantUsage := agent.Usage{InputTokens: 38, OutputTokens: 14, CacheReadTokens: 6}
	if requestStarts != 2 || requestEnds != 2 || messages != 2 || metrics.UsageCalls != 2 || metrics.Usage != wantUsage || metrics.UsageStatus != "reported_only" {
		t.Fatalf("Execute usage duplicated or missing: starts=%d ends=%d messages=%d metrics=%+v", requestStarts, requestEnds, messages, metrics)
	}
}
