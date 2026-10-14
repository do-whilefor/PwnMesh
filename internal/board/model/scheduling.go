package model

type ScheduleStep struct {
	ID             string   `json:"id"`
	Status         string   `json:"status"`
	Priority       int      `json:"priority"`
	CreatedAt      string   `json:"created_at"`
	Ready          bool     `json:"ready"`
	Running        bool     `json:"running,omitempty"`
	InvalidSources []string `json:"invalid_sources,omitempty"`
	BlockedBy      []string `json:"blocked_by,omitempty"`
	WritePaths     []string `json:"-"` // Server-side admission only; checks carry the result.
}

const ScheduleProtocolVersion = 1

type SchedulePage struct {
	ProtocolVersion   int                       `json:"protocol_version"`
	Project           Project                   `json:"project"`
	FactCount         int                       `json:"fact_count"`
	HintCount         int                       `json:"hint_count"`
	OpenCount         int                       `json:"open_count"`
	Initial           bool                      `json:"initial"`
	Revision          int64                     `json:"revision"`
	DecisionRevision  int64                     `json:"decision_revision"`
	StateVersion      string                    `json:"state_version"`
	RetryKey          string                    `json:"retry_key"`
	Steps             []ScheduleStep            `json:"steps"`
	NextOffset        int                       `json:"next_offset,omitempty"`
	ExecutionChecks   map[string]ExecutionCheck `json:"execution_checks,omitempty"`
	CurationNeeded    bool                      `json:"curation_needed,omitempty"`
	CurationRequested bool                      `json:"curation_requested,omitempty"`
	CurationRetryKey  string                    `json:"curation_retry_key,omitempty"`
}
