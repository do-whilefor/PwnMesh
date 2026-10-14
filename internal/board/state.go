package board

import (
	"pwnmesh/internal/board/internal/projection"
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

type StateEvent = model.StateEvent
type stateData = projection.Metadata
type stepMetadata = projection.StepMetadata

func stepMetadataFrom(step Step) stepMetadata {
	metadata := stepMetadata{ID: step.ID, GoalID: step.GoalID, Priority: step.Priority, Reason: step.Reason, DisputeID: step.DisputeID, DependsOn: append([]string(nil), step.DependsOn...), WritePaths: append([]string(nil), step.WritePaths...), Repair: step.Repair}
	if step.Status == "abandoned" {
		metadata.Status = "abandoned"
	}
	return metadata
}
