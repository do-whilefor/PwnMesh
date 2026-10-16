package model

import (
	"encoding/json"

	"pwnmesh/internal/artifactcheck"
)

// State extends PwnMesh's graph while retaining its historical JSON contract.
type State struct {
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
	Curation         CurationProgress `json:"curation,omitempty"`
	Assets           []Asset          `json:"assets,omitempty"`
	AssetAnchors     []AssetAnchor    `json:"asset_anchors,omitempty"`
}

type EvidenceRef struct {
	RunID     string `json:"run_id"`
	Path      string `json:"path"`
	Excerpt   string `json:"excerpt"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}
type FactRecord struct {
	ID             string        `json:"id"`
	Description    string        `json:"description"`
	Scope          string        `json:"scope"`
	ObservedAt     string        `json:"observed_at"`
	Evidence       []EvidenceRef `json:"evidence"`
	Status         string        `json:"status"`
	RunID          string        `json:"run_id,omitempty"`
	SourceStepID   string        `json:"source_step_id,omitempty"`
	Legacy         bool          `json:"legacy"`
	SupportInvalid bool          `json:"support_invalid,omitempty"`
	DerivedFrom    []string      `json:"derived_from,omitempty"`
}
type FactRelation struct {
	Kind      string `json:"kind"`
	Source    string `json:"source"`
	Target    string `json:"target"`
	Reason    string `json:"reason"`
	RunID     string `json:"run_id"`
	CreatedAt string `json:"created_at"`
}
type Goal struct {
	ID           string   `json:"id"`
	ParentID     string   `json:"parent_id,omitempty"`
	Condition    string   `json:"condition"`
	Status       string   `json:"status"`
	Sources      []string `json:"sources"`
	Reason       string   `json:"reason,omitempty"`
	CreatedAt    string   `json:"created_at"`
	SupportValid bool     `json:"support_valid"`
}

// Assessment binds coordinated producers to one proposition and observation scope.
type Assessment struct {
	Claim string `json:"claim"`
	Scope string `json:"scope"`
}

type Step struct {
	Assessment     *Assessment         `json:"assessment,omitempty"`
	Repair         *artifactcheck.Spec `json:"repair,omitempty"`
	ID             string              `json:"id"`
	From           []string            `json:"from"`
	GoalID         string              `json:"goal_id"`
	Description    string              `json:"description"`
	Status         string              `json:"status"`
	Priority       int                 `json:"priority"`
	Result         *string             `json:"result"`
	Worker         *string             `json:"worker"`
	Reason         string              `json:"reason,omitempty"`
	CreatedAt      string              `json:"created_at"`
	InvalidSources []string            `json:"invalid_sources,omitempty"`
	DisputeID      string              `json:"dispute_id,omitempty"`
	DependsOn      []string            `json:"depends_on,omitempty"`
	WritePaths     []string            `json:"write_paths,omitempty"`
	BlockedBy      []string            `json:"blocked_by,omitempty"`
	SupportValid   bool                `json:"support_valid,omitempty"`
	// The latest failed or retry-authorized attempt. Ordinary first starts do
	// not stale plans; consuming an explicit retry grant does change this ID.
	LatestRunID string `json:"latest_run_id,omitempty"`
}
type Finding struct {
	ID              string        `json:"id"`
	Claim           string        `json:"claim"`
	Scope           string        `json:"scope"`
	Status          string        `json:"status"`
	Sources         []string      `json:"sources"`
	Evidence        []EvidenceRef `json:"evidence"`
	Reason          string        `json:"reason,omitempty"`
	CreatedAt       string        `json:"created_at"`
	UpdatedAt       string        `json:"updated_at"`
	SupportValid    bool          `json:"support_valid"`
	CandidateIDs    []string      `json:"candidate_ids,omitempty"`
	DisputeID       string        `json:"dispute_id,omitempty"`
	CuratedRevision int64         `json:"curated_revision,omitempty"`
}
type StateAction struct {
	Op              string          `json:"op"`
	IdempotencyKey  string          `json:"idempotency_key"`
	Payload         json.RawMessage `json:"payload"`
	ExpectedVersion string          `json:"expected_version,omitempty"`
}
type StateActionResult struct {
	Op           string          `json:"op"`
	ID           string          `json:"id"`
	Revision     int64           `json:"revision"`
	Result       json.RawMessage `json:"result"`
	StateVersion string          `json:"state_version,omitempty"`
	Unchanged    bool            `json:"unchanged,omitempty"`
	Committed    bool            `json:"committed,omitempty"`
	AssetIDs     []string        `json:"asset_ids,omitempty"`
}

// Curation is the final field in the current canonical encoding. Keep that
// order: frozen inputs and decision state versions hash these exact JSON bytes.
func (s State) MarshalJSON() ([]byte, error) {
	type stateAlias State
	view := struct {
		*stateAlias
		Curation *CurationProgress `json:"curation"`
	}{stateAlias: (*stateAlias)(&s), Curation: &s.Curation}
	return json.Marshal(view)
}

func (s State) ValidateFactSources(ids []string, requireObservation bool) error {
	if len(ids) == 0 {
		return Err(422, "at least one source fact is required")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return Err(422, "duplicate source fact")
		}
		seen[id] = true
		found := false
		for _, f := range s.FactRecords {
			if f.ID != id {
				continue
			}
			found = true
			// Orchestrated conclusions require an actual observation. Reopen
			// feedback is a valid legacy Fact for planning, never evidence that
			// the newly requested work has already been performed.
			if requireObservation && s.Graph.Project.OrchestrationVersion == 1 && (f.Legacy || len(f.Evidence) == 0) {
				return Err(409, "Source fact "+id+" is not an evidence-backed observation")
			}
			if f.SupportInvalid || (f.Status != "valid" && !(id == "origin" && !requireObservation)) {
				return Err(409, "Source fact "+id+" is not effective evidence")
			}
		}
		if !found {
			return Err(404, "Fact "+id+" not found")
		}
	}
	return nil
}
