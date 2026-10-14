package board

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
type Step struct {
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
type StateEvent struct {
	Revision  int64           `json:"revision"`
	Op        string          `json:"op"`
	ID        string          `json:"id"`
	RunID     string          `json:"run_id"`
	CreatedAt string          `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
	Result    json.RawMessage `json:"result"`
}
type stateData struct {
	Goals         []Goal           `json:"goals"`
	Steps         []stepMetadata   `json:"steps"`
	Facts         []FactRecord     `json:"facts"`
	Findings      []Finding        `json:"findings"`
	FactRelations []FactRelation   `json:"fact_relations"`
	Candidates    []Candidate      `json:"candidates,omitempty"`
	Disputes      []Dispute        `json:"disputes,omitempty"`
	Curation      CurationProgress `json:"curation,omitempty"`
}

// Persist only abandonment; other statuses are projected from the authoritative
// Intent and execution records.
type stepMetadata struct {
	Repair     *artifactcheck.Spec `json:"repair,omitempty"`
	ID         string              `json:"id"`
	GoalID     string              `json:"goal_id"`
	Priority   int                 `json:"priority"`
	Reason     string              `json:"reason,omitempty"`
	Status     string              `json:"status,omitempty"`
	DisputeID  string              `json:"dispute_id,omitempty"`
	DependsOn  []string            `json:"depends_on,omitempty"`
	WritePaths []string            `json:"write_paths,omitempty"`
}

func stepMetadataFrom(step Step) stepMetadata {
	metadata := stepMetadata{ID: step.ID, GoalID: step.GoalID, Priority: step.Priority, Reason: step.Reason, DisputeID: step.DisputeID, DependsOn: append([]string(nil), step.DependsOn...), WritePaths: append([]string(nil), step.WritePaths...), Repair: step.Repair}
	if step.Status == "abandoned" {
		metadata.Status = "abandoned"
	}
	return metadata
}
