package model

type TaskBudget struct {
	Timeout         int `yaml:"timeout" json:"timeout"`
	ConcludeTimeout int `yaml:"conclude_timeout" json:"conclude_timeout"`
	MaxIntents      int `yaml:"max_intents" json:"max_intents"`
	// Empty inherits the backend's PWNMESH_REASONING_EFFORT. An explicit role
	// policy is persisted in the Job, so same-run recovery keeps that choice.
	ReasoningEffort string `yaml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty"`
}

// DependencyResult freezes the accepted upstream result used by one run.
// RunID is the registered execution ID, not a model-supplied evidence identity.
type DependencyResult struct {
	StepID string `json:"step_id"`
	FactID string `json:"fact_id"`
	RunID  string `json:"run_id"`
}

// InputSnapshot identifies an immutable input, not a promise to reconstruct a
// historical revision from the current graph. Its state remains server-side.
type InputSnapshot struct {
	Version          int    `json:"version"`
	ID               string `json:"id"`
	ProjectID        string `json:"project_id"`
	Generation       int64  `json:"generation"`
	Revision         int64  `json:"revision"`
	DecisionRevision int64  `json:"decision_revision"`
	StateVersion     string `json:"state_version"`
	FactCount        int    `json:"fact_count"`
	HintCount        int    `json:"hint_count"`
	OpenCount        int    `json:"open_count"`
	StepCount        int    `json:"step_count"`
}
