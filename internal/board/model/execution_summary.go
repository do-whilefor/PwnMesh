package model

type ExecutionSummary struct {
	ProjectID        string `json:"project_id"`
	ID               string `json:"id"`
	Namespace        string `json:"namespace"`
	Backend          string `json:"backend"`
	Kind             string `json:"kind"`
	Intent           string `json:"intent"`
	Lease            string `json:"lease"`
	RetryKey         string `json:"retry_key"`
	Status           string `json:"status"`
	Resumes          int    `json:"resumes"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	Generation       int64  `json:"generation"`
	InputRevision    int64  `json:"input_revision"`
	DecisionRevision int64  `json:"decision_revision"`
	StateVersion     string `json:"state_version"`
	HasState         bool   `json:"has_state"`
	FactCount        int    `json:"fact_count"`
	HintCount        int    `json:"hint_count"`
	OpenCount        int    `json:"open_count"`
}

type ExecutionPage struct {
	Items      []ExecutionSummary `json:"items"`
	NextCursor int64              `json:"next_cursor,omitempty"`
}

type ExecutionCheck struct {
	Pending          bool              `json:"pending"`
	Blocked          bool              `json:"blocked"`
	PreviousRunID    string            `json:"previous_run_id"`
	Attempts         int               `json:"attempts"`
	AutomaticRetryID string            `json:"automatic_retry_id"`
	Repeated         bool              `json:"repeated"`
	LatestDecision   *ExecutionSummary `json:"latest_decision,omitempty"`
}

func (e ExecutionSummary) Pending() bool {
	return e.Status == "prepared" || e.Status == "running" || e.Status == "retryable" || e.Status == "result_pending"
}
