package protocol

import "pwnmesh/internal/agent"

// DecisionMetrics reports observed work, not a bill or proof that proposed
// changes were applied. The execution receipt and graph events remain the
// authority for committed business actions.
type DecisionMetrics struct {
	Version              int         `json:"version"`
	ModelCalls           int         `json:"model_calls"`
	SummaryCalls         int         `json:"summary_calls"`
	CompletedCalls       int         `json:"completed_calls"`
	FailedCalls          int         `json:"failed_calls"`
	UsageCalls           int         `json:"usage_calls"`
	Usage                agent.Usage `json:"usage"`
	UsageStatus          string      `json:"usage_status"`
	CostStatus           string      `json:"cost_status"`
	InitialInputBytes    int         `json:"initial_input_bytes"`
	TotalInputBytes      int64       `json:"total_input_bytes"`
	InputBytesBasis      string      `json:"input_bytes_basis,omitempty"`
	ModelDurationMS      int64       `json:"model_duration_ms"`
	ElapsedWallMS        int64       `json:"elapsed_wall_ms"`
	GraphReads           int         `json:"graph_reads"`
	GraphReadFailures    int         `json:"graph_read_failures"`
	GraphActions         int         `json:"graph_actions"`
	GraphActionSuccesses int         `json:"graph_action_successes"`
	GraphActionFailures  int         `json:"graph_action_failures"`
	DraftCalls           int         `json:"draft_calls,omitempty"`
	DraftFailures        int         `json:"draft_failures,omitempty"`
	DraftActions         int         `json:"draft_actions,omitempty"`
	PreviewCalls         int         `json:"preview_calls,omitempty"`
	PreviewFailures      int         `json:"preview_failures,omitempty"`
	CommitCalls          int         `json:"commit_calls,omitempty"`
	CommitFailures       int         `json:"commit_failures,omitempty"`
	CommitDurationMS     int64       `json:"commit_duration_ms,omitempty"`
	ReceiptCalls         int         `json:"receipt_calls,omitempty"`
	ReceiptFailures      int         `json:"receipt_failures,omitempty"`
	StateChanged         int         `json:"state_changed,omitempty"`
	Committed            bool        `json:"committed,omitempty"`
	CommittedActions     int         `json:"committed_actions,omitempty"`
	Outcome              string      `json:"outcome"`
	Trigger              string      `json:"trigger,omitempty"`
	Repeated             bool        `json:"repeated"`
	ViewMode             string      `json:"view_mode,omitempty"`
	BaselineViewBytes    int         `json:"baseline_view_bytes,omitempty"`
	SelectedViewBytes    int         `json:"selected_view_bytes,omitempty"`
}
