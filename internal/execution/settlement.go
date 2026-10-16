package execution

import (
	"encoding/json"
	"errors"
	b "pwnmesh/internal/board"
	"pwnmesh/internal/contract"
)

// Apply settles a registered result without replaying Worker effects.
func Apply(t *b.Tx, project, run string, fence b.ExecutionFence) (string, error) {
	e, err := leasedExecution(t, project, run, fence)
	if err != nil {
		return "", err
	}
	return settle(t, e)
}

// Final business mutations and their receipt commit in the same SQLite
// transaction. Retrying after a lost HTTP response cannot append duplicates.
func settle(t *b.Tx, e b.Execution) (string, error) {
	if e.Status == "succeeded" || e.Status == "rejected" {
		return e.Status, nil
	}
	if e.Kind == "curate" {
		// Pre-upgrade curators could commit a batch without settling their Run.
		// Recovery is an explicit write; receipt GETs remain read-only.
		receipt, err := t.CurationReceipt(e.ProjectID, e.Lease)
		var api *b.APIError
		if err != nil && (!errors.As(err, &api) || api.Status != 404) {
			return "", err
		}
		if receipt.Committed {
			if err = t.CompleteCurationExecution(e.ProjectID, e.Lease); err != nil {
				return "", err
			}
			return "succeeded", nil
		}
	}
	if e.Status != "result_pending" {
		return "", b.Err(409, "Execution has no pending result")
	}
	if err := validateRepairResult(e.Job, e.Result); err != nil {
		return "", err
	}
	g, err := t.Load(e.ProjectID)
	if err != nil {
		return "", err
	}
	if err = g.RequireActive(); err != nil {
		return "", err
	}
	if err = t.CheckExecution(g, e.Fence()); err != nil {
		return "", err
	}
	var result struct {
		Text         string `json:"text"`
		Conclude     bool   `json:"conclude"`
		StateVersion string `json:"state_version"`
	}
	if err = json.Unmarshal(e.Result, &result); err != nil {
		return "", err
	}
	if e.Kind == "reason" {
		initialVersion, err := b.DecisionJobVersion(e.Job)
		if err != nil {
			return "", err
		}
		if initialVersion != "" || result.StateVersion != "" {
			state, err := t.State(e.ProjectID)
			if err != nil {
				return "", err
			}
			if result.StateVersion == "" || result.StateVersion != b.DecisionStateVersion(state) {
				return "", b.CodedErr(409, b.CodeStateChanged, "state_changed: decision result does not match current shared state")
			}
		}
	}
	// The only executable protocol is fixed by registration, never by apply input.
	parsed, err := contract.ParseWithPolicy(result.Text, e.Kind, result.Conclude, contract.Policy{Version: 2, GraphRPC: true})
	if err != nil {
		return "", b.Err(422, err.Error())
	}
	if parsed.Outcome == "continue" || parsed.Outcome == "incomplete" {
		return "", b.Err(422, "Continuing or incomplete worker output cannot be applied as a successful result")
	}
	if e.Kind == "reason" && parsed.Kind != "rejected" {
		// Only a refusal can finish without publishing a plan. A successful
		// batch already wrote its receipt; final JSON cannot replace that commit.
		return "", b.Err(409, "Decide version 2 requires a committed decision batch")
	}
	status := "succeeded"
	if parsed.Kind == "rejected" {
		status = "rejected"
	} else if e.Kind == "curate" {
		// Committed receipts were settled above in this same transaction.
		return "", b.Err(409, "Curator has no committed curation receipt")
	} else {
		if _, err = t.ConcludeEvidenceStep(e.ProjectID, e.Fence(), parsed.FactID, parsed.FactPayload); err != nil {
			return "", err
		}
	}
	if err = t.ExecutionStatus(e, status, e.Result); err != nil {
		return "", err
	}
	if e.Kind == "curate" {
		if err = t.ReleaseCurator(e.ProjectID, e.Lease); err != nil {
			return "", err
		}
	}
	return status, nil
}
