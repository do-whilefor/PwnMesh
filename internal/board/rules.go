package board

import "pwnmesh/internal/board/internal/sqlstore"

func (t *Tx) CheckExecution(g Graph, fence ExecutionFence) error {
	if fence.Run == "" {
		return nil
	}
	if fence.Lease != "reason" && fence.Lease != "curate" && fence.Lease != "explore" {
		return Err(403, "unsupported execution lease")
	}
	revoked, err := t.RunRevoked(g.Project.ID, fence.Run)
	if err != nil {
		return err
	}
	if revoked {
		return Err(409, "Execution was revoked")
	}
	if fence.Lease == "curate" {
		if g.Project.OrchestrationVersion != 1 || fence.Intent != "" || g.Project.Curator == nil || g.Project.Curator.Worker != fence.Run {
			return Err(409, "Curation execution no longer owns its lease")
		}
		return nil
	}
	if fence.Lease == "reason" {
		if g.Project.Reason == nil || g.Project.Reason.Worker != fence.Run {
			return Err(409, "Reason execution no longer owns its lease")
		}
		return nil
	}
	for _, i := range g.Intents {
		if i.ID == fence.Intent && i.To == nil && Value(i.Worker) == fence.Run {
			return nil
		}
	}
	return Err(409, "Intent execution no longer owns its lease")
}

// Read authority must expire even when no writer has performed housekeeping.
// Use the same clock and SQLite comparison as ExpireProject without changing
// stored leases or reserving the writer connection for snapshot reads.
func (t *Tx) CheckExecutionRead(g Graph, fence ExecutionFence) error {
	if err := t.CheckExecution(g, fence); err != nil {
		return err
	}
	settings, err := t.Settings()
	if err != nil {
		return err
	}
	heartbeat, timeout := "", settings.ReasonTimeout
	switch fence.Lease {
	case "reason":
		if g.Project.Reason != nil {
			heartbeat = g.Project.Reason.Heartbeat
		}
	case "curate":
		if g.Project.Curator != nil {
			heartbeat = g.Project.Curator.Heartbeat
		}
	default:
		timeout = settings.IntentTimeout
		for _, intent := range g.Intents {
			if intent.ID == fence.Intent {
				heartbeat = Value(intent.Heartbeat)
				break
			}
		}
	}
	expired, err := sqlstore.ReadLeaseExpired(t.transactionQueries, t.Now, heartbeat, timeout)
	if err != nil {
		return err
	}
	if expired {
		return Err(409, "Execution lease has expired")
	}
	return nil
}

func (t *Tx) SetStatus(g *Graph, status string) error {
	if status != "active" && status != "stopped" {
		return Err(422, "status must be active or stopped")
	}
	if g.Project.Status == "completed" {
		return Err(409, "Completed projects cannot change status")
	}
	if g.Project.Status == "terminated" {
		return Err(409, "Terminated projects require an explicit restart")
	}
	if g.Project.Status == status {
		return nil
	}
	if status == "stopped" {
		if err := t.markPausedExecutions(*g); err != nil {
			return err
		}
		if err := t.RevokeRuns(g.Project.ID); err != nil {
			return err
		}
		g.Project.Reason = nil
		g.Project.Curator = nil
		for n := range g.Intents {
			if g.Intents[n].ConcludedAt == nil {
				g.Intents[n].Worker = nil
			}
		}
	} else if err := t.continuePausedExecutions(*g); err != nil {
		return err
	}
	g.Project.Status = status
	return nil
}
