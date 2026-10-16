package dispatcher

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"pwnmesh/internal/protocol"
)

func (s *Scheduler) runTask(ctx context.Context, t *task) (outcome string, runErr error) {
	started := time.Now()
	defer func() {
		if attrs := controlOutcomeAttrs(t, outcome, runErr, started, time.Now()); attrs != nil {
			slog.LogAttrs(context.Background(), slog.LevelInfo, "control task finished", attrs...)
		}
	}()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	leaseCtx, stopLease := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() { defer close(heartbeatDone); s.heartbeat(leaseCtx, t, cancel) }()
	defer func() {
		stopLease()
		<-heartbeatDone
		if cause := context.Cause(ctx); outcome != "success" && dependencyInvalidated(cause) && (t.Root == nil || t.Root.Err() == nil) {
			// The same terminal boundary covers cancellation during model calls,
			// health checks and infrastructure retry backoff. Recovery must not
			// restart work after its accepted upstream result became invalid.
			s.terminal(t, "cancelled", protocol.Result{Status: "failed", FailureKind: "dependency_invalidated", Error: cause.Error()})
			outcome, runErr = "cancelled", cause
		}
		// Invalidation can interrupt startup, a model call, or recovery backoff.
		// Reconcile once after the heartbeat stops, before releasing this lease.
		if cause := context.Cause(ctx); outcome != "failed" && outcome != "success" && decisionStateChanged(cause) && (t.Root == nil || t.Root.Err() == nil) {
			committed, err := s.controlCommitted(ctx, t)
			switch {
			case err != nil:
				outcome, runErr = "interrupted", err
			case committed:
				outcome, runErr = "success", nil
			default:
				s.terminal(t, "failed", protocol.Result{Status: "failed", FailureKind: "state_changed", Error: cause.Error()})
				outcome, runErr = "failed", cause
			}
		}
		releaseCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = s.Client.Do(releaseCtx, "POST", s.leasePath(t)+"/release", map[string]string{"worker": t.Lease.Run}, nil, nil)
	}()
	return s.runRegistered(ctx, t, func() { stopLease(); <-heartbeatDone })
}
func (s *Scheduler) leasePath(t *task) string {
	base := projectPath(t.Job.Graph.Project.ID)
	if t.Job.Kind == "reason" || t.Job.Kind == "curate" {
		return base + "/" + t.Job.Kind
	}
	return base + "/intents/" + t.Lease.Intent
}
func (s *Scheduler) renewLease(ctx context.Context, t *task) error {
	return s.renewLeaseWithVersion(ctx, t, t.Job.Kind != "reason" || t.Job.Decision == nil || t.Job.Decision.Version != 2)
}

// A running batch planner can refresh its stable read view after a conflict.
// Its initial whole-graph hash must not kill that session when producers publish
// new facts. Startup/recovery still reject obsolete inputs before model work;
// periodic renewal retains the project, generation and execution lease fences.
func (s *Scheduler) renewLeaseWithVersion(ctx context.Context, t *task, checkInput bool) error {
	body := map[string]string{"worker": t.Lease.Run}
	if version := immutableInputVersion(t); checkInput && version != "" {
		body["expected_version"] = version
	}
	return s.Client.Do(ctx, "POST", s.leasePath(t)+"/heartbeat", body, nil, &t.Lease)
}

func (s *Scheduler) heartbeat(ctx context.Context, t *task, cancel context.CancelCauseFunc) {
	interval := time.Duration(s.Config.Runtime.Interval) * time.Second
	timeout := t.LeaseTimeout
	if timeout <= interval {
		timeout = 2 * interval
	}
	// A valid configured interval may consume most of the lease. Renew early
	// enough to leave another interval for the request and one for shutdown;
	// otherwise the first renewal can start with an already expired deadline.
	interval = min(interval, timeout/3)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// Queueing behind other transactions must not turn a healthy busy
			// Server into a hard cancellation after only two heartbeat intervals.
			// Stop before the actual Server lease expires, including queue time.
			callCtx, c := context.WithDeadline(ctx, last.Add(timeout-interval))
			err := s.renewLease(callCtx, t)
			c()
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				last = time.Now()
				continue
			}
			var pe *ProtocolError
			if errors.As(err, &pe) && (pe.Status == 403 || pe.Status == 404 || pe.Status == 409) || time.Since(last) >= timeout-interval {
				if s.decisionFinishAllowed(ctx, t) {
					// A commit revokes its lease before the Worker receives the
					// acknowledgement. Give that reply and metrics a bounded drain.
					timer := time.NewTimer(time.Until(time.Unix(0, t.committedAt.Load()).Add(decisionFinishGrace)))
					select {
					case <-ctx.Done():
						timer.Stop()
					case <-timer.C:
						cancel(err)
					}
					return
				}
				if decisionStateChanged(err) {
					slog.Info("decision input changed; cancelling stale run", "project", t.Job.Graph.Project.ID, "run", t.Job.RunID)
				}
				cancel(err)
				return
			}
		}
	}
}
