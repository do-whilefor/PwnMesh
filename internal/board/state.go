package board

import (
	"encoding/json"

	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/board/model"
)

type State = model.State

type EvidenceRef = model.EvidenceRef
type FactRecord = model.FactRecord
type FactRelation = model.FactRelation
type Goal = model.Goal
type Step = model.Step
type Finding = model.Finding
type StateAction = model.StateAction
type StateActionResult = model.StateActionResult

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
