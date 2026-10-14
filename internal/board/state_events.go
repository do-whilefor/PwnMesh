package board

import "encoding/json"

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
	_, err = t.Exec("INSERT INTO xloom_state_events(project_id,revision,event) VALUES(?,?,?)", e.ProjectID, revision, string(event))
	return err
}

// Event-only changes preserve metadata byte-for-byte, including unknown fields
// retained by older databases. The surrounding transaction owns the event too.
func (t *Tx) advanceStateRevision(project string, decisionChange bool) (int64, error) {
	increment := 0
	if decisionChange {
		increment = 1
	}
	var revision int64
	err := t.QueryRow(`INSERT INTO xloom_state(project_id,data,revision,decision_revision)
		VALUES(?,'{"goals":[],"steps":[],"facts":[],"findings":[],"fact_relations":[]}',1,?)
		ON CONFLICT(project_id) DO UPDATE SET revision=revision+1,decision_revision=decision_revision+excluded.decision_revision
		RETURNING revision`, project, increment).Scan(&revision)
	return revision, err
}

func (t *Tx) StateEvents(project string, after int64) ([]StateEvent, error) {
	if err := t.RequireProject(project); err != nil {
		return nil, err
	}
	rows, err := t.Query("SELECT event FROM xloom_state_events WHERE project_id=? AND revision>? ORDER BY revision LIMIT 1000", project, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StateEvent{}
	for rows.Next() {
		var raw string
		var event StateEvent
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}
