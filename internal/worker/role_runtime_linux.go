//go:build linux

package worker

import (
	"context"
	"pwnmesh/internal/agent"
	"pwnmesh/internal/protocol"
)

// roleRuntime owns the mutable capabilities of one invocation. Graph state is
// copied across the session persistence boundary, never shared through pointers.
type roleRuntime struct {
	graph      graphRuntimeState
	tools      []agent.Tool
	decision   *decisionDraft
	curation   *curationCommit
	stepFinish *stepFinish
	emit       agent.Emit
	request    func(context.Context, GraphRequest) (string, error)
}

type graphRuntimeState struct{ Version, Conflict string }

func (r *roleRuntime) stopResult() (string, bool) {
	if r.stepFinish != nil {
		return r.stepFinish.result()
	}
	if r.curation != nil {
		return r.curation.result()
	}
	if r.decision != nil {
		return r.decision.result()
	}
	return "", false
}

func (r *roleRuntime) bindLoop(loop *agent.Loop) {
	if r.decision != nil || r.curation != nil || r.stepFinish != nil {
		loop.StopResult = r.stopResult
	}
	if r.stepFinish != nil {
		loop.StopResultTools = []string{"finish_step"}
	}
}

// Receipt authority is settled before a model session is resumed. A lost draft
// is process-local; only its server receipt can prove a completed operation.
func (r *roleRuntime) recover(ctx context.Context, j Job, resuming bool) (string, bool, error) {
	if r.stepFinish != nil {
		if err := r.stepFinish.recover(ctx); err != nil {
			return "", false, err
		}
		if text, done := r.stepFinish.result(); done {
			return text, true, nil
		}
	}
	if r.curation != nil {
		if _, err := r.curation.recover(ctx); err != nil {
			return "", false, err
		}
		if text, done := r.curation.result(); done {
			return text, true, nil
		}
	}
	if r.decision != nil {
		if protocol.OrchestrationJob(j) && j.Decision != nil {
			r.decision.assessment = j.Decision.CompletionAssessment
		}
		if _, err := r.decision.recover(ctx); err != nil {
			return "", false, err
		}
		if text, done := r.decision.result(); done {
			return text, true, nil
		}
		r.adaptUncommittedPlanner(resuming)
	}
	return "", false, nil
}

// Older bound sessions persisted a terminal planner conflict. Adapt it only
// after checking the authoritative receipt. Recovery never restores a private
// draft or grants fresh deadlines, repair allowances or continuation slots.
func (r *roleRuntime) adaptUncommittedPlanner(resuming bool) {
	if r.graph.Conflict != "" || resuming {
		r.decision.invalidate()
	}
	r.graph.Conflict = ""
}
