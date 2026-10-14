package protocol

import (
	"errors"
	"pwnmesh/internal/board/model"
)

func ControlJob(j Job) bool { return j.Kind == "reason" || j.Kind == "curate" }

func OrchestrationJob(j Job) bool { return j.Graph.Project.OrchestrationVersion == 1 }

func GraphActions(j Job) []string {
	switch j.Kind {
	case "curate":
		return []string{"curate"}
	case "reason":
		return []string{"goal", "step", "curation_request"}
	case "explore":
		return []string{"fact", "candidate"}
	default:
		return nil
	}
}

func ValidateCuratorInput(j Job) error {
	if !OrchestrationJob(j) || !j.GraphRPC || j.Intent != nil || j.Decision != nil {
		return errors.New("curate requires protocol 1, a live bridge and an immutable state without an intent or decision")
	}
	if j.InputSnapshot != nil {
		return ValidateSnapshotInput(j)
	}
	if j.State == nil || len(j.InputView) != 0 {
		return errors.New("curate requires an immutable state or input snapshot")
	}
	if j.State.Graph.Project.ID != j.Graph.Project.ID || j.State.Graph.Project.Generation != j.Graph.Project.Generation || j.State.Graph.Project.OrchestrationVersion != 1 {
		return errors.New("curate state identity mismatch")
	}
	bound := *j.State
	bound.Graph = j.Graph
	if model.DecisionStateVersion(bound) != model.DecisionStateVersion(*j.State) {
		return errors.New("curate graph differs from its immutable state")
	}
	return nil
}

func (j Job) CurationVersion() string {
	if j.InputSnapshot != nil {
		return j.InputSnapshot.StateVersion
	}
	if j.State != nil {
		return model.DecisionStateVersion(*j.State)
	}
	return ""
}

func (j Job) CurationRevision() int64 {
	if j.InputSnapshot != nil {
		return j.InputSnapshot.Revision
	}
	if j.State != nil {
		return j.State.Revision
	}
	return 0
}
