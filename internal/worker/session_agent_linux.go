//go:build linux

package worker

import (
	"context"
	"errors"
	"os"
	"pwnmesh/internal/agent"
	"pwnmesh/internal/protocol"
	eventlog "pwnmesh/internal/worker/internal/journal"
	"pwnmesh/internal/worker/internal/phase"
	"strconv"
)

func (runtime *sessionRuntime) runAgent(ctx context.Context, retained eventlog.Checkpoint) (Result, error) {
	j, o, state := runtime.job, runtime.options, &runtime.state
	emit := runtime.emit
	var recall *workerRecall
	if j.Kind == "explore" && j.GraphRPC && j.Intent != nil {
		recall = restoreWorkerRecall(ctx, j, o.RunDir, retained)
		emit = func(event agent.Event) {
			runtime.emit(event)
			if err := recall.observe(event); err != nil {
				recall.Unavailable = "Some observations exceeded the bounded index; inspect the committed trace."
				recall.projection = traceProjector{Own: true}
			}
		}
	}
	finish := func(r Result) (Result, error) { return runtime.finish(ctx, r) }
	var err error
	o = contextOptions(o)
	l := &agent.Loop{Provider: o.Provider, Tools: runtime.roles.tools, History: state.History, TaskPrompt: state.TaskPrompt, Emit: emit,
		Checkpoint: state.ContextCheckpoint, SaveState: runtime.saveAgentState,
		ContextBytes: o.ContextBytes, ContextTokens: o.ContextTokens, ContextTargetTokens: o.ContextTargetTokens,
		ObserveRequests: true, ContextData: state.ExecuteUpdates.contextData()}
	if recall != nil {
		l.ContextDataPrefixes = []string{recallPrefix}
	}
	runtime.roles.bindLoop(l)

	runtime.options, runtime.loop = o, l
	runtime.applyPhase()
	phases := &sessionPhases{runtime: runtime, ctx: ctx, phaseCtx: ctx, endCancel: func() {}}
	defer func() { phases.endCancel() }()
	l.BeforeRequest = func(ctx context.Context, loop *agent.Loop) (context.Context, error) {
		next, err := phases.beforeRequest(ctx, loop)
		if err == nil && recall != nil {
			recall.pin(loop)
		}
		return next, err
	}
	l.OnTurnEnd = phases.onTurnEnd
	runCtx, prompt := ctx, ""
	if state.Concluding {
		runCtx, err = phases.startConclusion()
		if err != nil {
			return finish(Result{Type: "result", Status: "failed", Conclude: true, Error: err.Error()})
		}
		if !containsInstruction(l.History, state.ConclusionPrompt) {
			prompt = state.ConclusionPrompt
		}
	}
	if protocol.ControlJob(j) && (j.Budget.Timeout > 0 || !state.ReasonDeadline.IsZero()) {
		state.setPhaseState(state.phaseState().BoundControl())
		remaining := state.ReasonDeadline.Sub(o.Now())
		reasonCtx, reasonCancel := context.WithTimeout(ctx, remaining)
		defer reasonCancel()
		runCtx = reasonCtx
		phases.phaseCtx = reasonCtx
	}
	repairRecovery := state.phaseState().ResumeRepair(containsInstruction(l.History, state.RepairPrompt), runtime.infrastructureResume)
	if repairRecovery != phase.NoRepairRecovery {
		switch repairRecovery {
		case phase.ReuseRepairRequest:
			// Transport recovery continues the unanswered request without buying
			// or consuming another JSON-format repair attempt.
			prompt = ""
		case phase.RetryRepairRequest:
			// The request may have been sent before a crash. Do not reset or
			// replay that attempt for free; use only a remaining repair slot.
			prompt, err = phases.startRepair(runCtx, &outputFailure{Reason: "interrupted_repair", Detail: "the previous repair request has no durable response"})
		case phase.PrepareRepairRequest:
			err = phases.prepareRepairHistory()
			prompt = state.RepairPrompt
		}
		if err != nil {
			return finish(Result{Type: "result", Status: "failed", Conclude: state.Concluding, Error: err.Error()})
		}
	}
	if len(l.History) == 0 {
		if phases.shouldConclude() {
			runCtx, err = phases.startConclusion()
			if err != nil {
				return Result{}, err
			}
		}
		if state.Concluding {
			prompt = state.ConclusionPrompt
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
				return finish(Result{Type: "result", Status: "failed", Text: last.Text(), Conclude: state.Concluding, Error: err.Error()})
			}
			if prompt == "" {
				return finish(Result{Type: "result", Status: "success", Text: last.Text(), Conclude: state.Concluding})
			}
		}
	}
	// Resuming a transcript is itself a boundary. An expired exploration
	// budget must not buy another unrestricted model/tool turn after restart.
	if len(l.History) > 0 && phases.shouldConclude() {
		runCtx, err = phases.startConclusion()
		if err == nil {
			prompt = state.ConclusionPrompt
		}
		if err != nil {
			return Result{}, err
		}
	}
	if runtime.resuming && runtime.roles.decision != nil {
		// Saved tool results describe a previous private draft, not a plan that
		// survived the restart. Keep the immutable job and its original budget.
		if err := l.RepairHistory(); err != nil {
			return Result{}, err
		}
		if err := l.AppendInstruction("The uncommitted decision draft was discarded on recovery. Read the current overview and affected facts, then restage the whole plan. Old $aliases, draft receipts and completion_assessment reuse are invalid. Completion now requires preview and review of completion_review in a subsequent model turn before commit; ordinary plans can commit directly."); err != nil {
			return Result{}, err
		}
	}
	if runtime.resuming && recall != nil && recall.contextData() != "" {
		// Settle incomplete tool groups before appending the bounded recovery
		// notice. Rebuilding the index reads only the original saved prefix.
		if err := l.RepairHistory(); err != nil {
			return Result{}, err
		}
		if err := l.AppendInstruction(recall.contextData()); err != nil {
			return Result{}, err
		}
	}
	text, runErr := l.Run(runCtx, prompt)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	r := Result{Type: "result", Status: "success", Text: text, Conclude: state.Concluding}
	if _, committed := runtime.roles.stopResult(); runErr == nil && !committed {
		if last, ok := lastAssistant(l.History); ok {
			if problem := outputProblem(j, state.Concluding, last); problem != nil {
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
	if runtime.roles.graph.Conflict != "" && runtime.roles.curation != nil && !runtime.roles.curation.committed {
		// Curate owns an immutable boundary and cannot refresh its evidence in
		// place. Batch planners instead discard and rebuild their private draft.
		r.Status, r.FailureKind, r.Error, r.Retryable = "failed", "state_changed", runtime.roles.graph.Conflict, false
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

// Resolve explicit options before environment defaults for every Agent session.
func contextOptions(o Options) Options {
	if o.ContextBytes <= 0 {
		o.ContextBytes = envInt("PWNMESH_CONTEXT_BYTES", DefaultContextBytes)
	}
	if o.ContextTokens <= 0 {
		o.ContextTokens = envInt("PWNMESH_CONTEXT_TOKENS", DefaultContextTokens)
	}
	if o.ContextTargetTokens <= 0 {
		o.ContextTargetTokens = envInt("PWNMESH_CONTEXT_TARGET_TOKENS", DefaultContextTargetTokens)
	}
	return o
}
