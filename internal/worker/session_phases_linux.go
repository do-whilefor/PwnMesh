//go:build linux

package worker

import (
	"context"
	"errors"
	"fmt"
	"pwnmesh/internal/agent"
	"pwnmesh/internal/worker/internal/phase"
)

// sessionPhases adapts pure phase decisions to settled Agent boundaries. It
// owns effects (contexts, prompt/evidence assembly and persistence); policy
// receives only values and cannot access the session, Loop or their hooks.
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
	transition, err := state.phaseState().BeginConclusion(o.Now(), phaseBudget(j.Budget))
	if err != nil {
		return nil, err
	}
	remaining := transition.ConcludeDeadline.Sub(o.Now())
	if remaining <= 0 {
		return nil, context.DeadlineExceeded
	}
	next, cancel := context.WithTimeout(ctx, remaining)
	if !wasConcluding {
		// Freeze the boundary before reading any artifact. A crash during
		// preparation resumes conservatively without reading new outputs.
		if err := p.runtime.savePhase(transition, state.RepairPrompt); err != nil {
			cancel()
			return nil, err
		}
	}
	p.endCancel = cancel
	p.phaseCtx = next
	if state.ConclusionInputVersion != conclusionInputVersion || l.ConclusionPrompt == "" {
		input, refs, err := conclusionInputWithEvidence(next, j, o.RunDir, !wasConcluding)
		if err != nil {
			return nil, err
		}
		before, beforePrompt := *state, l.ConclusionPrompt
		state.ConclusionPrompt, l.ConclusionPrompt = input, input
		state.ConclusionEvidence = refs
		state.ConclusionInputVersion = conclusionInputVersion
		// Persist the frozen input before another model request. Recovery
		// reuses it even if files have changed or disappeared since then.
		if err = save(l.History); err != nil {
			*state, l.ConclusionPrompt = before, beforePrompt
			return nil, err
		}
	}
	return next, nil
}

func (p *sessionPhases) shouldConclude() bool {
	j, o, state := p.runtime.job, p.runtime.options, &p.runtime.state
	if !state.phaseState().CanConclude(controlJob(j)) {
		return false
	}
	softStop := false
	select {
	case <-o.SoftStop:
		softStop = true
	default:
	}
	return state.phaseState().ShouldConclude(o.Now(), controlJob(j), softStop)
}

func (p *sessionPhases) beforeRequest(turnCtx context.Context, loop *agent.Loop) (context.Context, error) {
	state := &p.runtime.state

	if !loop.Concluding && !loop.Repairing && !p.shouldConclude() {
		if err := state.ToolProgress.problem(); err != nil {
			return nil, err
		}
	}
	if p.runtime.roles.graph.Conflict != "" {
		return nil, errors.New(p.runtime.roles.graph.Conflict)
	}
	if p.runtime.roles.decision != nil {
		p.runtime.roles.decision.beforeRequest(loop)
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
	if err := p.runtime.refreshExecutionUpdates(turnCtx); err != nil {
		return nil, err
	}
	// A slow graph read cannot buy another exploration turn. Enter the same
	// bounded conclusion used by settled tool turns, then record deferral.
	if p.shouldConclude() {
		if err := concludeAtBoundary(); err != nil {
			return nil, err
		}
		if err := p.runtime.refreshExecutionUpdates(turnCtx); err != nil {
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
	if err := turnCtx.Err(); err != nil {
		return "", err
	}
	next, err := state.phaseState().BeginRepair()
	if errors.Is(err, phase.ErrRepairExhausted) {
		return "", fmt.Errorf("result-format repair exhausted after %d attempts: %w", maxOutputRepairs, problem)
	}
	if err != nil {
		return "", err
	}
	instruction, err := repairInstruction(j, l.Concluding, next.RepairCount, problem)
	if err != nil {
		return "", err
	}
	// Consume the attempt before adding its prompt or making a request.
	if err = p.runtime.savePhase(next, instruction); err != nil {
		return "", err
	}
	if err = p.prepareRepairHistory(); err != nil {
		return "", err
	}
	return instruction, nil
}

func (p *sessionPhases) continueExecution() (string, error) {
	state, l := &p.runtime.state, p.runtime.loop

	last, ok := lastAssistant(l.History)
	if !ok || last.Sequence == 0 {
		return "", errors.New("continuation requires a durable assistant message")
	}
	next, err := state.phaseState().Continue(last.Sequence)
	if errors.Is(err, phase.ErrContinuationExhausted) {
		return "", &outputFailure{Reason: "continuation_exhausted", Detail: "repeated continue responses made no successful tool progress"}
	}
	if err != nil {
		return "", err
	}
	// Persist consumption before the follow-up instruction. Recovery can
	// then reissue that instruction without replaying tools or buying turns.
	if err := p.runtime.savePhase(next, ""); err != nil {
		return "", err
	}
	if p.runtime.roles.decision != nil {
		return "This Decide has no committed receipt. Use native tool calls for read_graph, assess_root when available, and graph_action; XML/JSON text is not executed. Then commit the draft; an empty plan requires a valid open or running Step. For truncated tool calls, reissue complete arguments. The original deadline still applies. If unable to proceed, return accepted:false with a reason.", nil
	}
	if p.runtime.roles.curation != nil {
		return "This curation has no committed receipt. Read the supplied evidence and submit graph_action curate using native tool calls; XML/JSON text is not executed. The original input boundary and deadline still apply. If unable to proceed, return accepted:false with a reason.", nil
	}
	return "Continue the unfinished work in this same execution. Tools are enabled. The original task deadline still applies. Use completed only when the assigned task is finished; otherwise continue working or report incomplete with the remaining work and blocker.", nil
}

func (p *sessionPhases) onTurnEnd(turnCtx context.Context, l *agent.Loop, m agent.Message) (context.Context, string, error) {
	j, state, ctx := p.runtime.job, &p.runtime.state, p.ctx
	save := p.runtime.save

	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if p.runtime.roles.graph.Conflict != "" {
		return nil, "", errors.New(p.runtime.roles.graph.Conflict)
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
	turn := phase.Turn{
		Control: controlJob(j), NativeControl: p.runtime.roles.decision != nil || p.runtime.roles.curation != nil,
		HasCalls: hasCalls, Truncated: truncated(m),
	}
	var problem *outputFailure
	if turn.NativeControl {
		if !turn.Truncated && !hasCalls {
			parsed, err := parseOutput(j, false, m.Text())
			turn.Rejected = err == nil && parsed.Kind == "rejected"
		}
	} else {
		problem = outputProblem(j, l.Concluding, m)
		turn.InvalidResult = problem != nil
		if problem == nil {
			parsed, err := parseOutput(j, l.Concluding, m.Text())
			if err != nil {
				return nil, "", err
			}
			turn.Continue = parsed.Outcome == "continue"
		}
		turn.ConcludeDue = p.shouldConclude()
	}
	action := state.phaseState().AfterTurn(turn)
	switch action {
	case phase.Conclude, phase.ConcludeAndRepair:
		next, err := p.startConclusion()
		if err != nil {
			return nil, "", err
		}
		if action == phase.ConcludeAndRepair {
			instruction, err := p.startRepair(next, problem)
			return next, instruction, err
		}
		return next, l.ConclusionPrompt, nil
	case phase.Repair:
		instruction, err := p.startRepair(turnCtx, problem)
		return turnCtx, instruction, err
	case phase.Continue:
		instruction, err := p.continueExecution()
		return turnCtx, instruction, err
	}
	return turnCtx, "", nil
}
