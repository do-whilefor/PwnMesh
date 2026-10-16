package model

const MaxExecuteUpdateBytes = 16 << 10
const MaxExecuteUpdateEvents = 1000

type ExecuteUpdateCursor struct {
	ProjectID  string `json:"project_id"`
	Generation int64  `json:"generation"`
	StepID     string `json:"step_id"`
	RunID      string `json:"run_id"`
	Revision   int64  `json:"revision"`
}

type ExecuteUpdates struct {
	Version int `json:"version"`
	ExecuteUpdateCursor
	FromRevision int64  `json:"from_revision"`
	ToRevision   int64  `json:"to_revision"`
	StateVersion string `json:"state_version"`
	Complete     bool   `json:"complete"`
	// Rebased replaces an over-limit event scan with the current complete
	// dependency closure. Complete remains false whenever that closure is omitted.
	Rebased        bool           `json:"rebased,omitempty"`
	PendingReason  string         `json:"pending_reason,omitempty"`
	InvalidSources []string       `json:"invalid_sources"`
	Facts          []FactRecord   `json:"facts"`
	Relations      []FactRelation `json:"relations"`
	Omitted        map[string]int `json:"omitted"`
	ReadMore       string         `json:"read_more,omitempty"`
}
