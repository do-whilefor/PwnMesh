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
	"time"
)

// sessionRuntime owns the durable session, journal and live loop. It is private
// to one locked run; no runtime state is shared across Worker invocations.
type sessionRuntime struct {
	job                  Job
	options              Options
	state                session
	journal              *eventJournal
	encoder              *json.Encoder
	loop                 *agent.Loop
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
	if j.Budget.Timeout > 0 {
		state.ExecutionDeadline = state.StartedAt.Add(time.Duration(j.Budget.Timeout) * time.Second)
	}
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
	var checkpoint *journalCheckpoint
	if resuming {
		checkpoint = &state.Log
	}
	journal, err := openJournal(o.RunDir, checkpoint)
	if err != nil {
		return nil, err
	}

	runtime := &sessionRuntime{job: j, options: o, state: state, journal: journal, encoder: json.NewEncoder(o.Output), resuming: resuming}
	runtime.options.decisionEmit = runtime.emit
	return runtime, nil
}

func (r *sessionRuntime) emit(e agent.Event) {
	state, journal, enc := &r.state, r.journal, r.encoder
	if state.Repairing && e.Type == "message_end" && e.Message != nil && e.Message.Role == "assistant" {
		state.RepairPending = false
	}
	if r.logErr == nil {
		r.logErr = journal.append(e)
	}
	if r.logErr == nil {
		r.logErr = enc.Encode(e)
	}
}

func (r *sessionRuntime) save(history []agent.Message) error {
	state, journal, o, l := &r.state, r.journal, r.options, r.loop
	if r.logErr != nil {
		return r.logErr
	}
	before := *state
	state.History = history
	progress, err := state.ToolProgress.settle(history)
	if err != nil {
		*state = before
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
	r.StateVersion = state.GraphVersion
	if controlJob(j) {
		metrics := journal.metrics.finish(j, r, state.StartedAt, o.Now())
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
