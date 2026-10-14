package worker

import (
	"encoding/json"
	"time"

	"pwnmesh/internal/agent"
)

// decisionMetrics owns runtime accumulation; the embedded data is the wire report.
type decisionMetrics struct{ DecisionMetrics }

// decisionOperation is emitted by the runtime, never inferred from a successful
// graph_action tool call: a version 2 action may only have staged a local draft.
// These events are journaled so recovery retains observed attempts and receipts.
type decisionOperation struct {
	Op           string `json:"op"`
	ElapsedMS    int64  `json:"elapsed_ms"`
	Failed       bool   `json:"failed"`
	StateChanged bool   `json:"state_changed"`
	Committed    bool   `json:"committed"`
	Actions      int    `json:"actions"`
}

func (operation decisionOperation) event() agent.Event {
	raw, _ := json.Marshal(operation)
	return agent.Event{Type: "decision_operation", Text: string(raw)}
}

func newDecisionMetrics() decisionMetrics {
	return decisionMetrics{DecisionMetrics{Version: 1, UsageStatus: "unknown", CostStatus: "unknown", Outcome: "unknown"}}
}

func (m *decisionMetrics) observe(event agent.Event) {
	switch event.Type {
	case "decision_operation":
		var operation decisionOperation
		if json.Unmarshal([]byte(event.Text), &operation) != nil {
			return
		}
		m.observeDecisionOperation(operation)
	case "model_call_start":
		if event.Request == nil {
			return
		}
		m.ModelCalls++
		if event.Request.Kind == "summary" {
			m.SummaryCalls++
		}
		if m.InitialInputBytes == 0 && event.Request.Kind == "turn" {
			m.InitialInputBytes = event.Request.InputBytes
		}
		m.TotalInputBytes += int64(event.Request.InputBytes)
		if m.InputBytesBasis == "" {
			m.InputBytesBasis = event.Request.InputBasis
		} else if m.InputBytesBasis != event.Request.InputBasis {
			m.InputBytesBasis = "mixed"
		}
	case "model_call_end":
		if event.Request == nil {
			return
		}
		m.CompletedCalls++
		m.ModelDurationMS += event.Request.DurationMS
		if event.Request.Failed {
			m.FailedCalls++
		}
		if usage := event.Request.Usage; usage != nil {
			m.UsageCalls++
			m.Usage.InputTokens += usage.InputTokens
			m.Usage.OutputTokens += usage.OutputTokens
			m.Usage.CacheReadTokens += usage.CacheReadTokens
			m.Usage.CacheWriteTokens += usage.CacheWriteTokens
		}
	case "tool_start":
		if event.ToolName == "read_graph" || event.ToolName == "read_evidence" {
			m.GraphReads++
		} else if event.ToolName == "graph_action" {
			m.GraphActions++
		}
	case "tool_end":
		if (event.ToolName == "read_graph" || event.ToolName == "read_evidence") && event.Error != "" {
			m.GraphReadFailures++
		} else if event.ToolName == "graph_action" {
			if event.Error == "" {
				m.GraphActionSuccesses++
			} else {
				m.GraphActionFailures++
			}
		}
	}
	// Reported usage can exclude provider-internal HTTP retries even when every
	// logical call supplied usage. Never describe these totals as a full bill.
	if m.UsageCalls == 0 {
		m.UsageStatus = "unknown"
	} else if m.UsageCalls < m.ModelCalls {
		m.UsageStatus = "partial"
	} else {
		m.UsageStatus = "reported_only"
	}
}

func (m *decisionMetrics) observeDecisionOperation(operation decisionOperation) {
	failure := 0
	if operation.Failed {
		failure = 1
	}
	switch operation.Op {
	case "draft":
		m.DraftCalls++
		m.DraftFailures += failure
		if !operation.Failed {
			m.DraftActions += max(0, operation.Actions)
		}
	case "decision_preview":
		m.PreviewCalls++
		m.PreviewFailures += failure
	case "decision_commit":
		m.CommitCalls++
		m.CommitFailures += failure
		m.CommitDurationMS += max(0, operation.ElapsedMS)
	case "decision_receipt":
		m.ReceiptCalls++
		m.ReceiptFailures += failure
	case "read_graph":
	default:
		return
	}
	if operation.StateChanged {
		m.StateChanged++
	}
	if !operation.Failed && operation.Committed && (operation.Op == "decision_commit" || operation.Op == "decision_receipt") {
		m.Committed = true
		// A run has one authoritative batch. Receipt retries confirm that same
		// commit; counting their actions again would inflate published work.
		m.CommittedActions = max(m.CommittedActions, operation.Actions)
	}
}

func (m decisionMetrics) finish(j Job, r Result, started, ended time.Time) DecisionMetrics {
	m.ElapsedWallMS = max(0, ended.Sub(started).Milliseconds())
	m.Trigger, m.Repeated = j.DecisionTrigger, j.DecisionRepeated
	if j.Decision != nil {
		m.ViewMode, m.BaselineViewBytes, m.SelectedViewBytes = j.Decision.Mode, j.Decision.BaselineBytes, len(j.Decision.View)
		// Completion mode moves evidence out of View; it is still model input.
		// Count both bodies so the projection cannot claim savings by relocation.
		if j.Decision.CompletionAssessment != nil {
			assessment, _ := json.Marshal(j.Decision.CompletionAssessment)
			m.SelectedViewBytes += len(assessment)
		}
	}
	if j.Kind == "reason" && j.Decision != nil && j.Decision.Version == 2 {
		if m.Committed {
			m.Outcome = "no_op_committed"
			if m.CommittedActions > 0 {
				m.Outcome = "actions_committed"
			}
		} else if m.DraftActions > 0 {
			m.Outcome = "draft_only_observed"
		}
		return m.DecisionMetrics
	}
	if j.Kind != "curate" || r.Status != "success" || m.GraphActions > 0 {
		return m.DecisionMetrics // An interrupted/failed action may have reached the server.
	}
	parsed, err := parseOutput(j, r.Conclude, r.Text)
	if err == nil && parsed.Kind == "rejected" {
		m.Outcome = "rejected"
	}
	return m.DecisionMetrics
}
