package board

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
)

// CompleteCurationExecution settles the accepted batch in the same transaction
// as its receipt. It also closes pre-upgrade pending receipts without rerunning
// the model or changing a retained result. Stopped/revoked runs cannot recover.
func (t *Tx) CompleteCurationExecution(project, run string) error {
	e, err := t.executionForLease(project, run)
	if err != nil {
		return err
	}
	if e.Kind != "curate" || e.Intent != "" {
		return Err(403, "curation completion requires its registered curator execution")
	}
	receipt, err := t.CurationReceipt(project, run)
	if err != nil {
		return err
	}
	if !receipt.Committed || receipt.Op != "curate" {
		return Err(409, "Curator has no committed curation receipt")
	}
	if e.Status == "succeeded" {
		return nil
	}
	if !e.Pending() {
		return Err(409, "Execution is terminal")
	}
	g, err := t.Load(project)
	if err != nil {
		return err
	}
	if err = g.RequireActive(); err != nil {
		return err
	}
	var job struct {
		Graph Graph `json:"graph"`
	}
	if json.Unmarshal(e.Job, &job) != nil || job.Graph.Project.Generation != g.Project.Generation {
		return Err(409, "Execution input belongs to a previous project round")
	}
	if revoked, err := t.RunRevoked(project, run); err != nil {
		return err
	} else if revoked {
		return Err(409, "Execution was revoked")
	}
	if g.Project.Curator != nil && g.Project.Curator.Worker != run {
		return Err(409, "Another execution owns curation")
	}
	result := e.Result
	if e.Status != "result_pending" {
		result = json.RawMessage(`{"status":"success","type":"result","text":"{\"accepted\":true,\"data\":{\"curated\":true}}"}`)
		if err = t.ExecutionStatus(e, "result_pending", result); err != nil {
			return err
		}
	}
	if err = t.ExecutionStatus(e, "succeeded", result); err != nil {
		return err
	}
	return t.ReleaseCurator(project, run)
}

// Completed submissions may replay their exact receipt after their lease was
// revoked. This never grants authority for a new action or changed payload.
func (t *Tx) replayCuration(project string, fence ExecutionFence, action StateAction) (StateActionResult, bool, error) {
	if fence.Run == "" || fence.Lease != "curate" || fence.Intent != "" {
		return StateActionResult{}, false, nil
	}
	e, err := t.executionForLease(project, fence.Run)
	if err != nil || e.Kind != "curate" || e.Status != "succeeded" {
		return StateActionResult{}, false, err
	}
	canonical, err := canonicalStateAction(fence, action)
	if err != nil {
		return StateActionResult{}, false, err
	}
	var request, response string
	err = t.QueryRow("SELECT request,response FROM xloom_state_actions WHERE project_id=? AND idempotency_key=?", project, action.IdempotencyKey).Scan(&request, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return StateActionResult{}, false, nil
	}
	if err != nil {
		return StateActionResult{}, false, err
	}
	if request != string(canonical) {
		return StateActionResult{}, false, Err(409, "idempotency key was used for a different action")
	}
	var receipt StateActionResult
	err = json.Unmarshal([]byte(response), &receipt)
	return receipt, true, err
}

// Preserve exact values and execution identity while ignoring JSON object order.
func canonicalStateAction(fence ExecutionFence, action StateAction) ([]byte, error) {
	var payload any
	decoder := json.NewDecoder(bytes.NewReader(action.Payload))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, Err(422, "invalid action JSON")
	}
	return json.Marshal(struct {
		Run, Op string
		Payload any
	}{fence.Run, action.Op, payload})
}
