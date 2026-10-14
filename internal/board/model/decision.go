package model

import "encoding/json"

type DecisionAction struct {
	Op      string          `json:"op"`
	Ref     string          `json:"ref,omitempty"`
	GapID   string          `json:"gap_id,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

type DecisionBatch struct {
	ExpectedVersion string           `json:"expected_version"`
	Actions         []DecisionAction `json:"actions"`
	Assessment      *RootAssessment  `json:"assessment,omitempty"`
}

type DecisionReceipt struct {
	Committed        bool                `json:"committed"`
	StateVersion     string              `json:"state_version,omitempty"`
	Results          []StateActionResult `json:"results"`
	ChangedActions   int                 `json:"changed_actions,omitempty"`
	IDs              map[string]string   `json:"ids"`
	Completed        bool                `json:"completed"`
	ValidationScope  string              `json:"validation_scope,omitempty"`
	CompletionReview *CompletionReview   `json:"completion_review,omitempty"`
}
