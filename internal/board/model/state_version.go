package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// DecisionStateVersion binds a plan to shared content without runtime metadata.
func DecisionStateVersion(state State) string {
	state.Revision, state.DecisionRevision = 0, 0
	state.Graph.Project.Reason = nil
	state.Graph.Project.Curator = nil
	state.Graph.Intents = append([]Intent{}, state.Graph.Intents...)
	for n := range state.Graph.Intents {
		state.Graph.Intents[n].Heartbeat = nil
		state.Graph.Intents[n].Worker = nil
	}
	state.Steps = append([]Step{}, state.Steps...)
	for n := range state.Steps {
		state.Steps[n].Worker = nil
		// Claiming work changes neither its plan nor its evidence. Keep actual
		// running status for operation checks, but do not stale a decision when
		// the dispatcher starts a step that decision just proposed.
		if state.Steps[n].Status == "running" {
			state.Steps[n].Status = "open"
		}
	}
	raw, _ := json.Marshal(state)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
