package board

import (
	"encoding/json"
	"strings"

	"pwnmesh/internal/board/internal/sqlstore"
)

func (t *Tx) recordExecutionFailure(e Execution, status string, failure json.RawMessage) error {
	d, _, _, err := t.stateData(e.ProjectID)
	if err != nil {
		return err
	}
	triggerDecision := true
	for _, step := range d.Steps {
		if step.ID == e.Intent && step.Status == "abandoned" {
			// This terminal event merely acknowledges Decide's own explicit
			// cancellation. It must not trigger another decision about itself.
			triggerDecision = false
		}
	}
	revision, err := t.advanceStateRevision(e.ProjectID, triggerDecision)
	if err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{"run_id": e.ID, "status": status, "reason": executionFailureDescription(status, failure), "result": failure})
	event, _ := json.Marshal(StateEvent{Revision: revision, Op: "execution_failed", ID: e.Intent, RunID: e.Lease, CreatedAt: t.Now, Payload: detail, Result: detail})
	return sqlstore.InsertStateEvent(t.transactionQueries, e.ProjectID, revision, string(event))
}

// Event-only changes preserve metadata byte-for-byte, including unknown fields
// retained by older databases. The surrounding transaction owns the event too.
func (t *Tx) advanceStateRevision(project string, decisionChange bool) (int64, error) {
	increment := 0
	if decisionChange {
		increment = 1
	}
	return sqlstore.AdvanceStateRevision(t.transactionQueries, project, `{"goals":[],"steps":[],"facts":[],"findings":[],"fact_relations":[]}`, increment)
}

func (t *Tx) StateEvents(project string, after int64) ([]StateEvent, error) {
	if err := t.RequireProject(project); err != nil {
		return nil, err
	}
	out := []StateEvent{}
	err := sqlstore.VisitStateEvents(t.transactionQueries, project, &after, 1000, func(_ int64, raw []byte) error {
		var event StateEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return err
		}
		out = append(out, event)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func executionFailureDescription(status string, raw json.RawMessage) string {
	var result struct {
		Error       string `json:"error"`
		FailureKind string `json:"failure_kind"`
		Text        string `json:"text"`
	}
	_ = json.Unmarshal(raw, &result)
	description := strings.TrimSpace(result.Error)
	if description == "" && status == "rejected" {
		var declined struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal([]byte(result.Text), &declined)
		description = strings.TrimSpace(declined.Reason)
	}
	if result.FailureKind != "" {
		description = result.FailureKind + ": " + description
	}
	if description == "" {
		description = "execution " + status
	}
	if len(description) > 2048 {
		description = strings.ToValidUTF8(description[:2048], "�") + "…"
	}
	return description
}
