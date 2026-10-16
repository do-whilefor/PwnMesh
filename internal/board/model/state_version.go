package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// canonicalState preserves the existing field order and omission rules. Keep
// Curation last: snapshots and existing decision hashes bind these exact bytes.
// New presentation values must not implicitly become part of this contract.
type canonicalState struct {
	Graph            Graph            `json:"graph"`
	Goals            []Goal           `json:"goals"`
	Steps            []Step           `json:"steps"`
	FactRecords      []FactRecord     `json:"fact_records"`
	Findings         []Finding        `json:"findings"`
	FactRelations    []FactRelation   `json:"fact_relations"`
	Revision         int64            `json:"revision"`
	DecisionRevision int64            `json:"decision_revision"`
	Candidates       []Candidate      `json:"candidates,omitempty"`
	Disputes         []Dispute        `json:"disputes,omitempty"`
	Assets           []Asset          `json:"assets,omitempty"`
	AssetAnchors     []AssetAnchor    `json:"asset_anchors,omitempty"`
	Curation         CurationProgress `json:"curation"`
}

func marshalCanonicalState(state State) ([]byte, error) {
	return json.Marshal(canonicalState{
		Graph: state.Graph, Goals: state.Goals, Steps: state.Steps,
		FactRecords: state.FactRecords, Findings: state.Findings, FactRelations: state.FactRelations,
		Revision: state.Revision, DecisionRevision: state.DecisionRevision,
		Candidates: state.Candidates, Disputes: state.Disputes,
		Assets: state.Assets, AssetAnchors: state.AssetAnchors, Curation: state.Curation,
	})
}

// canonicalDecisionState removes runtime activity without changing shared input
// or the caller's snapshot. Preserve the historical nil-to-empty normalization.
func canonicalDecisionState(state State) State {
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
	return state
}

// DecisionStateVersion binds a plan to the explicit canonical shared content.
func DecisionStateVersion(state State) string {
	raw, _ := marshalCanonicalState(canonicalDecisionState(state))
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
