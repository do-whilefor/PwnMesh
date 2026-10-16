// Package execution composes preparation and settlement inside a caller-owned
// Board transaction. It has no HTTP, Worker, or transaction-finalization access.
package execution

import (
	"encoding/json"
	b "pwnmesh/internal/board"
	"pwnmesh/internal/board/model"
)

// Execution inputs always use the live evidence protocol.
func validateExecutionProtocol(graphRPC, resultVersion json.RawMessage) error {
	var enabled bool
	if json.Unmarshal(graphRPC, &enabled) != nil || !enabled {
		return b.Err(422, "graph_rpc must be true")
	}
	var version int
	if json.Unmarshal(resultVersion, &version) != nil || version != 2 {
		return b.Err(422, "result_contract_version must be 2")
	}
	return nil
}

func requireCurrentExecution(e b.Execution) error {
	var job struct {
		Kind                  string `json:"kind"`
		GraphRPC              bool   `json:"graph_rpc"`
		ResultContractVersion int    `json:"result_contract_version"`
		Graph                 struct {
			Project struct {
				OrchestrationVersion int `json:"orchestration_version"`
			} `json:"project"`
		} `json:"graph"`
		Decision *struct {
			Version int `json:"version"`
		} `json:"decision"`
	}
	if json.Unmarshal(e.Job, &job) != nil || job.Graph.Project.OrchestrationVersion != 1 || !job.GraphRPC || job.ResultContractVersion != 2 || job.Kind != e.Kind || (e.Kind != "reason" && e.Kind != "curate" && e.Kind != "explore") || (e.Kind == "reason" && (job.Decision == nil || job.Decision.Version != 2)) {
		return b.Err(409, "Unsupported execution protocol; create a new project to continue")
	}
	return nil
}

// Production registrations receive the job built by server-side preparation.
func Register(t *b.Tx, e b.Execution, fence b.ExecutionFence) (b.Execution, error) {
	var job struct {
		RunID                 string           `json:"run_id"`
		Kind                  string           `json:"kind"`
		Graph                 b.Graph          `json:"graph"`
		Intent                *b.Intent        `json:"intent"`
		Workspace             string           `json:"workspace"`
		GraphRPC              json.RawMessage  `json:"graph_rpc"`
		ResultContractVersion json.RawMessage  `json:"result_contract_version"`
		Budget                model.TaskBudget `json:"budget"`
	}
	if json.Unmarshal(e.Job, &job) != nil || e.ID == "" || e.Namespace == "" || e.Backend == "" || e.Lease == "" || e.RetryKey == "" || job.RunID != e.ID || job.Kind != e.Kind || job.Graph.Project.ID != e.ProjectID || job.Workspace == "" {
		return b.Execution{}, b.Err(422, "Invalid execution identity")
	}
	if err := validateExecutionProtocol(job.GraphRPC, job.ResultContractVersion); err != nil {
		return b.Execution{}, err
	}
	if err := job.Budget.ValidateTimeouts(); err != nil {
		return b.Execution{}, b.Err(422, err.Error())
	}
	if !b.ValidExecutionID(e.ID) || len(e.Namespace) > 128 || len(e.Backend) > 256 || len(e.RetryKey) > 1024 || e.Lease != e.Backend+"@"+e.ID {
		return b.Execution{}, b.Err(422, "Invalid execution identity")
	}
	if e.Kind != "reason" && e.Kind != "curate" && e.Kind != "explore" {
		return b.Execution{}, b.Err(422, "Invalid execution kind")
	}
	control := e.Kind == "reason" || e.Kind == "curate"
	if (control && (e.Intent != "" || job.Intent != nil)) || (!control && (job.Intent == nil || job.Intent.ID != e.Intent)) {
		return b.Execution{}, b.Err(422, "Invalid execution step")
	}
	if fence != e.Fence() {
		return b.Execution{}, b.Err(403, "Execution registration requires its lease")
	}
	if err := t.RegisterExecution(e); err != nil {
		return b.Execution{}, err
	}
	saved, err := t.Execution(e.ProjectID, e.ID)
	return saved, err
}
