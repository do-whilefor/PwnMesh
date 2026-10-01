//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"pwnmesh/internal/agent"
	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/config"
	"pwnmesh/internal/process"
	"strconv"
	"time"
)

type Options struct {
	Provider            agent.Provider
	Tools               []agent.Tool
	RunDir              string
	Output              io.Writer
	Now                 func() time.Time
	SoftStop            <-chan struct{}
	ContextBytes        int
	ContextTokens       int
	ContextTargetTokens int
	GraphVersion        *string
	ReplanShadow        bool
	decision            *decisionDraft
	curation            *curationCommit
	stepFinish          *stepFinish
	decisionEmit        agent.Emit
	decisionConflict    *string
	graphRequest        func(context.Context, GraphRequest) (string, error)
	graphDeadline       time.Time
	graphProvider       func(string) (agent.Provider, error)
}

func Execute(ctx context.Context, jobPath string, output io.Writer) error {
	raw, err := os.ReadFile(jobPath)
	if err != nil {
		return err
	}
	var j Job
	if err = json.Unmarshal(raw, &j); err != nil {
		return err
	}
	_, err = Run(ctx, j, Options{RunDir: filepath.Dir(jobPath), Output: output})
	return err
}

func validateWorkerProtocol(j Job) error {
	if j.Graph.Project.OrchestrationVersion != 1 || j.ResultContractVersion != 2 || !j.GraphRPC {
		return errors.New("worker requires orchestration version 1 with graph RPC and result protocol 2")
	}
	if j.WorkerType != "" && j.WorkerType != "go" {
		return errors.New("worker requires the go backend")
	}
	if j.Kind != "reason" && j.Kind != "curate" && j.Kind != "explore" {
		return errors.New("invalid job kind")
	}
	if j.Kind == "reason" && (j.Decision == nil || j.Decision.Version != 2) {
		return errors.New("reason requires decision protocol 2")
	}
	return nil
}

// runSession persists tool calls before executing their side effects. A task budget
// requests conclusion only at a settled turn; cancellation never restarts it.
func runSession(parent context.Context, j Job, o Options) (Result, error) {
	if err := parent.Err(); err != nil {
		return Result{}, err
	}
	if j.RunID == "" {
		return Result{}, errors.New("job requires run_id")
	}
	if j.PreviousRunID == j.RunID || j.Graph.Project.ID == "" {
		return Result{}, errors.New("job requires a project and a distinct previous_run_id")
	}
	if err := validateWorkerProtocol(j); err != nil {
		return Result{}, err
	}
	if j.Repair != nil {
		if j.Kind != "explore" {
			return Result{}, errors.New("repair requires an orchestration execute job with evidence protocol 2")
		}
		if err := artifactcheck.Validate(*j.Repair); err != nil {
			return Result{}, err
		}
	}
	if j.Kind == "curate" {
		if err := validateCuratorInput(j); err != nil {
			return Result{}, err
		}
	}
	if !controlJob(j) && j.Intent == nil {
		return Result{}, errors.New("job requires an intent")
	}
	if j.Budget.Timeout < 0 || j.Budget.ConcludeTimeout < 0 {
		return Result{}, errors.New("task budgets must not be negative")
	}
	if !controlJob(j) && j.Budget.ConcludeTimeout <= 0 {
		return Result{}, errors.New("conclude timeout must be positive")
	}
	if o.RunDir == "" || j.Workspace == "" {
		return Result{}, errors.New("job requires workspace and execution directory")
	}
	var err error
	o.RunDir, err = filepath.Abs(o.RunDir)
	if err != nil {
		return Result{}, err
	}
	j.Workspace, err = filepath.Abs(j.Workspace)
	if err != nil {
		return Result{}, err
	}
	if err = os.MkdirAll(o.RunDir, 0700); err != nil {
		return Result{}, err
	}
	unlock, err := process.Lock(o.RunDir)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	if err = process.CheckLaunch(o.RunDir); err != nil {
		return Result{}, err
	}
	if process.Cancelled(o.RunDir) {
		return Result{}, context.Canceled
	}
	if err = process.RegisterWorker(o.RunDir); err != nil {
		return Result{}, err
	}
	defer os.Remove(filepath.Join(o.RunDir, "worker.pid"))
	defer process.KillGroups(o.RunDir)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer func() {
		if ctx.Err() != nil && !errors.Is(context.Cause(parent), ErrInterrupted) {
			_ = os.WriteFile(filepath.Join(o.RunDir, "cancelled"), []byte("hard stop\n"), 0600)
		}
	}()
	// The marker also covers cancel arriving before a container exec starts.
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if process.Cancelled(o.RunDir) {
					cancel()
					return
				}
			}
		}
	}()
	if process.Cancelled(o.RunDir) {
		return Result{}, context.Canceled
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Output == nil {
		o.Output = io.Discard
	}
	identity, err := identityFor(j, o.RunDir)
	if err != nil {
		return Result{}, err
	}
	state := session{SchemaVersion: sessionSchemaVersion, Identity: identity, RunID: j.RunID, Kind: j.Kind, StartedAt: o.Now()}
	if j.Kind == "reason" && j.Decision != nil {
		state.GraphVersion = j.Decision.StateVersion
	}
	if j.Kind == "curate" {
		state.GraphVersion = j.curationVersion()
	}
	if j.Budget.Timeout > 0 {
		state.ExecutionDeadline = state.StartedAt.Add(time.Duration(j.Budget.Timeout) * time.Second)
	}
	statePath := filepath.Join(o.RunDir, "session.json")
	resuming := false
	if previous, readErr := os.ReadFile(statePath); readErr == nil {
		if err = json.Unmarshal(previous, &state); err != nil {
			return Result{}, fmt.Errorf("invalid saved session: %w", err)
		}
		if err = state.validate(identity); err != nil {
			return Result{}, err
		}
		resuming = true
	} else if !os.IsNotExist(readErr) {
		return Result{}, readErr
	}
	if err := validateExecuteUpdateState(j, &state); err != nil {
		return Result{}, err
	}
	var checkpoint *journalCheckpoint
	if resuming {
		checkpoint = &state.Log
	}
	journal, err := openJournal(o.RunDir, checkpoint)
	if err != nil {
		return Result{}, err
	}
	defer journal.file.Close()
	enc := json.NewEncoder(o.Output)
	var logErr error
	emit := func(e agent.Event) {
		if state.Repairing && e.Type == "message_end" && e.Message != nil && e.Message.Role == "assistant" {
			state.RepairPending = false
		}
		if logErr == nil {
			logErr = journal.append(e)
		}
		if logErr == nil {
			logErr = enc.Encode(e)
		}
	}
	o.decisionEmit = emit
	var l *agent.Loop
	save := func(history []agent.Message) error {
		if logErr != nil {
			return logErr
		}
		before := state
		state.History = history
		progress, err := state.ToolProgress.settle(history)
		if err != nil {
			state = before
			return err
		}
		if progress {
			state.ContinuationCount = 0
		}
		if l != nil {
			state.TaskPrompt = l.TaskPrompt
			state.ConclusionPrompt = l.ConclusionPrompt
			state.RepairPrompt = l.RepairPrompt
			state.ContextCheckpoint = l.Checkpoint
		}
		if err := state.save(o.RunDir, journal); err != nil {
			state = before
			return err
		}
		return nil
	}
	finish := func(r Result) (Result, error) {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if process.Cancelled(o.RunDir) {
			return Result{}, context.Canceled
		}
		r = checkedResult(j, r)
		if j.Repair != nil && r.Status == "success" {
			prepared, checkErr := finishRepair(ctx, j, o.RunDir, r, state.RepairCheck, o.Now())
			if checkErr != nil {
				r.Status, r.FailureKind, r.Error = "failed", "repair_acceptance", checkErr.Error()
			} else {
				r = prepared
			}
		}
		if r.Status == "success" {
			prepared, err := prepareFinalEvidence(ctx, j, o.RunDir, r, state.ConclusionEvidence)
			if err != nil {
				r.Status, r.FailureKind, r.Error = "failed", "result_evidence", err.Error()
			} else {
				r = prepared
			}
		}
		if r.Status == "failed" && r.FailureKind == "" {
			r.FailureKind = "execution"
		}
		r.StateVersion = state.GraphVersion
		if controlJob(j) {
			metrics := journal.metrics.finish(j, r, state.StartedAt, o.Now())
			metrics.Replan = state.Replan
			if o.curation != nil && o.curation.committed {
				metrics.Committed, metrics.Outcome = true, "curation_committed"
			}
			r.Metrics = &metrics
		}
		if err := journal.append(r); err != nil {
			return r, err
		}
		state.Result = &r
		if err := save(state.History); err != nil {
			return r, err
		}
		return r, enc.Encode(r)
	}
	if j.Repair != nil {
		check, _, checkErr := inspectRepair(ctx, j)
		if checkErr != nil {
			return finish(Result{Type: "result", Status: "failed", FailureKind: "repair_check", Error: checkErr.Error()})
		}
		// A resumed successful result can only reuse its exact inspected bytes.
		if state.Result != nil && state.Result.Status == "success" && state.Result.RepairCheck != nil && state.Result.RepairCheck.SHA256 != check.SHA256 {
			return finish(Result{Type: "result", Status: "failed", FailureKind: "repair_stale", Error: "repair target changed after acceptance; main Agent must reassess", RepairCheck: &check})
		}
		if state.RepairCheck == nil {
			state.RepairCheck = &check
		}
		if check.Outcome == "stale" {
			return finish(Result{Type: "result", Status: "failed", FailureKind: "repair_stale", Error: "repair target SHA-256 changed and required checks still fail; main Agent must reassess", RepairCheck: &check})
		}
		if check.Satisfied && (state.Result == nil || state.Result.Retryable) {
			// Preserve the first execution decision across infrastructure recovery.
			// A prior model/tool run that fixed the target is never relabeled no-op.
			r, err := finishRepair(ctx, j, o.RunDir, Result{}, state.RepairCheck, o.Now())
			if err != nil {
				return finish(Result{Type: "result", Status: "failed", FailureKind: "repair_acceptance", Error: err.Error()})
			}
			return finish(r)
		}
	}
	infrastructureResume := false
	resumeFailureCause := ""
	if state.Result != nil {
		last, ok := lastAssistant(state.History)
		parsed, parseErr := parseOutput(j, state.Result.Conclude, state.Result.Text)
		if state.Result.Retryable {
			infrastructureResume = true
			resumeFailureCause = infrastructureCauseKind(state.Result.FailureKind)
			state.Result = nil
		} else if state.Result.Status == "success" && (parseErr != nil || parsed.Outcome == "continue" || (ok && truncated(last))) {
			if !ok {
				return finish(checkedResult(j, *state.Result))
			}
			state.Result = nil
		} else if state.Result.Status == "success" && parsed.Outcome == "incomplete" {
			return finish(checkedResult(j, *state.Result))
		} else {
			return *state.Result, enc.Encode(state.Result)
		}
	}
	if resuming {
		if state.RecoveryCount >= maxRunRecoveries {
			return finish(Result{Type: "result", Status: "failed", Conclude: state.Concluding, FailureKind: "recovery_exhausted", FailureCause: resumeFailureCause, Error: "same-run infrastructure recovery exhausted after 2 attempts"})
		}
		state.RecoveryCount++
		if state.ContextCheckpoint == nil && (journal.lastSequence > 0 || journal.lastCompaction > 0) {
			state.ContextCheckpoint = &agent.ContextCheckpoint{Version: agent.ContextCheckpointVersion}
		}
		if state.ContextCheckpoint != nil && state.ContextCheckpoint.LastSequence < journal.lastSequence {
			state.ContextCheckpoint.LastSequence = journal.lastSequence
		}
		if state.ContextCheckpoint != nil && state.ContextCheckpoint.CompactionCount < journal.lastCompaction {
			state.ContextCheckpoint.CompactionCount = journal.lastCompaction
		}
		emit(agent.Event{Type: "recovery", Text: fmt.Sprintf("same run recovery %d/%d; retained %d uncommitted log bytes; incomplete tail archive: %s", state.RecoveryCount, maxRunRecoveries, journal.uncommitted, journal.partialArchive)})
	}
	// The identity, budget and consumed recovery allowance precede any request.
	if err := save(state.History); err != nil {
		return Result{}, err
	}
	if o.Provider == nil {
		p, err := modelForJob(j)
		if err != nil {
			return Result{}, err
		}
		o.Provider = p
	}
	o.GraphVersion = &state.GraphVersion
	o.graphDeadline = state.ExecutionDeadline
	o.decisionConflict = &state.DecisionConflict
	if err := ConfigureRuntimeTools(j, &o); err != nil {
		return Result{}, err
	}
	if o.stepFinish != nil {
		if err := o.stepFinish.recover(ctx); err != nil {
			return Result{}, err
		}
		if text, done := o.stepFinish.result(); done {
			return finish(Result{Type: "result", Status: "success", Text: text})
		}
	}
	if o.curation != nil {
		if _, err := o.curation.recover(ctx); err != nil {
			return Result{}, err
		}
		if o.curation.committed {
			return finish(Result{Type: "result", Status: "success", Text: committedCurationText})
		}
		state.Repairing, state.RepairPending = false, false
		state.RepairPrompt = ""
	}
	if o.decision != nil {
		if orchestrationJob(j) && j.Decision != nil {
			o.decision.assessment = j.Decision.CompletionAssessment
		}
		if _, err := o.decision.recover(ctx); err != nil {
			return Result{}, err
		}
		if o.decision.committed {
			return finish(Result{Type: "result", Status: "success", Text: committedDecisionText})
		}
		if state.DecisionConflict != "" {
			return finish(Result{Type: "result", Status: "failed", FailureKind: "state_changed", Error: state.DecisionConflict})
		}
		if resuming {
			o.decision.invalidate()
		}
		// A batch planner succeeds through a tool receipt, never repaired final
		// JSON. Resume its uncommitted planning phase without restoring consumed
		// repair/continuation allowances or deadlines. Execute repair is unchanged.
		state.Repairing, state.RepairPending = false, false
		state.RepairPrompt = ""
	}
	if o.ContextBytes <= 0 {
		o.ContextBytes = envInt("PWNMESH_CONTEXT_BYTES", DefaultContextBytes)
	}
	if o.ContextTokens <= 0 {
		o.ContextTokens = envInt("PWNMESH_CONTEXT_TOKENS", DefaultContextTokens)
	}
	if o.ContextTargetTokens <= 0 {
		o.ContextTargetTokens = envInt("PWNMESH_CONTEXT_TARGET_TOKENS", DefaultContextTargetTokens)
	}
	l = &agent.Loop{Provider: o.Provider, Tools: o.Tools, History: state.History, Concluding: state.Concluding, Repairing: state.Repairing, RepairPrompt: state.RepairPrompt, Emit: emit, Checkpoint: state.ContextCheckpoint, SaveState: func(history []agent.Message, _ *agent.ContextCheckpoint) error {
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
	var endCancel context.CancelFunc = func() {}
	defer func() { endCancel() }()
	runCtx := ctx
	phaseCtx := ctx
	prompt := ""
	startConclusion := func() (context.Context, error) {
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
				l.TaskPrompt, err = Prompt(j, false, o.RunDir)
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
		endCancel = cancel
		phaseCtx = next
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
	if state.Concluding {
		runCtx, err = startConclusion()
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
		phaseCtx = reasonCtx
	}
	shouldConclude := func() bool {
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
	l.BeforeRequest = func(turnCtx context.Context, loop *agent.Loop) (context.Context, error) {
		if !loop.Concluding && !loop.Repairing && !shouldConclude() {
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
			next, err := startConclusion()
			if err != nil {
				return err
			}
			turnCtx = next
			return loop.AppendInstruction(loop.ConclusionPrompt)
		}
		if shouldConclude() {
			if err := concludeAtBoundary(); err != nil {
				return nil, err
			}
		}
		if err := refreshExecutionUpdates(turnCtx, j, o.graphRequest, &state, loop, save); err != nil {
			return nil, err
		}
		// A slow graph read cannot buy another exploration turn. Enter the same
		// bounded conclusion used by settled tool turns, then record deferral.
		if shouldConclude() {
			if err := concludeAtBoundary(); err != nil {
				return nil, err
			}
			if err := refreshExecutionUpdates(turnCtx, j, o.graphRequest, &state, loop, save); err != nil {
				return nil, err
			}
		}
		return turnCtx, nil
	}
	prepareRepairHistory := func() error {
		if err := l.RepairHistory(); err != nil {
			return err
		}
		if l.Concluding && !containsInstruction(l.History, l.ConclusionPrompt) {
			return l.AppendInstruction(l.ConclusionPrompt)
		}
		return nil
	}
	startRepair := func(turnCtx context.Context, problem *outputFailure) (string, error) {
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
		if err = prepareRepairHistory(); err != nil {
			return "", err
		}
		return instruction, nil
	}
	continueExecution := func() (string, error) {
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
			return "This Decide has no committed receipt. Continue planning with read_graph and graph_action, then commit the draft; an empty plan requires a valid open or running Step. Final JSON cannot publish a plan. For truncated tool calls, reissue complete arguments. The original deadline still applies. If unable to proceed, return accepted:false with a reason.", nil
		}
		if o.curation != nil {
			return "This curation has no committed receipt. Read the supplied evidence and submit graph_action curate. Final JSON cannot commit curation. The original input boundary and deadline still apply. If unable to proceed, return accepted:false with a reason.", nil
		}
		return "Continue the unfinished work in this same execution. Tools are enabled. The original task deadline still applies. Use completed only when the assigned task is finished; otherwise continue working or report incomplete with the remaining work and blocker.", nil
	}
	l.OnTurnEnd = func(turnCtx context.Context, l *agent.Loop, m agent.Message) (context.Context, string, error) {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if state.DecisionConflict != "" {
			return nil, "", errors.New(state.DecisionConflict)
		}
		hasCalls := hasToolCalls(m)
		if hasCalls && !l.Concluding && !l.Repairing && !truncated(m) && !shouldConclude() {
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
			instruction, err := continueExecution()
			return turnCtx, instruction, err
		}
		problem := outputProblem(j, l.Concluding, m)
		needsResult := !hasCalls || truncated(m) || l.Concluding || l.Repairing
		if !l.Concluding && !controlJob(j) && shouldConclude() {
			next, err := startConclusion()
			if err != nil {
				return nil, "", err
			}
			if needsResult && problem != nil {
				instruction, err := startRepair(next, problem)
				return next, instruction, err
			}
			return next, l.ConclusionPrompt, nil
		}
		if needsResult && problem != nil {
			instruction, err := startRepair(turnCtx, problem)
			return turnCtx, instruction, err
		}
		if needsResult {
			parsed, err := parseOutput(j, l.Concluding, m.Text())
			if err != nil {
				return nil, "", err
			}
			if parsed.Outcome == "continue" {
				instruction, err := continueExecution()
				return turnCtx, instruction, err
			}
		}
		return turnCtx, "", nil
	}
	if state.Repairing && state.RepairPending {
		if containsInstruction(l.History, l.RepairPrompt) && infrastructureResume {
			// Transport recovery continues the unanswered request without buying
			// or consuming another JSON-format repair attempt.
			prompt = ""
		} else if containsInstruction(l.History, l.RepairPrompt) {
			// The request may have been sent before a crash. Do not reset or
			// replay that attempt for free; use only a remaining repair slot.
			prompt, err = startRepair(runCtx, &outputFailure{Reason: "interrupted_repair", Detail: "the previous repair request has no durable response"})
		} else {
			err = prepareRepairHistory()
			prompt = l.RepairPrompt
		}
		if err != nil {
			return finish(Result{Type: "result", Status: "failed", Conclude: l.Concluding, Error: err.Error()})
		}
	}
	// The experiment runs once, before planning, with the same absolute task
	// deadline. A crash cannot buy another check or carry its private transcript
	// into Decide. Even a valid keep does not skip the normal planner.
	if j.Kind == "reason" {
		if state.Replan != nil && state.Replan.Status == "running" {
			state.Replan.Status, state.Replan.Fallback = "interrupted", "decide"
		} else if !resuming && (o.ReplanShadow || config.Getenv("PWNMESH_REPLAN_SHADOW") == "1") {
			state.Replan = &ReplanObservation{Mode: "shadow", Status: "skipped", Fallback: "decide"}
			if j.Decision != nil {
				state.Replan.StateVersion, state.Replan.Generation = j.Decision.StateVersion, j.Decision.Generation
				state.Replan.FromRevision, state.Replan.ToRevision = j.Decision.FromRevision, j.Decision.ToRevision
			}
			if j.Decision != nil && j.Decision.Mode == "changes" && (j.State != nil || j.InputSnapshot != nil) && j.openCount() > 0 {
				state.Replan.Status = "running"
				if err = save(l.History); err != nil {
					return Result{}, err
				}
				if err = runReplanCheck(runCtx, j, o, state.Replan, emit, func() error { return save(l.History) }); err != nil {
					return Result{}, err
				}
			}
		}
		if state.Replan != nil {
			raw, _ := json.Marshal(state.Replan)
			emit(agent.Event{Type: "replan_observation", Text: string(raw)})
			if err = save(l.History); err != nil {
				return Result{}, err
			}
		}
	}
	if len(l.History) == 0 {
		if shouldConclude() {
			runCtx, err = startConclusion()
			if err != nil {
				return Result{}, err
			}
		}
		if l.Concluding {
			prompt = l.ConclusionPrompt
		} else {
			prompt, err = Prompt(j, false, o.RunDir)
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
	if len(l.History) > 0 && shouldConclude() {
		runCtx, err = startConclusion()
		if err == nil {
			prompt = l.ConclusionPrompt
		}
		if err != nil {
			return Result{}, err
		}
	}
	if resuming && o.decision != nil {
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
		r.FailureKind, r.Retryable = classifyFailure(runErr, phaseCtx)
		if r.FailureKind == "budget_exhausted" {
			r.FailureCause = infrastructureFailureCause(runErr)
		}
		if r.Retryable && state.RecoveryCount >= maxRunRecoveries {
			r.Retryable = false
			r.FailureCause = infrastructureCauseKind(r.FailureKind)
			r.FailureKind = "recovery_exhausted"
		}
	}
	if state.DecisionConflict != "" && ((o.decision != nil && !o.decision.committed) || (o.curation != nil && !o.curation.committed)) {
		// A rejected transaction has no uncertain writes to recover. Let the
		// scheduler coalesce changed input into a new run with its own budget,
		// instead of spending this run's remainder on repeated model refreshes.
		r.Status, r.FailureKind, r.Error, r.Retryable = "failed", "state_changed", state.DecisionConflict, false
		r.FailureCause = ""
	}
	if logErr != nil {
		return r, logErr
	}
	return finish(r)
}

func envInt(key string, fallback int) int {
	n, err := strconv.Atoi(config.Getenv(key))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
