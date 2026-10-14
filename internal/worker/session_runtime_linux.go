//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"pwnmesh/internal/agent"
	"pwnmesh/internal/process"
	"pwnmesh/internal/protocol"
	eventlog "pwnmesh/internal/worker/internal/journal"
	"pwnmesh/internal/worker/internal/phase"
	"time"
)

// sessionRuntime owns the durable session, journal and live loop. It is private
// to one locked run; no runtime state is shared across Worker invocations.
type sessionRuntime struct {
	job                  Job
	options              Options
	state                session
	journal              *eventlog.Log
	metrics              decisionMetrics
	encoder              *json.Encoder
	loop                 *agent.Loop
	roles                *roleRuntime
	logErr               error
	resuming             bool
	infrastructureResume bool
}

func restoreSession(j Job, o Options) (*sessionRuntime, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Output == nil {
		o.Output = io.Discard
	}
	identity, err := identityFor(j, o.RunDir)
	if err != nil {
		return nil, err
	}
	state := session{SchemaVersion: sessionSchemaVersion, Identity: identity, RunID: j.RunID, Kind: j.Kind, StartedAt: o.Now()}
	if j.Kind == "reason" && j.Decision != nil {
		state.GraphVersion = j.Decision.StateVersion
	}
	if j.Kind == "curate" {
		state.GraphVersion = j.CurationVersion()
	}
	state.setPhaseState(phase.New(state.StartedAt, phaseBudget(j.Budget)))
	statePath := filepath.Join(o.RunDir, "session.json")
	resuming := false
	if previous, readErr := os.ReadFile(statePath); readErr == nil {
		if err = json.Unmarshal(previous, &state); err != nil {
			return nil, fmt.Errorf("invalid saved session: %w", err)
		}
		if err = state.validate(identity); err != nil {
			return nil, err
		}
		resuming = true
	} else if !os.IsNotExist(readErr) {
		return nil, readErr
	}
	if err := validateExecuteUpdateState(j, &state); err != nil {
		return nil, err
	}
	var checkpoint *eventlog.Checkpoint
	if resuming {
		checkpoint = &state.Log
	}
	metrics := newDecisionMetrics()
	journal, err := eventlog.Open(o.RunDir, checkpoint, metrics.observeRecovered)
	if err != nil {
		return nil, err
	}

	runtime := &sessionRuntime{job: j, options: o, state: state, journal: journal, metrics: metrics, encoder: json.NewEncoder(o.Output), resuming: resuming}
	return runtime, nil
}

func (r *sessionRuntime) emit(e agent.Event) {
	state, journal, enc := &r.state, r.journal, r.encoder
	if state.Repairing && e.Type == "message_end" && e.Message != nil && e.Message.Role == "assistant" {
		state.setPhaseState(state.phaseState().RepairResponded())
	}
	if r.logErr == nil {
		r.logErr = journal.Append(e)
		if r.logErr == nil {
			r.metrics.observe(e)
		}
	}
	if r.logErr == nil {
		r.logErr = enc.Encode(e)
	}
}

// save is used at runtime-owned boundaries; Agent callbacks supply their exact
// checkpoint through saveAgentState instead of reading a mutable Loop pointer.
func (r *sessionRuntime) save(history []agent.Message) error {
	checkpoint := r.state.ContextCheckpoint
	if r.loop != nil {
		checkpoint = r.loop.SnapshotCheckpoint()
	}
	return r.saveAgentState(history, checkpoint)
}

func (r *sessionRuntime) saveAgentState(history []agent.Message, checkpoint *agent.ContextCheckpoint) error {
	state, journal, o, l := &r.state, r.journal, r.options, r.loop
	if r.logErr != nil {
		return r.logErr
	}
	before := *state
	state.History, state.ContextCheckpoint = history, checkpoint
	if r.roles != nil {
		state.GraphVersion, state.DecisionConflict = r.roles.graph.Version, r.roles.graph.Conflict
	}
	progress, err := state.ToolProgress.settle(history)
	if err != nil {
		*state = before
		return err
	}
	if progress {
		state.setPhaseState(state.phaseState().SuccessfulProgress())
	}
	if l != nil {
		state.TaskPrompt = l.TaskPrompt
		state.ConclusionPrompt = l.ConclusionPrompt
		state.RepairPrompt = l.RepairPrompt
	}
	if err := state.save(o.RunDir, journal); err != nil {
		*state = before
		return err
	}
	return nil
}

func (runtime *sessionRuntime) finish(ctx context.Context, r Result) (Result, error) {
	j, o, state, journal, enc := runtime.job, runtime.options, &runtime.state, runtime.journal, runtime.encoder
	save := runtime.save
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
	if runtime.roles != nil {
		state.GraphVersion, state.DecisionConflict = runtime.roles.graph.Version, runtime.roles.graph.Conflict
	}
	r.StateVersion = state.GraphVersion
	if protocol.ControlJob(j) {
		metrics := runtime.metrics.finish(j, r, state.StartedAt, o.Now())
		if runtime.roles != nil && runtime.roles.curation != nil && runtime.roles.curation.committed {
			metrics.Committed, metrics.Outcome = true, "curation_committed"
		}
		r.Metrics = &metrics
	}
	if err := journal.Append(r); err != nil {
		return r, err
	}
	state.Result = &r
	if err := save(state.History); err != nil {
		return r, err
	}
	return r, enc.Encode(r)
}

// Graph updates enter the transcript only through the session persistence owner.
// A rejected save retains the old cursor and old pinned request data.
func (r *sessionRuntime) refreshExecutionUpdates(ctx context.Context) error {
	phase := ""
	if r.state.Concluding {
		phase = "conclude"
	}
	if r.state.Repairing {
		phase = "repair"
	}
	previous := r.state.ExecuteUpdates
	next, notice, err := readExecutionUpdates(ctx, r.job, r.roles.request, previous, phase)
	if err != nil || next == previous {
		return err
	}
	loop := r.loop
	previousData := loop.ContextData
	loop.ContextData, r.state.ExecuteUpdates = next.contextData(), next
	if notice != "" {
		next.NoticeSequence = loop.Checkpoint.LastSequence + 1
		if next.Review == notice {
			next.ReviewSequence = next.NoticeSequence
		}
		err = loop.AppendInstruction(notice)
	} else {
		err = r.save(loop.History)
	}
	if err != nil {
		r.state.ExecuteUpdates, loop.ContextData = previous, previousData
	}
	return err
}
