//go:build linux

package worker

import (
	"time"

	"pwnmesh/internal/board/model"
	"pwnmesh/internal/worker/internal/phase"
)

func phaseBudget(b model.TaskBudget) phase.Budget {
	return phase.Budget{Execution: time.Duration(b.Timeout) * time.Second, Conclusion: time.Duration(b.ConcludeTimeout) * time.Second}
}

// The session remains the sole durable state owner. Explicit conversion keeps
// its persisted JSON fields and ordering independent of the policy package.
func (s *session) phaseState() phase.State {
	return phase.State{
		StartedAt: s.StartedAt, ExecutionDeadline: s.ExecutionDeadline, ReasonDeadline: s.ReasonDeadline,
		ConcludeStartedAt: s.ConcludeStartedAt, ConcludeDeadline: s.ConcludeDeadline, Concluding: s.Concluding,
		Repairing: s.Repairing, RepairCount: s.RepairCount, RepairPending: s.RepairPending,
		ContinuationCount: s.ContinuationCount, ContinuationSequence: s.ContinuationSequence,
	}
}

func (s *session) setPhaseState(next phase.State) {
	s.StartedAt, s.ExecutionDeadline, s.ReasonDeadline = next.StartedAt, next.ExecutionDeadline, next.ReasonDeadline
	s.ConcludeStartedAt, s.ConcludeDeadline, s.Concluding = next.ConcludeStartedAt, next.ConcludeDeadline, next.Concluding
	s.Repairing, s.RepairCount, s.RepairPending = next.Repairing, next.RepairCount, next.RepairPending
	s.ContinuationCount, s.ContinuationSequence = next.ContinuationCount, next.ContinuationSequence
}

// Worker supplies a generic request policy. Durable phase names and prompts
// remain owned by the session; only the initial task is initialized by Loop.
func (r *sessionRuntime) applyPhase() {
	if r.loop == nil {
		return
	}
	r.loop.ToolsDisabledReason = ""
	if r.state.Concluding {
		r.loop.ToolsDisabledReason = "all tools are disabled during conclusion; use the supplied snapshot and session evidence"
	} else if r.state.Repairing {
		r.loop.ToolsDisabledReason = "all tools are disabled during result-format repair; use existing session evidence"
	}
	r.loop.PinnedInstructions = nil
	for _, instruction := range []string{r.state.ConclusionPrompt, r.state.RepairPrompt} {
		if instruction != "" {
			r.loop.PinnedInstructions = append(r.loop.PinnedInstructions, instruction)
		}
	}
}

func (r *sessionRuntime) endRepair() {
	r.state.setPhaseState(r.state.phaseState().EndRepair())
	r.state.RepairPrompt = ""
	r.applyPhase()
}

// Commit policy facts before appending an instruction or reading new evidence.
// A rejected save must not leave an uncommitted allowance or phase in memory.
// Journal records remain append-only, including any uncommitted crash tail.
func (r *sessionRuntime) savePhase(next phase.State, repairPrompt string) error {
	before := r.state
	loop := r.loop
	policy, instructions := loop.ToolsDisabledReason, loop.PinnedInstructions
	r.state.setPhaseState(next)
	r.state.RepairPrompt = repairPrompt
	r.applyPhase()
	if err := r.save(loop.History); err != nil {
		r.state = before
		loop.ToolsDisabledReason, loop.PinnedInstructions = policy, instructions
		return err
	}
	return nil
}
