package model

import "encoding/json"

type TraceRun struct {
	ProjectID  string `json:"project_id"`
	Generation int64  `json:"generation"`
	RunID      string `json:"run_id"`
	StepID     string `json:"step_id"`
	Status     string `json:"status"`
	Workspace  string `json:"workspace"`
	JobSHA256  string `json:"job_sha256"`
	// Server-side only: Worker owns Job's canonical serialization. It binds
	// JobSHA256 before delivering this descriptor through the trace bridge.
	RegisteredJob json.RawMessage `json:"-"`
}
