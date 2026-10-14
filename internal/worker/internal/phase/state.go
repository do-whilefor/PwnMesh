// Package phase owns execution-phase policy independently of prompts, journals,
// model loops and storage. Transitions return values; the caller publishes a
// transition only at its durable boundary and performs effects afterwards.
package phase

import (
	"context"
	"errors"
	"time"
)

const MaxOutputRepairs = 2
const MaxContinuations = 3

var ErrRepairExhausted = errors.New("result-format repair exhausted")
var ErrContinuationExhausted = errors.New("continuation exhausted")

type Budget struct {
	Execution  time.Duration
	Conclusion time.Duration
}

// State contains only durable policy facts. Repair can overlap conclusion:
// repairing a conclusion must not restore tools or refresh its deadline.
type State struct {
	StartedAt            time.Time
	ExecutionDeadline    time.Time
	ReasonDeadline       time.Time
	ConcludeStartedAt    time.Time
	ConcludeDeadline     time.Time
	Concluding           bool
	Repairing            bool
	RepairCount          int
	RepairPending        bool
	ContinuationCount    int
	ContinuationSequence uint64
}

func New(start time.Time, budget Budget) State {
	s := State{StartedAt: start}
	if budget.Execution > 0 {
		s.ExecutionDeadline = start.Add(budget.Execution)
	}
	return s
}

func (s State) ValidateAllowances(hasRepairPrompt bool) error {
	if s.RepairCount < 0 || s.RepairCount > MaxOutputRepairs || (s.Repairing && (s.RepairCount == 0 || !hasRepairPrompt)) || (!s.Repairing && (s.RepairPending || hasRepairPrompt)) {
		return errors.New("invalid saved result-repair state")
	}
	if s.ContinuationCount < 0 || s.ContinuationCount > MaxContinuations {
		return errors.New("invalid saved continuation count")
	}
	return nil
}

func (s State) ValidateDeadlines(budget Budget) error {
	if s.ExecutionDeadline.IsZero() != (budget.Execution == 0) {
		return errors.New("missing original execution deadline")
	}
	if budget.Execution > 0 && !s.ExecutionDeadline.Equal(s.StartedAt.Add(budget.Execution)) {
		return errors.New("execution deadline differs from original budget")
	}
	if s.Concluding && (s.ConcludeStartedAt.IsZero() || s.ConcludeDeadline.IsZero()) {
		return errors.New("conclusion lacks its original deadline")
	}
	if !s.ReasonDeadline.IsZero() && (s.ExecutionDeadline.IsZero() || s.ReasonDeadline.After(s.ExecutionDeadline)) {
		return errors.New("reason deadline exceeds the original execution budget")
	}
	if s.Concluding && !s.ConcludeDeadline.Equal(s.ConcludeStartedAt.Add(budget.Conclusion)) {
		return errors.New("conclusion deadline differs from original budget")
	}
	return nil
}

func (s State) ShouldConclude(now time.Time, control, softStop bool) bool {
	return s.CanConclude(control) && (softStop || (!s.ExecutionDeadline.IsZero() && !now.Before(s.ExecutionDeadline)))
}

func (s State) CanConclude(control bool) bool {
	return !control && !s.Concluding
}

func (s State) BeginConclusion(now time.Time, budget Budget) (State, error) {
	next := s
	if next.ConcludeStartedAt.IsZero() {
		if next.Concluding {
			return s, errors.New("saved conclusion has no start time; cannot refresh its deadline")
		}
		next.ConcludeStartedAt = now
	}
	if next.ConcludeDeadline.IsZero() {
		next.ConcludeDeadline = next.ConcludeStartedAt.Add(budget.Conclusion)
	}
	if !now.Before(next.ConcludeDeadline) {
		return s, context.DeadlineExceeded
	}
	next.Concluding = true
	return next, nil
}

// BoundControl preserves a saved shorter deadline and never starts a new budget.
func (s State) BoundControl() State {
	if s.ReasonDeadline.IsZero() {
		s.ReasonDeadline = s.ExecutionDeadline
	}
	return s
}

func (s State) BeginRepair() (State, error) {
	if s.RepairCount >= MaxOutputRepairs {
		return s, ErrRepairExhausted
	}
	s.RepairCount++
	s.Repairing, s.RepairPending = true, true
	return s, nil
}

func (s State) EndRepair() State {
	s.Repairing, s.RepairPending = false, false
	return s
}

func (s State) RepairResponded() State {
	if s.Repairing {
		s.RepairPending = false
	}
	return s
}

// Continue consumes once per durable assistant message. Reissuing a follow-up
// after a crash neither replenishes nor spends another allowance.
func (s State) Continue(sequence uint64) (State, error) {
	if sequence == 0 {
		return s, errors.New("continuation requires a durable assistant message")
	}
	if s.ContinuationSequence != sequence {
		if s.ContinuationCount >= MaxContinuations {
			return s, ErrContinuationExhausted
		}
		s.ContinuationCount++
		s.ContinuationSequence = sequence
	}
	return s.EndRepair(), nil
}

// SuccessfulProgress runs in the same session save as settled tool results.
// Its caller owns tool-progress identity and only reports newly settled work.
func (s State) SuccessfulProgress() State {
	s.ContinuationCount = 0
	return s
}
