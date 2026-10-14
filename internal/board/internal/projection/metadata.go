// Package projection derives canonical state from retained values. It owns no
// storage, transactions or mutation authority.
package projection

import (
	"encoding/json"
	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/board/model"
)

type Metadata struct {
	Goals         []model.Goal           `json:"goals"`
	Steps         []StepMetadata         `json:"steps"`
	Facts         []model.FactRecord     `json:"facts"`
	Findings      []model.Finding        `json:"findings"`
	FactRelations []model.FactRelation   `json:"fact_relations"`
	Candidates    []model.Candidate      `json:"candidates,omitempty"`
	Disputes      []model.Dispute        `json:"disputes,omitempty"`
	Curation      model.CurationProgress `json:"curation,omitempty"`
}

// Persist only abandonment; other statuses are projected from the authoritative
// Intent and execution records.
type StepMetadata struct {
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

// DecodeMetadata accepts nil for an absent row. Historical runtime fields are
// ignored; only explicit abandonment can override current execution state.
func DecodeMetadata(raw []byte) (Metadata, error) {
	d := Metadata{Goals: []model.Goal{}, Steps: []StepMetadata{}, Facts: []model.FactRecord{}, Findings: []model.Finding{}, FactRelations: []model.FactRelation{}}
	if raw == nil {
		return d, nil
	}
	err := json.Unmarshal(raw, &d)
	for n := range d.Steps {
		if d.Steps[n].Status != "abandoned" {
			d.Steps[n].Status = ""
		}
	}
	return d, err
}
