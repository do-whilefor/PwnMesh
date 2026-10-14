//go:build linux

package worker

import (
	"context"
	"errors"
	"os"
	"pwnmesh/internal/agent"
	"strconv"
)

func (runtime *sessionRuntime) runAgent(ctx context.Context) (Result, error) {
	j, o, state := runtime.job, runtime.options, &runtime.state
	save, emit := runtime.save, runtime.emit
	finish := func(r Result) (Result, error) { return runtime.finish(ctx, r) }
	var err error
	if o.ContextBytes <= 0 {
		o.ContextBytes = envInt("PWNMESH_CONTEXT_BYTES", DefaultContextBytes)
	}
	if o.ContextTokens <= 0 {
		o.ContextTokens = envInt("PWNMESH_CONTEXT_TOKENS", DefaultContextTokens)
	}
	if o.ContextTargetTokens <= 0 {
		o.ContextTargetTokens = envInt("PWNMESH_CONTEXT_TARGET_TOKENS", DefaultContextTargetTokens)
	}
	l := &agent.Loop{Provider: o.Provider, Tools: o.Tools, History: state.History, Concluding: state.Concluding, Repairing: state.Repairing, RepairPrompt: state.RepairPrompt, Emit: emit, Checkpoint: state.ContextCheckpoint, SaveState: func(history []agent.Message, _ *agent.ContextCheckpoint) error {
		return save(history)
	}, ContextBytes: o.ContextBytes, ContextTokens: o.ContextTokens, ContextTargetTokens: o.ContextTargetTokens, ObserveRequests: true, TaskPrompt: state.TaskPrompt, ConclusionPrompt: state.ConclusionPrompt, ContextData: state.ExecuteUpdates.contextData()}
	if o.decision != nil {
		l.StopResult = o.decision.result
	}
	if o.curation != nil {
		l.StopResult = o.curation.result
	}
	if o.stepFinish != nil {
		l.StopResult = o.stepFinish.result
		l.StopResultTools = []string{"finish_step"}
	}

	runtime.options, runtime.loop = o, l
	phases := &sessionPhases{runtime: runtime, ctx: ctx, phaseCtx: ctx, endCancel: func() {}}
	defer func() { phases.endCancel() }()
	l.BeforeRequest, l.OnTurnEnd = phases.beforeRequest, phases.onTurnEnd
	runCtx, prompt := ctx, ""
	if state.Concluding {
		runCtx, err = phases.startConclusion()
		if err != nil {
			return finish(Result{Type: "result", Status: "failed", Conclude: true, Error: err.Error()})
		}
		if !containsInstruction(l.History, l.ConclusionPrompt) {
			prompt = l.ConclusionPrompt
		}
	}
	if controlJob(j) && (j.Budget.Timeout > 0 || !state.ReasonDeadline.IsZero()) {
		if state.ReasonDeadline.IsZero() {
			state.ReasonDeadline = state.ExecutionDeadline
		}
		remaining := state.ReasonDeadline.Sub(o.Now())
		reasonCtx, reasonCancel := context.WithTimeout(ctx, remaining)
		defer reasonCancel()
		runCtx = reasonCtx
		phases.phaseCtx = reasonCtx
	}
	if state.Repairing && state.RepairPending {
		if containsInstruction(l.History, l.RepairPrompt) && runtime.infrastructureResume {
			// Transport recovery continues the unanswered request without buying
			// or consuming another JSON-format repair attempt.
			prompt = ""
		} else if containsInstruction(l.History, l.RepairPrompt) {
			// The request may have been sent before a crash. Do not reset or
			// replay that attempt for free; use only a remaining repair slot.
			prompt, err = phases.startRepair(runCtx, &outputFailure{Reason: "interrupted_repair", Detail: "the previous repair request has no durable response"})
		} else {
			err = phases.prepareRepairHistory()
			prompt = l.RepairPrompt
		}
		if err != nil {
			return finish(Result{Type: "result", Status: "failed", Conclude: l.Concluding, Error: err.Error()})
		}
	}
	if len(l.History) == 0 {
		if phases.shouldConclude() {
			runCtx, err = phases.startConclusion()
			if err != nil {
				return Result{}, err
			}
		}
		if l.Concluding {
			prompt = l.ConclusionPrompt
		} else {
			prompt, err = Prompt(j, false)
		}
		if err != nil {
			return Result{}, err
		}
	} else if last, ok := lastAssistant(l.History); ok && prompt == "" && !awaitingInstructionResponse(l.History) {
		if !hasToolCalls(last) || truncated(last) || state.Repairing {
			// A model turn may have been saved immediately before the worker crashed.
			runCtx, prompt, err = l.OnTurnEnd(runCtx, l, last)
			if err != nil {
				return finish(Result{Type: "result", Status: "failed", Text: last.Text(), Conclude: l.Concluding, Error: err.Error()})
			}
			if prompt == "" {
				return finish(Result{Type: "result", Status: "success", Text: last.Text(), Conclude: l.Concluding})
			}
		}
	}
	// Resuming a transcript is itself a boundary. An expired exploration
	// budget must not buy another unrestricted model/tool turn after restart.
	if len(l.History) > 0 && phases.shouldConclude() {
		runCtx, err = phases.startConclusion()
		if err == nil {
			prompt = l.ConclusionPrompt
		}
		if err != nil {
			return Result{}, err
		}
	}
	if runtime.resuming && o.decision != nil {
		// Saved tool results describe a previous private draft, not a plan that
		// survived the restart. Keep the immutable job and its original budget.
		if err := l.RepairHistory(); err != nil {
			return Result{}, err
		}
		if err := l.AppendInstruction("The uncommitted decision draft was discarded on recovery. Read the current overview and affected facts, then restage the whole plan. Old $aliases, draft receipts and completion_assessment reuse are invalid. Completion now requires preview and review of completion_review in a subsequent model turn before commit; ordinary plans can commit directly."); err != nil {
			return Result{}, err
		}
	}
	text, runErr := l.Run(runCtx, prompt)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	r := Result{Type: "result", Status: "success", Text: text, Conclude: l.Concluding}
	if runErr == nil && !(o.decision != nil && o.decision.committed) && !(o.curation != nil && o.curation.committed) && !(o.stepFinish != nil && o.stepFinish.text != "") {
		if last, ok := lastAssistant(l.History); ok {
			if problem := outputProblem(j, l.Concluding, last); problem != nil {
				runErr = problem
			}
		} else {
			runErr = errors.New("model returned no final message")
		}
	}
	if runErr != nil {
		r.Status = "failed"
		r.Error = runErr.Error()
		r.FailureKind, r.Retryable = classifyFailure(runErr, phases.phaseCtx)
		if r.FailureKind == "budget_exhausted" {
			r.FailureCause = infrastructureFailureCause(runErr)
		}
		if r.Retryable && state.RecoveryCount >= maxRunRecoveries {
			r.Retryable = false
			r.FailureCause = infrastructureCauseKind(r.FailureKind)
			r.FailureKind = "recovery_exhausted"
		}
	}
	if state.DecisionConflict != "" && o.curation != nil && !o.curation.committed {
		// Curate owns an immutable boundary and cannot refresh its evidence in
		// place. Batch planners instead discard and rebuild their private draft.
		r.Status, r.FailureKind, r.Error, r.Retryable = "failed", "state_changed", state.DecisionConflict, false
		r.FailureCause = ""
	}
	if runtime.logErr != nil {
		return r, runtime.logErr
	}
	return finish(r)
}
func envInt(key string, fallback int) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
