// Package protocol defines the shared execution wire contract.
package protocol

import (
	"encoding/json"
	"errors"
	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/board/model"
)

// ErrInterrupted marks a dispatcher shutdown/infrastructure interruption that
// may resume the same run. Ordinary cancellation remains a permanent stop.
var ErrInterrupted = errors.New("execution interrupted; same-run recovery permitted")

type Job struct {
	InputFiles            []model.InputFile        `json:"input_files,omitempty"`
	Repair                *artifactcheck.Spec      `json:"repair,omitempty"`
	RunID                 string                   `json:"run_id"`
	PreviousRunID         string                   `json:"previous_run_id,omitempty"`
	GraphRPC              bool                     `json:"graph_rpc,omitempty"`
	ResultContractVersion int                      `json:"result_contract_version,omitempty"`
	EnvironmentID         string                   `json:"environment_id,omitempty"`
	DecisionRevision      int64                    `json:"decision_revision,omitempty"`
	Decision              *model.DecisionContext   `json:"decision,omitempty"`
	DecisionTrigger       string                   `json:"decision_trigger,omitempty"`
	DecisionTriggers      []string                 `json:"decision_triggers,omitempty"`
	DecisionRepeated      bool                     `json:"decision_repeated,omitempty"`
	Kind                  string                   `json:"kind"`
	WorkerType            string                   `json:"worker_type"`
	Graph                 model.Graph              `json:"graph"`
	State                 *model.State             `json:"state,omitempty"`
	InputSnapshot         *model.InputSnapshot     `json:"input_snapshot,omitempty"`
	InputView             json.RawMessage          `json:"input_view,omitempty"`
	PreparationKey        string                   `json:"preparation_key,omitempty"`
	Intent                *model.Intent            `json:"intent,omitempty"`
	DependencyResults     []model.DependencyResult `json:"dependency_results,omitempty"`
	Budget                model.TaskBudget         `json:"budget"`
	Workspace             string                   `json:"workspace"`
}

type Result struct {
	RepairCheck  *RepairCheck     `json:"repair_check,omitempty"`
	Type         string           `json:"type"`
	Text         string           `json:"text"`
	Conclude     bool             `json:"conclude"`
	Status       string           `json:"status"`
	Error        string           `json:"error,omitempty"`
	Retryable    bool             `json:"retryable,omitempty"`
	FailureKind  string           `json:"failure_kind,omitempty"`
	FailureCause string           `json:"failure_cause,omitempty"`
	StateVersion string           `json:"state_version,omitempty"`
	Metrics      *DecisionMetrics `json:"metrics,omitempty"`
}

// RepairCheck is a runtime observation, never a model-authored permission.
type RepairCheck struct {
	artifactcheck.Result
	BlankContent   bool   `json:"blank_content,omitempty"`
	Path           string `json:"path"`
	ExpectedSHA256 string `json:"expected_sha256"`
	Outcome        string `json:"outcome"`
}
