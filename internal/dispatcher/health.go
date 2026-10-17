package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/config"
	"pwnmesh/internal/modelwire"
	"pwnmesh/internal/provider"
)

func (s *Scheduler) Health(ctx context.Context, force bool) error {
	if err := s.refreshModelSettings(); err != nil {
		return err
	}
	if s.Config.Runtime.HealthMode == "disabled" && !force {
		return nil
	}
	var result error
	for _, w := range s.Config.Workers {
		err := s.health(ctx, w)
		s.recordHealth(w.Name, err)
		if err != nil {
			result = errors.Join(result, fmt.Errorf("worker %s: %w", w.Name, err))
			slog.Warn("worker health failed", "worker", w.Name, "error", err)
		}
	}
	return result
}

// Readiness verifies a small tool round trip, not task quality or the model's
// full context window. Each strategy gets two turns under one worker deadline;
// provider transport retries remain bounded by that same deadline.
const healthMaxTokens = provider.ProbeMaxTokens

type healthFailure struct {
	Kind string
	Err  error
}

func (e *healthFailure) Error() string { return "model readiness " + e.Kind + ": " + e.Err.Error() }
func (e *healthFailure) Unwrap() error { return e.Err }

// An explicit endpoint protocol/configuration rejection is not repaired by a five-second
// delay. Keep that worker out of new assignments until a successful explicit
// readiness check (or the next process startup). Unknown/transient failures
// retain the existing bounded cooldown and never claim incompatibility.
func (s *Scheduler) recordHealth(name string, err error) {
	if err == nil {
		delete(s.incompatible, name)
		delete(s.unhealthy, name)
		return
	}
	var failure *healthFailure
	if errors.As(err, &failure) && (failure.Kind == "incompatible" || failure.Kind == "configuration") {
		if s.incompatible == nil {
			s.incompatible = map[string]string{}
		}
		s.incompatible[name] = failure.Kind
		delete(s.unhealthy, name)
		return
	}
	if s.unhealthy == nil {
		s.unhealthy = map[string]time.Time{}
	}
	s.unhealthy[name] = time.Now().Add(5 * time.Second)
}

func (s *Scheduler) health(ctx context.Context, w config.Worker, budgets ...config.Task) error {
	if s.CheckHealth != nil {
		return s.CheckHealth(ctx, w)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.Runtime.HealthTimeout)*time.Second)
	defer cancel()
	if len(budgets) == 0 {
		for _, kind := range w.TaskTypes {
			budgets = append(budgets, s.Config.Task(kind))
		}
		if len(budgets) == 0 {
			budgets = []config.Task{{}}
		}
	}
	seen := map[string]bool{}
	for _, budget := range budgets {
		p, err := provider.FromEnvironment(ctx, w.Getenv, budget.ReasoningEffort)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return healthRequestFailure(err)
			}
			return &healthFailure{Kind: "configuration", Err: err}
		}
		if p.Client != nil {
			defer p.Client.CloseIdleConnections()
		}
		if seen[p.ReasoningEffort] {
			continue
		}
		seen[p.ReasoningEffort] = true
		// Match role effort without asking an unverified model to consume the
		// much larger task output allowance. Truncation means unverified.
		p.MaxTokens = min(p.MaxTokens, healthMaxTokens)
		if err := probeHealth(ctx, p); err != nil {
			return fmt.Errorf("effort %s: %w", p.ReasoningEffort, err)
		}
	}
	return nil
}

func healthRequestFailure(err error) error {
	kind := "unverified"
	var model *agent.ModelError
	var status *modelwire.HTTPError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		kind = "unverified"
	case errors.As(err, &model) && (model.Kind == agent.ErrorContextOverflow || model.Kind == agent.ErrorBudget):
		kind = "unverified"
	case errors.As(err, &status) && (status.Status == 401 || status.Status == 403):
		kind = "configuration"
	case errors.As(err, &status) && (status.Status == 400 || status.Status == 404 || status.Status == 405 || status.Status == 415 || status.Status == 422):
		kind = "incompatible"
	case errors.As(err, &model) && (model.Kind == agent.ErrorTransport || model.Kind == agent.ErrorRateLimit || model.Kind == agent.ErrorUnavailable):
		kind = "transient"
	}
	return &healthFailure{Kind: kind, Err: err}
}

func probeHealth(ctx context.Context, p agent.Provider) error {
	if err := provider.Probe(ctx, p); err != nil {
		return healthRequestFailure(err)
	}
	return nil
}
