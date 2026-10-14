//go:build linux

package worker

import (
	"context"
	"errors"
	"fmt"
	"pwnmesh/internal/agent"
	"time"
)

// sessionPhases changes phase only at settled Agent boundaries. Deadlines and
// repair/continuation allowances are always owned by the durable session.
type sessionPhases struct {
	runtime   *sessionRuntime
	ctx       context.Context
	phaseCtx  context.Context
	endCancel context.CancelFunc
}

func (p *sessionPhases) startConclusion() (context.Context, error) {
	j, o, state, l, ctx := p.runtime.job, p.runtime.options, &p.runtime.state, p.runtime.loop, p.ctx
	save := p.runtime.save

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A soft stop can precede the first model request. Seed the same pinned
	// task used by normal execution before appending the phase-only input.
	if l.TaskPrompt == "" {
		if len(l.History) > 0 {
			l.TaskPrompt = l.History[0].Text()
		} else {
			var err error
			l.TaskPrompt, err = Prompt(j, false)
			if err != nil {
				return nil, err
			}
		}
	}
	if len(l.History) == 0 {
		if err := l.AppendInstruction(l.TaskPrompt); err != nil {
			return nil, err
		}
	}
	wasConcluding := state.Concluding
	state.Concluding = true
	l.Concluding = true
	if state.ConcludeStartedAt.IsZero() {
		if wasConcluding {
			return nil, errors.New("saved conclusion has no start time; cannot refresh its deadline")
		}
		state.ConcludeStartedAt = o.Now()
	}
	if state.ConcludeDeadline.IsZero() {
		// Only a newly entered conclusion can establish its deadline.
		state.ConcludeDeadline = state.ConcludeStartedAt.Add(time.Duration(j.Budget.ConcludeTimeout) * time.Second)
	}
	remaining := state.ConcludeDeadline.Sub(o.Now())
	if remaining <= 0 {
		return nil, context.DeadlineExceeded
	}
	next, cancel := context.WithTimeout(ctx, remaining)
	p.endCancel = cancel
	p.phaseCtx = next
	if !wasConcluding {
		// Freeze the boundary before reading any artifact. A crash during
		// preparation resumes conservatively without reading new outputs.
		if err := save(l.History); err != nil {
			return nil, err
		}
	}
	if state.ConclusionInputVersion != conclusionInputVersion || l.ConclusionPrompt == "" {
		input, refs, err := conclusionInputWithEvidence(next, j, o.RunDir, !wasConcluding)
		if err != nil {
			return nil, err
		}
		l.ConclusionPrompt = input
		state.ConclusionEvidence = refs
		state.ConclusionInputVersion = conclusionInputVersion
		// Persist the frozen input before another model request. Recovery
		// reuses it even if files have changed or disappeared since then.
		if err = save(l.History); err != nil {
			return nil, err
		}
	}
	return next, nil
}

func (p *sessionPhases) shouldConclude() bool {
	j, o, state, l := p.runtime.job, p.runtime.options, &p.runtime.state, p.runtime.loop

	if controlJob(j) || l.Concluding {
		return false
	}
	select {
	case <-o.SoftStop:
		return true
	default:
	}
	return !state.ExecutionDeadline.IsZero() && !o.Now().Before(state.ExecutionDeadline)
}

func (p *sessionPhases) beforeRequest(turnCtx context.Context, loop *agent.Loop) (context.Context, error) {
	j, o, state := p.runtime.job, p.runtime.options, &p.runtime.state
	save := p.runtime.save

	if !loop.Concluding && !loop.Repairing && !p.shouldConclude() {
		if err := state.ToolProgress.problem(); err != nil {
			return nil, err
		}
	}
	if state.DecisionConflict != "" {
		return nil, errors.New(state.DecisionConflict)
	}
	if o.decision != nil {
		o.decision.beforeRequest(loop)
	}
	concludeAtBoundary := func() error {
		next, err := p.startConclusion()
		if err != nil {
			return err
		}
		turnCtx = next
		return loop.AppendInstruction(loop.ConclusionPrompt)
	}
	if p.shouldConclude() {
		if err := concludeAtBoundary(); err != nil {
			return nil, err
		}
	}
	if err := refreshExecutionUpdates(turnCtx, j, o.graphRequest, state, loop, save); err != nil {
		return nil, err
	}
	// A slow graph read cannot buy another exploration turn. Enter the same
	// bounded conclusion used by settled tool turns, then record deferral.
	if p.shouldConclude() {
		if err := concludeAtBoundary(); err != nil {
			return nil, err
		}
		if err := refreshExecutionUpdates(turnCtx, j, o.graphRequest, state, loop, save); err != nil {
			return nil, err
		}
	}
	return turnCtx, nil
}

func (p *sessionPhases) prepareRepairHistory() error {
	l := p.runtime.loop

	if err := l.RepairHistory(); err != nil {
		return err
	}
	if l.Concluding && !containsInstruction(l.History, l.ConclusionPrompt) {
		return l.AppendInstruction(l.ConclusionPrompt)
	}
	return nil
}

func (p *sessionPhases) startRepair(turnCtx context.Context, problem *outputFailure) (string, error) {
	j, state, l := p.runtime.job, &p.runtime.state, p.runtime.loop
	save := p.runtime.save

	if err := turnCtx.Err(); err != nil {
		return "", err
	}
	if state.RepairCount >= maxOutputRepairs {
		return "", fmt.Errorf("result-format repair exhausted after %d attempts: %w", maxOutputRepairs, problem)
	}
	state.RepairCount++
	state.Repairing = true
	l.Repairing = true
	instruction, err := repairInstruction(j, l.Concluding, state.RepairCount, problem)
	if err != nil {
		return "", err
	}
	l.RepairPrompt = instruction
	state.RepairPending = true
	// Consume the attempt before adding its prompt or making a request.
	if err = save(l.History); err != nil {
		return "", err
	}
	if err = p.prepareRepairHistory(); err != nil {
		return "", err
	}
	return instruction, nil
}

func (p *sessionPhases) continueExecution() (string, error) {
	o, state, l := p.runtime.options, &p.runtime.state, p.runtime.loop
	save := p.runtime.save

	last, ok := lastAssistant(l.History)
	if !ok || last.Sequence == 0 {
		return "", errors.New("continuation requires a durable assistant message")
	}
	if state.ContinuationSequence != last.Sequence {
		if state.ContinuationCount >= maxContinuations {
			return "", &outputFailure{Reason: "continuation_exhausted", Detail: "repeated continue responses made no successful tool progress"}
		}
		state.ContinuationCount++
		state.ContinuationSequence = last.Sequence
	}
	state.Repairing, state.RepairPending, l.Repairing = false, false, false
	state.RepairPrompt, l.RepairPrompt = "", ""
	// Persist consumption before the follow-up instruction. Recovery can
	// then reissue that instruction without replaying tools or buying turns.
	if err := save(l.History); err != nil {
		return "", err
	}
	if o.decision != nil {
		return "This Decide has no committed receipt. Use native tool calls for read_graph, assess_root when available, and graph_action; XML/JSON text is not executed. Then commit the draft; an empty plan requires a valid open or running Step. For truncated tool calls, reissue complete arguments. The original deadline still applies. If unable to proceed, return accepted:false with a reason.", nil
	}
	if o.curation != nil {
		return "This curation has no committed receipt. Read the supplied evidence and submit graph_action curate using native tool calls; XML/JSON text is not executed. The original input boundary and deadline still apply. If unable to proceed, return accepted:false with a reason.", nil
	}
	return "Continue the unfinished work in this same execution. Tools are enabled. The original task deadline still applies. Use completed only when the assigned task is finished; otherwise continue working or report incomplete with the remaining work and blocker.", nil
}

func (p *sessionPhases) onTurnEnd(turnCtx context.Context, l *agent.Loop, m agent.Message) (context.Context, string, error) {
	j, o, state, ctx := p.runtime.job, p.runtime.options, &p.runtime.state, p.ctx
	save := p.runtime.save

	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if state.DecisionConflict != "" {
		return nil, "", errors.New(state.DecisionConflict)
	}
	hasCalls := hasToolCalls(m)
	if hasCalls && !l.Concluding && !l.Repairing && !truncated(m) && !p.shouldConclude() {
		if err := state.ToolProgress.problem(); err != nil {
			return nil, "", err
		}
		if state.ToolProgress.needsCorrection() {
			state.ToolProgress.Warned = true
			if err := save(l.History); err != nil {
				return nil, "", err
			}
			return turnCtx, "Recent tools have repeated unchanged observations or kept failing. Reassess the blocker and change the approach; do not repeat the same unsuccessful call or unchanged read without a reason to expect new information. If unable to proceed, Reason and Curate return accepted:false with a reason; execution tasks report incomplete with the blocker. The original task and budgets still apply.", nil
		}
	}
	if o.decision != nil || o.curation != nil {
		if !truncated(m) {
			if hasCalls {
				return turnCtx, "", nil
			}
			if parsed, err := parseOutput(j, false, m.Text()); err == nil && parsed.Kind == "rejected" {
				return turnCtx, "", nil
			}
		}
		instruction, err := p.continueExecution()
		return turnCtx, instruction, err
	}
	problem := outputProblem(j, l.Concluding, m)
	needsResult := !hasCalls || truncated(m) || l.Concluding || l.Repairing
	if !l.Concluding && !controlJob(j) && p.shouldConclude() {
		next, err := p.startConclusion()
		if err != nil {
			return nil, "", err
		}
		if needsResult && problem != nil {
			instruction, err := p.startRepair(next, problem)
			return next, instruction, err
		}
		return next, l.ConclusionPrompt, nil
	}
	if needsResult && problem != nil {
		instruction, err := p.startRepair(turnCtx, problem)
		return turnCtx, instruction, err
	}
	if needsResult {
		parsed, err := parseOutput(j, l.Concluding, m.Text())
		if err != nil {
			return nil, "", err
		}
		if parsed.Outcome == "continue" {
			instruction, err := p.continueExecution()
			return turnCtx, instruction, err
		}
	}
	return turnCtx, "", nil
}
