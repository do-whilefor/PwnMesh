package model

import "encoding/json"

type Execution struct {
	ProjectID string          `json:"project_id"`
	ID        string          `json:"id"`
	Namespace string          `json:"namespace"`
	Backend   string          `json:"backend"`
	Kind      string          `json:"kind"`
	Intent    string          `json:"intent"`
	Lease     string          `json:"lease"`
	Job       json.RawMessage `json:"job"`
	RetryKey  string          `json:"retry_key"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Resumes   int             `json:"resumes"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

func (e Execution) Fence() ExecutionFence {
	return ExecutionFence{Run: e.Lease, Lease: e.Kind, Intent: e.Intent}
}

func (e Execution) Pending() bool {
	return e.Status == "prepared" || e.Status == "running" || e.Status == "retryable" || e.Status == "result_pending"
}
