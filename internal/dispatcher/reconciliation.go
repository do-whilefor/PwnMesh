package dispatcher

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"pwnmesh/internal/board"
)

func (s *Scheduler) reconcileProjects(ctx context.Context) (map[string]string, []board.Summary, error) {
	summaries, err := s.Client.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	states := map[string]string{}
	active := []board.Summary{}
	for _, p := range summaries {
		s.observeGeneration(p.Project)
		states[p.ID] = p.Status
		if p.Status == "active" {
			active = append(active, p)
			delete(s.cleaned, p.ID)
			if _, ok := s.checkpoints[p.ID]; !ok && p.Working+p.Unclaimed > 0 {
				s.checkpoints[p.ID] = checkpoint{p.FactCount, p.HintCount, p.Working + p.Unclaimed}
			}
		}
	}
	for id := range s.admitted {
		if states[id] != "active" {
			delete(s.admitted, id)
			delete(s.checkpoints, id)
		}
	}
	for id := range s.reasonWaits {
		if states[id] != "active" {
			delete(s.reasonWaits, id)
		}
	}
	for id := range s.curationWaits {
		if states[id] != "active" {
			delete(s.curationWaits, id)
		}
	}
	for id := range s.controlConflicts {
		if states[id] != "active" {
			delete(s.controlConflicts, id)
		}
	}
	for _, t := range s.running {
		id := t.Job.Graph.Project.ID
		if t.Job.Graph.Project.Generation != s.generations[id] {
			t.Cancel()
			continue
		}
		if states[id] != "active" {
			if states[id] == "completed" && s.decisionFinishAllowed(ctx, t) {
				continue
			}
			t.Cancel()
		}
	}
	return states, active, nil
}

func (s *Scheduler) cleanupInactiveProjects(ctx context.Context, states map[string]string) error {
	managed, err := s.Runner.Projects(ctx)
	if err != nil {
		return err
	}
	for _, id := range managed {
		state := states[id]
		if state == "active" {
			continue
		}
		if state == "completed" {
			finishing := false
			for _, t := range s.running {
				finishing = finishing || t.Job.Graph.Project.ID == id
			}
			if finishing {
				continue // Let the committed planner return its final observation.
			}
		}
		if state == "" {
			state = "deleted"
		}
		if s.cleaned[id] == state || s.cleanup[id] != "" {
			continue
		}
		s.queueCleanup(ctx, id, state)
	}
	return nil
}

func (s *Scheduler) reap() {
	for {
		select {
		case f := <-s.done:
			delete(s.running, f.Task.Job.RunID)
			// Retained results can retry delivery indefinitely, but a Server
			// outage must not turn completion wakeups into an immediate loop.
			if f.Outcome == "interrupted" && f.Task.Execution.Status == "result_pending" {
				s.deliveryWaits[f.Task.Job.RunID] = time.Now().Add(5 * time.Second)
			} else {
				delete(s.deliveryWaits, f.Task.Job.RunID)
			}
			if f.Outcome == "failed" && f.Task.urgentInput && f.Task.Job.Graph.Project.Generation == s.generations[f.Task.Job.Graph.Project.ID] {
				id := f.Task.Job.Graph.Project.ID
				wait := s.reasonWaits[id]
				wait.Urgent = true
				s.reasonWaits[id] = wait
			}
			if f.Outcome == "failed" && !f.Task.staleInputAt.IsZero() && f.Task.Job.Graph.Project.Generation == s.generations[f.Task.Job.Graph.Project.ID] {
				id, until := f.Task.Job.Graph.Project.ID, f.Task.staleInputAt.Add(reasonMaxWait)
				if until.After(s.controlConflicts[id]) {
					s.controlConflicts[id] = until
				}
			}
			key := s.rejectKey(f.Task.Job.Graph.Project.ID, f.Task.Job.Kind, f.Task.Worker.Name)
			if s.currentModelSettings(f.Task.Worker) {
				if f.Outcome == "unhealthy" {
					s.recordHealth(f.Task.Worker.Name, f.Err)
				} else {
					delete(s.unhealthy, f.Task.Worker.Name)
				}
			}
			if f.Outcome == "rejected" {
				s.rejected[key] = time.Now().Add(5 * time.Second)
			} else {
				delete(s.rejected, key)
			}
			if f.Outcome == "success" && f.Task.Job.Kind == "reason" && f.Task.Job.Graph.Project.Generation == s.generations[f.Task.Job.Graph.Project.ID] {
				g := f.Task.Job.Graph
				s.checkpoints[g.Project.ID] = checkpoint{len(g.Facts), len(g.Hints), g.OpenCount()}
				if ref := f.Task.Job.InputSnapshot; ref != nil {
					s.checkpoints[g.Project.ID] = checkpoint{ref.FactCount, ref.HintCount, ref.OpenCount}
				}
				s.decisionRevisions[g.Project.ID] = f.Task.Job.DecisionRevision
			}
			slog.Info("task finished", "project", f.Task.Job.Graph.Project.ID, "run", f.Task.Job.RunID, "task", f.Task.Job.Kind, "outcome", f.Outcome, "error", f.Err)
		default:
			goto cleanup
		}
	}
cleanup:
	for {
		select {
		case f := <-s.cleanupDone:
			delete(s.cleanup, f.ID)
			if f.Err == nil {
				s.cleaned[f.ID] = f.State
				if raw, ok := strings.CutPrefix(f.State, "restart:"); ok {
					generation, _ := strconv.ParseInt(raw, 10, 64)
					s.restartCleaned[f.ID] = generation
				}
			} else {
				slog.Warn("container cleanup failed", "project", f.ID, "error", f.Err)
			}
		default:
			return
		}
	}
}

// Restart keeps the project ID but replaces its execution round. Forget the
// prior planner boundary, including when a restart raced the list request.
func (s *Scheduler) observeGeneration(project board.Project) {
	if s.generations == nil {
		s.generations = map[string]int64{}
	}
	if s.generations[project.ID] != project.Generation {
		delete(s.checkpoints, project.ID)
		delete(s.decisionRevisions, project.ID)
		delete(s.schedules, project.ID)
		delete(s.reasonWaits, project.ID)
		delete(s.curationWaits, project.ID)
		delete(s.controlConflicts, project.ID)
		s.generations[project.ID] = project.Generation
	}
}

func (s *Scheduler) queueCleanup(ctx context.Context, id, state string) {
	s.cleanup[id] = state
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err := s.Runner.Cleanup(cleanupCtx, id, state)
		select {
		case s.cleanupDone <- cleaned{id, state, err}:
			// A cleanup failure must not trigger its own immediate retry loop.
			if err == nil {
				s.wake()
			}
		case <-ctx.Done():
		}
	}()
}

// Individual process cancellation is best effort during Docker outages. A
// restart additionally requires a confirmed container stop before any worker
// may enter the new round; this retains the container and workspace files.
func (s *Scheduler) restartReady(ctx context.Context, project board.Project) bool {
	if project.Generation == 0 || s.restartCleaned[project.ID] == project.Generation {
		return true
	}
	for _, task := range s.running {
		if task.Job.Graph.Project.ID == project.ID {
			return false
		}
	}
	if s.cleanup[project.ID] == "" {
		s.queueCleanup(ctx, project.ID, "restart:"+strconv.FormatInt(project.Generation, 10))
	}
	return false
}
