//go:build linux

package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"pwnmesh/internal/agent"
	board "pwnmesh/internal/board/model"
	eventlog "pwnmesh/internal/worker/internal/journal"
)

const sessionSchemaVersion = 2
const maxRunRecoveries = 2

func supportedSessionSchema(version int, checkpoint eventlog.Checkpoint) bool {
	// Version 1 never recorded crash-tail exclusions. Upgrade when saved;
	// older binaries reject version 2 instead of silently dropping provenance.
	return version == sessionSchemaVersion || version == 1 && len(checkpoint.Excluded) == 0
}

type executionIdentity struct {
	ProjectID       string           `json:"project_id"`
	StepID          string           `json:"step_id,omitempty"`
	RunID           string           `json:"run_id"`
	PreviousRunID   string           `json:"previous_run_id,omitempty"`
	Kind            string           `json:"kind"`
	Workspace       string           `json:"workspace"`
	RunDir          string           `json:"run_dir"`
	WorkspaceTarget string           `json:"workspace_target"`
	RunDirTarget    string           `json:"run_dir_target"`
	JobDigest       string           `json:"job_digest"`
	OriginalBudget  board.TaskBudget `json:"original_budget"`
}

// Paths are bound both as supplied (after Abs/Clean) and as resolved targets:
// changing a symlink cannot silently redirect a resumed execution.
func identityFor(j Job, runDir string) (executionIdentity, error) {
	w, err := filepath.EvalSymlinks(j.Workspace)
	if err != nil {
		return executionIdentity{}, fmt.Errorf("resolve workspace: %w", err)
	}
	r, err := filepath.EvalSymlinks(runDir)
	if err != nil {
		return executionIdentity{}, fmt.Errorf("resolve run directory: %w", err)
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return executionIdentity{}, err
	}
	sum := sha256.Sum256(raw)
	i := executionIdentity{ProjectID: j.Graph.Project.ID, RunID: j.RunID, PreviousRunID: j.PreviousRunID, Kind: j.Kind, Workspace: j.Workspace, RunDir: runDir, WorkspaceTarget: w, RunDirTarget: r, JobDigest: hex.EncodeToString(sum[:]), OriginalBudget: j.Budget}
	if j.Intent != nil {
		i.StepID = j.Intent.ID
	}
	return i, nil
}

type session struct {
	RepairCheck            *RepairCheck             `json:"repair_check,omitempty"`
	SchemaVersion          int                      `json:"schema_version"`
	Identity               executionIdentity        `json:"identity"`
	Log                    eventlog.Checkpoint      `json:"log_checkpoint"`
	ContextCheckpoint      *agent.ContextCheckpoint `json:"context_checkpoint,omitempty"`
	GraphVersion           string                   `json:"graph_version,omitempty"`
	DecisionConflict       string                   `json:"decision_conflict,omitempty"`
	ExecuteUpdates         *executeUpdateState      `json:"execute_updates,omitempty"`
	RecoveryCount          int                      `json:"recovery_count"`
	Phase                  string                   `json:"phase"`
	RunID                  string                   `json:"run_id"`
	Kind                   string                   `json:"kind"`
	StartedAt              time.Time                `json:"started_at"`
	ExecutionDeadline      time.Time                `json:"execution_deadline,omitempty"`
	ReasonDeadline         time.Time                `json:"reason_deadline,omitempty"`
	ConcludeStartedAt      time.Time                `json:"conclude_started_at,omitempty"`
	ConcludeDeadline       time.Time                `json:"conclude_deadline,omitempty"`
	Concluding             bool                     `json:"concluding"`
	History                []agent.Message          `json:"history"`
	Result                 *Result                  `json:"result,omitempty"`
	TaskPrompt             string                   `json:"task_prompt,omitempty"`
	ConclusionPrompt       string                   `json:"conclusion_prompt,omitempty"`
	ConclusionInputVersion int                      `json:"conclusion_input_version,omitempty"`
	ConclusionEvidence     []board.EvidenceRef      `json:"conclusion_evidence,omitempty"`
	Repairing              bool                     `json:"repairing,omitempty"`
	RepairCount            int                      `json:"repair_count,omitempty"`
	RepairPrompt           string                   `json:"repair_prompt,omitempty"`
	RepairPending          bool                     `json:"repair_pending,omitempty"`
	ContinuationCount      int                      `json:"continuation_count,omitempty"`
	ContinuationSequence   uint64                   `json:"continuation_sequence,omitempty"`
	ToolProgress           toolProgress             `json:"tool_progress,omitempty"`
}

func (s *session) validate(i executionIdentity) error {
	// Unversioned sessions cannot prove which immutable input produced their
	// messages/results. Preserve them for audit; never invent identity on load.
	if !supportedSessionSchema(s.SchemaVersion, s.Log) {
		return fmt.Errorf("unsupported session schema_version %d; automatic migration of unbound sessions is disabled", s.SchemaVersion)
	}
	a, _ := json.Marshal(s.Identity)
	b, _ := json.Marshal(i)
	if !bytes.Equal(a, b) || s.RunID != i.RunID || s.Kind != i.Kind || s.StartedAt.IsZero() {
		return errors.New("session task identity or immutable input mismatch")
	}
	if err := s.phaseState().ValidateAllowances(s.RepairPrompt != ""); err != nil {
		return err
	}
	var lastSequence uint64
	if s.ContextCheckpoint != nil {
		lastSequence = s.ContextCheckpoint.LastSequence
	}
	for _, message := range s.History {
		lastSequence = max(lastSequence, message.Sequence)
	}
	if err := s.ToolProgress.validate(lastSequence); err != nil {
		return err
	}
	if s.RecoveryCount < 0 || s.RecoveryCount > maxRunRecoveries {
		return errors.New("invalid saved recovery count")
	}
	return s.phaseState().ValidateDeadlines(phaseBudget(i.OriginalBudget))
}

func (s *session) save(runDir string, journal *eventlog.Log) error {
	checkpoint, err := journal.SyncCheckpoint()
	if err != nil {
		return err
	}
	s.Log = checkpoint
	s.SchemaVersion = sessionSchemaVersion
	s.Phase = "execute"
	if s.Concluding {
		s.Phase = "conclude"
	}
	if s.Repairing {
		s.Phase = "repair"
	}
	if s.Result != nil {
		s.Phase = "terminal"
		if s.Result.Retryable {
			s.Phase = "recoverable"
		}
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicSessionFile(runDir, raw)
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func atomicSessionFile(runDir string, raw []byte) error {
	f, err := os.CreateTemp(runDir, "session-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(runDir, "session.json")); err != nil {
		return err
	}
	return syncDirectory(runDir)
}
