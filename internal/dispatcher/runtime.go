package dispatcher

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"pwnmesh/internal/board"
)

func (s *Scheduler) loadLeaseTimeout(ctx context.Context) error {
	var settings board.Settings
	if err := s.Client.Do(ctx, "GET", "/settings", nil, &settings, nil); err != nil {
		return err
	}
	s.leaseTimeout = time.Duration(min(settings.IntentTimeout, settings.ReasonTimeout)) * time.Second
	if s.leaseTimeout <= time.Duration(s.Config.Runtime.Interval)*time.Second {
		return errors.New("heartbeat interval must be shorter than each server lease timeout")
	}
	return nil
}

func (s *Scheduler) prepareRun(ctx context.Context) error {
	if err := s.loadLeaseTimeout(ctx); err != nil {
		return err
	}
	if s.leaseTimeout < 2*time.Duration(s.Config.Runtime.Interval)*time.Second {
		slog.Warn("server lease timeout leaves little heartbeat slack", "interval", s.Config.Runtime.Interval, "lease_timeout", int(s.leaseTimeout/time.Second))
	}
	_ = s.Health(ctx, false)
	// With one dispatcher, old managed executions cannot survive a restart and
	// keep exploring after their leases are reassigned. Keep workspace contents.
	old, err := s.Runner.Projects(ctx)
	if err != nil {
		return err
	}
	for _, id := range old {
		if err := s.Runner.Cleanup(ctx, id, "stopped"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scheduler) prepareStep(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.bindOwnership(ctx); err != nil {
		return err
	}
	s.reap()
	s.modelSettingsErr = s.refreshModelSettings()
	if s.leaseTimeout == 0 {
		return s.loadLeaseTimeout(ctx)
	}
	return nil
}

func (s *Scheduler) stopWorkers() {
	for _, t := range s.running {
		t.Cancel()
	}
	s.wg.Wait()
}

func (s *Scheduler) runLoop(parent, ctx context.Context) error {
	tick := time.NewTicker(time.Duration(s.Config.Runtime.Interval) * time.Second)
	defer tick.Stop()
	deadline := time.NewTimer(time.Hour)
	deadline.Stop()
	defer deadline.Stop()
	for {
		if err := s.Step(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("dispatcher tick failed", "error", err)
		}
		var ready <-chan time.Time
		if !s.nextWake.IsZero() {
			deadline.Reset(time.Until(s.nextWake))
			ready = deadline.C
		}
		select {
		case <-ctx.Done():
			if parent.Err() == nil {
				return context.Cause(ctx)
			}
			return nil
		case <-tick.C:
		case <-s.wakeup:
		case <-ready:
		}
		deadline.Stop()
	}
}

// Completion records stay in their existing queues until Step reaps them.
// Coalesce notifications without blocking workers or losing queued outcomes.
func (s *Scheduler) wake() {
	select {
	case s.wakeup <- struct{}{}:
	default:
	}
}
