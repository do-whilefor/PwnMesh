package worker

import (
	"errors"
	"pwnmesh/internal/board"
)

func controlJob(j Job) bool { return j.Kind == "reason" || j.Kind == "curate" }

func orchestrationJob(j Job) bool { return j.Graph.Project.OrchestrationVersion == 1 }

func graphActions(j Job) []string {
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

func validateCuratorInput(j Job) error {
	if !orchestrationJob(j) || !j.GraphRPC || j.Intent != nil || j.Decision != nil {
		return errors.New("curate requires protocol 1, a live bridge and an immutable state without an intent or decision")
	}
	if j.InputSnapshot != nil {
		return validateSnapshotInput(j)
	}
	if j.State == nil || len(j.InputView) != 0 {
		return errors.New("curate requires an immutable state or input snapshot")
	}
	if j.State.Graph.Project.ID != j.Graph.Project.ID || j.State.Graph.Project.Generation != j.Graph.Project.Generation || j.State.Graph.Project.OrchestrationVersion != 1 {
		return errors.New("curate state identity mismatch")
	}
	bound := *j.State
	bound.Graph = j.Graph
	if board.DecisionStateVersion(bound) != board.DecisionStateVersion(*j.State) {
		return errors.New("curate graph differs from its immutable state")
	}
	return nil
}

func (j Job) curationVersion() string {
	if j.InputSnapshot != nil {
		return j.InputSnapshot.StateVersion
	}
	if j.State != nil {
		return board.DecisionStateVersion(*j.State)
	}
	return ""
}

func (j Job) curationRevision() int64 {
	if j.InputSnapshot != nil {
		return j.InputSnapshot.Revision
	}
	if j.State != nil {
		return j.State.Revision
	}
	return 0
}
