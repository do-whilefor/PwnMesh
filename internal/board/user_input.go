package board

import "encoding/json"

// SaveUserInput persists human hints and reopen feedback with their ordered
// change event. Graph commands own their events and use Save directly.
func (t *Tx) SaveUserInput(g Graph, op, id, run string, payload, result any) error {
	if op != "hint" && op != "reopen" {
		return Err(422, "unsupported user input event")
	}
	before, err := t.Load(g.Project.ID)
	if err != nil {
		return err
	}
	if err = t.Save(g); err != nil {
		return err
	}
	if err = t.CheckContextCapacity(g.Project.ID); err != nil {
		return err
	}
	after, err := t.Load(g.Project.ID)
	if err != nil {
		return err
	}
	// Compare persisted shared content, not the input graph: Save intentionally
	// ignores edits to immutable facts and ignores omitted optional metadata.
	// Heartbeats and lease ownership are not domain changes.
	if DecisionStateVersion(State{Graph: before}) == DecisionStateVersion(State{Graph: after}) {
		return nil
	}
	revision, err := t.advanceStateRevision(g.Project.ID, true)
	if err != nil {
		return err
	}
	input, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	output, err := json.Marshal(result)
	if err != nil {
		return err
	}
	event, err := json.Marshal(StateEvent{Revision: revision, Op: op, ID: id, RunID: run, CreatedAt: t.Now, Payload: input, Result: output})
	if err != nil {
		return err
	}
	_, err = t.Exec("INSERT INTO xloom_state_events(project_id,revision,event) VALUES(?,?,?)", g.Project.ID, revision, string(event))
	return err
}
