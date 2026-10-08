package dispatcher

import (
	"context"
	"errors"
	"slices"
	"time"

	"pwnmesh/internal/board"
)

// Coalesce a producer's fact/candidate/final-result burst without waiting
// indefinitely for a long-lived Worker. The persisted cursor is authoritative;
// this timer is only an optimization and can be lost on dispatcher restart.
func (s *Scheduler) waitForCuration(g board.Graph, revision int64, now time.Time) bool {
	id := g.Project.ID
	if !s.producersRunning(g) {
		delete(s.controlConflicts, id)
		return false
	}
	// Keep unacknowledged user input and invalid support urgent until a planner
	// consumes them, even when another ordinary control attempt was stale.
	if s.schedules[id].HintCount > s.checkpoints[id].Hints || s.reasonWaits[id].Urgent {
		return false
	}
	if until, ok := s.controlConflicts[id]; ok {
		return s.waitForControlConflict(until, now)
	}
	if s.curationWaits == nil {
		s.curationWaits = map[string]reasonWait{}
	}
	wait, ok := s.curationWaits[g.Project.ID]
	if !ok {
		wait.First, wait.Changed, wait.Revision = now, now, revision
	} else if wait.Revision != revision {
		wait.Changed, wait.Revision = now, revision
	}
	s.curationWaits[g.Project.ID] = wait
	return s.waitForQuiet(wait, now)
}

func (s *Scheduler) producersRunning(g board.Graph) bool {
	for _, running := range s.running {
		if running.Job.Graph.Project.ID == g.Project.ID && running.Job.Kind != "reason" && running.Job.Kind != "curate" {
			return true
		}
	}
	for _, intent := range g.Intents {
		if intent.Worker != nil && intent.To == nil && intent.ConcludedAt == nil {
			return true
		}
	}
	return false
}

// Only a confirmed stale attempt delays the next ordinary control run beyond
// its usual quiet window. Give the active producer wave time to drain, bounded
// from the failure itself: new observations never extend this deadline. This is
// an ephemeral scheduling optimization; server CAS still decides write safety.
func (s *Scheduler) waitForControlConflict(until, now time.Time) bool {
	if !now.Before(until) {
		return false
	}
	s.wakeAt(until)
	return true
}

func (s *Scheduler) curationCommitted(ctx context.Context, t *task) (bool, error) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var receipt struct {
		board.StateActionResult
		ExecutionStatus string `json:"execution_status"`
	}
	err := s.Client.Do(readCtx, "GET", projectPath(t.Job.Graph.Project.ID)+"/state/curation/receipt", nil, &receipt, &t.Lease)
	var pe *ProtocolError
	if errors.As(err, &pe) && pe.Status == 404 {
		return false, nil
	}
	if err == nil && receipt.Committed && receipt.ExecutionStatus != "succeeded" {
		// Only old persisted batches require delivery after commit. The explicit
		// apply atomically settles their existing receipt without model recovery.
		if !slices.Contains([]string{"prepared", "running", "retryable", "result_pending"}, receipt.ExecutionStatus) {
			return false, errors.New("committed curation has no pending or successful execution")
		}
		var result struct {
			Status string `json:"status"`
		}
		err = s.Client.Do(readCtx, "POST", executionPath(t)+"/apply", map[string]any{}, &result, &t.Lease)
		if err == nil && result.Status != "succeeded" {
			err = errors.New("curation receipt settlement did not succeed")
		}
	}
	return receipt.Committed && err == nil, err
}

// Keep one slot available to control roles when concurrency permits. A
// one-slot installation executes serially; no Worker waits inside its slot
// for another Agent to run. This limit is only applied to the new mode.
func (s *Scheduler) executionCapacity(project string) bool {
	executions, projectExecutions := 0, 0
	for _, running := range s.running {
		if running.Job.Kind == "reason" || running.Job.Kind == "curate" {
			continue
		}
		executions++
		if running.Job.Graph.Project.ID == project {
			projectExecutions++
		}
	}
	return executions < max(1, s.Config.Runtime.MaxWorkers-1) &&
		projectExecutions < max(1, s.Config.Runtime.MaxProjectWorkers-1)
}

func (s *Scheduler) backendExecutionCapacity(name string, limit int) bool {
	for _, backend := range s.Config.Workers {
		if backend.Name != name || !slices.Contains(backend.TaskTypes, "curate") && !slices.Contains(backend.TaskTypes, "reason") {
			continue
		}
		count := 0
		for _, running := range s.running {
			if running.Worker.Name == name && running.Job.Kind != "reason" && running.Job.Kind != "curate" {
				count++
			}
		}
		return count < max(1, limit-1)
	}
	return true
}
