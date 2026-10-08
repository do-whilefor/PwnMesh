package dispatcher

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/worker"
)

type Runner interface {
	Run(context.Context, config.Worker, worker.Job) (worker.Result, error)
	Cleanup(context.Context, string, string) error
	Projects(context.Context) ([]string, error)
}
type checkpoint struct{ Facts, Hints, Open int }
type reasonWait struct {
	First, Changed time.Time
	Revision       int64
	Urgent         bool
}

const reasonQuietPeriod = 15 * time.Second
const reasonMaxWait = time.Minute

type task struct {
	Job          worker.Job
	Worker       config.Worker
	Lease        Lease
	Cancel       context.CancelFunc
	Root         context.Context
	Execution    board.Execution
	LeaseTimeout time.Duration
	committedAt  atomic.Int64
	staleInputAt time.Time // Written before publishing this task to done.
	urgentInput  bool      // An invalid dependency is consumed only by success.
}
type finished struct {
	Task    *task
	Outcome string
	Err     error
}
type cleaned struct {
	ID, State string
	Err       error
}
type Scheduler struct {
	Config            config.Config
	Client            *Client
	Runner            Runner
	running           map[string]*task
	admitted          map[string]bool
	checkpoints       map[string]checkpoint
	reasonWaits       map[string]reasonWait
	curationWaits     map[string]reasonWait
	controlConflicts  map[string]time.Time
	unhealthy         map[string]time.Time
	incompatible      map[string]string
	rejected          map[string]time.Time
	deliveryWaits     map[string]time.Time
	cleanup           map[string]string
	cleaned           map[string]string
	done              chan finished
	cleanupDone       chan cleaned
	wakeup            chan struct{}
	nextWake          time.Time
	wg                sync.WaitGroup
	cursor            int
	pendingExecutions []board.ExecutionSummary
	decisionRevisions map[string]int64
	stateRevisions    map[string]int64
	schedules         map[string]board.SchedulePage
	generations       map[string]int64
	restartCleaned    map[string]int64
	leaseTimeout      time.Duration
	// CheckHealth overrides the model readiness probe in tests or embeddings.
	CheckHealth func(context.Context, config.Worker) error
}

func New(c config.Config, r Runner) *Scheduler {
	s := &Scheduler{Config: c, Runner: r, Client: &Client{Base: c.Server}, running: map[string]*task{}, admitted: map[string]bool{}, checkpoints: map[string]checkpoint{}, unhealthy: map[string]time.Time{}, rejected: map[string]time.Time{}, cleanup: map[string]string{}, cleaned: map[string]string{}, done: make(chan finished, c.Runtime.MaxWorkers), cleanupDone: make(chan cleaned, c.Runtime.MaxProjects+8), decisionRevisions: map[string]int64{}, stateRevisions: map[string]int64{}}
	s.configureGraphHandler()
	s.configureInputReader()
	s.schedules = map[string]board.SchedulePage{}
	s.generations = map[string]int64{}
	s.restartCleaned = map[string]int64{}
	s.reasonWaits = map[string]reasonWait{}
	s.controlConflicts = map[string]time.Time{}
	s.deliveryWaits = map[string]time.Time{}
	s.wakeup = make(chan struct{}, 1)
	return s
}
func (s *Scheduler) Health(ctx context.Context, force bool) error {
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
func (s *Scheduler) Run(ctx context.Context) error {
	var settings board.Settings
	if err := s.Client.Do(ctx, "GET", "/settings", nil, &settings, nil); err != nil {
		return err
	}
	leaseTimeout := min(settings.IntentTimeout, settings.ReasonTimeout)
	s.leaseTimeout = time.Duration(leaseTimeout) * time.Second
	if s.Config.Runtime.Interval >= leaseTimeout {
		return errors.New("heartbeat interval must be shorter than each server lease timeout")
	}
	if leaseTimeout < 2*s.Config.Runtime.Interval {
		slog.Warn("server lease timeout leaves little heartbeat slack", "interval", s.Config.Runtime.Interval, "lease_timeout", leaseTimeout)
	}
	_ = s.Health(ctx, false)
	// With one dispatcher, old managed executions cannot survive a restart and
	// keep exploring after their leases are reassigned. Keep workspace contents.
	old, err := s.Runner.Projects(ctx)
	if err != nil {
		return err
	}
	for _, id := range old {
		if err = s.Runner.Cleanup(ctx, id, "stopped"); err != nil {
			return err
		}
	}
	defer func() {
		for _, t := range s.running {
			t.Cancel()
		}
		s.wg.Wait()
	}()
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
			if f.Outcome == "unhealthy" {
				s.recordHealth(f.Task.Worker.Name, f.Err)
			} else {
				delete(s.unhealthy, f.Task.Worker.Name)
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
func (s *Scheduler) Step(ctx context.Context) error {
	s.nextWake = time.Time{}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.reap()
	if s.leaseTimeout == 0 {
		var settings board.Settings
		if err := s.Client.Do(ctx, "GET", "/settings", nil, &settings, nil); err != nil {
			return err
		}
		s.leaseTimeout = time.Duration(min(settings.IntentTimeout, settings.ReasonTimeout)) * time.Second
		if s.leaseTimeout <= time.Duration(s.Config.Runtime.Interval)*time.Second {
			return errors.New("heartbeat interval must be shorter than each server lease timeout")
		}
	}
	summaries, err := s.Client.List(ctx)
	if err != nil {
		return err
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
	if err := s.loadExecutions(ctx); err != nil {
		return err
	}
	s.releaseIdleAdmissions()
	if err := s.recoverExecutions(ctx, states); err != nil {
		return err
	}
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
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	if len(active) > 0 {
		offset := s.cursor % len(active)
		active = append(active[offset:], active[:offset]...)
		s.cursor++
	}
	for len(s.running) < s.Config.Runtime.MaxWorkers {
		launched := false
		for _, p := range active {
			if !s.admitted[p.ID] {
				continue
			}
			ok, err := s.dispatch(ctx, p.ID)
			if err != nil {
				slog.Warn("dispatch skipped", "project", p.ID, "error", err)
			}
			launched = launched || ok
			if len(s.running) >= s.Config.Runtime.MaxWorkers {
				return nil
			}
		}
		if launched {
			continue
		}
		if len(s.admitted) >= s.Config.Runtime.MaxProjects {
			return nil
		}
		for _, p := range active {
			if s.admitted[p.ID] {
				continue
			}
			ok, err := s.dispatch(ctx, p.ID)
			if err != nil {
				slog.Warn("dispatch skipped", "project", p.ID, "error", err)
			}
			if ok {
				launched = true
				break
			}
		}
		if !launched {
			break
		}
	}
	return nil
}
func (s *Scheduler) trigger(g board.Graph, check board.ExecutionCheck, previous board.SchedulePage, now time.Time) string {
	if check.PreviousRunID != "" {
		return "explicit_retry"
	}
	p, ok := s.checkpoints[g.Project.ID]
	if !ok {
		return "initial"
	}
	// Legacy To-based draining excludes the planner's own abandonment (To=nil).
	// Actual scheduling separately filters State.Steps and ConcludedAt.
	facts, hints, open := len(g.Facts), len(g.Hints), g.OpenCount()
	if input, ok := s.schedules[g.Project.ID]; ok {
		facts, hints, open = input.FactCount, input.HintCount, input.OpenCount
	}
	if facts > p.Facts || hints > p.Hints || (p.Open > 0 && open == 0) || s.stateRevisions[g.Project.ID] > s.decisionRevisions[g.Project.ID] {
		// User input and a drained execution phase need an immediate decision.
		// Ordinary Execute updates often arrive in bursts; deciding between
		// them starts model requests that the next heartbeat must cancel.
		if hints <= p.Hints && open > 0 && s.waitForReason(g, previous, now) {
			return ""
		}
		return "new_facts_or_hints_or_finished_intents"
	}
	delete(s.reasonWaits, g.Project.ID)
	return ""
}

func (s *Scheduler) waitForReason(g board.Graph, previous board.SchedulePage, now time.Time) bool {
	id := g.Project.ID
	if !s.producersRunning(g) {
		delete(s.controlConflicts, id)
		return false
	}
	input := s.schedules[id]
	s.noteInvalidDependencies(id, previous, input)
	if !s.reasonWaits[id].Urgent {
		if until, ok := s.controlConflicts[id]; ok {
			return s.waitForControlConflict(until, now)
		}
	}
	wait, ok := s.reasonWaits[id]
	if !ok || wait.First.IsZero() {
		wait.First, wait.Changed, wait.Revision = now, now, input.DecisionRevision
	} else if wait.Revision != input.DecisionRevision {
		wait.Changed, wait.Revision = now, input.DecisionRevision
	}
	s.reasonWaits[id] = wait
	return s.waitForQuiet(wait, now)
}

// Honor the coalescing boundary itself, rather than rounding it up to the
// next heartbeat tick. Only still-pending waits schedule a wake, so an expired
// wait that cannot acquire a worker does not turn into a busy retry loop.
func (s *Scheduler) waitForQuiet(wait reasonWait, now time.Time) bool {
	until := wait.Changed.Add(reasonQuietPeriod)
	if maximum := wait.First.Add(reasonMaxWait); maximum.Before(until) {
		until = maximum
	}
	if wait.Urgent || !now.Before(until) {
		return false
	}
	s.wakeAt(until)
	return true
}

func (s *Scheduler) wakeAt(until time.Time) {
	if s.nextWake.IsZero() || until.Before(s.nextWake) {
		s.nextWake = until
	}
}

// A queued dependency is normal pipeline progress. Missing, failed or invalid
// accepted dependencies require a new decision rather than more execution.
func supportNeedsDecision(step board.Step, steps map[string]board.Step) bool {
	if len(step.InvalidSources) > 0 {
		return true
	}
	for _, id := range step.BlockedBy {
		upstream, exists := steps[id]
		if !exists || !slices.Contains([]string{"open", "running", "blocked"}, upstream.Status) {
			return true
		}
	}
	return false
}

func (s *Scheduler) noteInvalidDependencies(id string, previous, input board.SchedulePage) {
	// A newly invalid dependency must be reconsidered promptly, including
	// when a planner slot only becomes available on a later tick. Old invalid
	// steps must not disable coalescing for every subsequent ordinary update.
	oldInvalid := map[string]bool{}
	oldSteps, currentSteps := map[string]board.Step{}, map[string]board.Step{}
	for _, step := range previous.Steps {
		oldSteps[step.ID] = step
	}
	for _, step := range input.Steps {
		currentSteps[step.ID] = step
	}
	for _, step := range previous.Steps {
		oldInvalid[step.ID] = supportNeedsDecision(step, oldSteps)
	}
	for _, step := range input.Steps {
		if supportNeedsDecision(step, currentSteps) && !oldInvalid[step.ID] {
			wait := s.reasonWaits[id]
			wait.Urgent = true
			s.reasonWaits[id] = wait
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
		delete(s.stateRevisions, project.ID)
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

func (s *Scheduler) dispatch(ctx context.Context, id string) (bool, error) {
	if s.cleanup[id] != "" {
		return false, nil
	}
	running := 0
	localReason, localCurator := false, false
	for _, t := range s.running {
		if t.Job.Graph.Project.ID == id {
			running++
			localReason = localReason || t.Job.Kind == "reason"
			localCurator = localCurator || t.Job.Kind == "curate"
		}
	}
	if running >= s.Config.Runtime.MaxProjectWorkers {
		return false, nil
	}
	input, err := s.scheduleInput(ctx, id)
	if err != nil {
		return false, err
	}
	g := board.Graph{Project: input.Project, Intents: input.Intents}
	if g.Project.OrchestrationVersion != 1 {
		return false, nil
	}
	state := board.State{Graph: g, Steps: input.Steps, Revision: input.Revision, DecisionRevision: input.DecisionRevision}
	s.observeGeneration(g.Project)
	previous := s.schedules[id]
	s.schedules[id] = input
	// Cancellation may take time in a container. Do not start the new round
	// in the same workspace until every old local execution has actually left.
	stale := false
	for _, task := range s.running {
		if task.Job.Graph.Project.ID == id && task.Job.Graph.Project.Generation != g.Project.Generation {
			task.Cancel()
			stale = true
		}
	}
	if stale {
		return false, nil
	}
	if !s.restartReady(ctx, g.Project) {
		return false, nil
	}
	s.stateRevisions[id] = state.DecisionRevision
	if g.Project.Status != "active" {
		return false, nil
	}
	// Preserve a correction while either control role is coalescing or a stale
	// attempt is still releasing its lease.
	s.noteInvalidDependencies(id, previous, input)
	if !s.producersRunning(g) {
		delete(s.controlConflicts, id)
	}
	// Curation is driven by a durable input boundary. Heartbeats and a
	// curator's own acknowledgement never create work. Keep its readiness while
	// checking already-authorized Steps so a new dependency result does not
	// insert a whole curation model turn into the execution chain.
	if !input.CurationNeeded {
		delete(s.curationWaits, id)
	}
	curating := localCurator || g.Project.Curator != nil
	var curationCheck board.ExecutionCheck
	curationReady := false
	if input.CurationNeeded && !localCurator {
		check, err := s.candidateCheck(ctx, input, g, "curate", nil)
		if err != nil {
			return false, err
		}
		if err := s.automaticRetry(ctx, &g, &check, "curate"); err != nil {
			return false, err
		}
		// An exhausted curator attempt must not prevent the main Agent from
		// handling failed work. Keep pending/runnable curation ahead of planning;
		// a new input version can authorize curation again after a terminal run.
		curating = g.Project.Curator != nil || check.Pending || !check.Blocked
		curationCheck = check
		curationReady = g.Project.Curator == nil && curating && (input.CurationRequested || check.PreviousRunID != "" || !s.waitForCuration(g, input.Revision, time.Now()))
	}
	reasonCheck, err := s.candidateCheck(ctx, input, g, "reason", nil)
	if err != nil {
		return false, err
	}
	s.restoreDecisionBoundary(id, reasonCheck.LatestDecision)
	if !localReason {
		if err := s.automaticRetry(ctx, &g, &reasonCheck, "reason"); err != nil {
			return false, err
		}
	}
	if input.Initial && !curating {
		if g.Project.Reason != nil || localReason {
			return false, nil
		}
		return s.launch(ctx, g, "reason", nil, "initial", reasonCheck)
	}
	if curationReady {
		// User corrections and newly invalid support must reach the planner
		// before draining more of its old authorization. Curation still runs
		// first because both control writes are bound to the same live version.
		if input.CurationRequested || input.HintCount > s.checkpoints[id].Hints || s.reasonWaits[id].Urgent {
			if ok, err := s.launch(ctx, g, "curate", nil, "pending_observations", curationCheck); ok || err != nil {
				return ok, err
			}
		}
	}
	// An explicit request says the current interpretation needs attention.
	// Hold new execution while that curator can run; an exhausted attempt still
	// lets Decide plan recovery through the existing blocked-control path.
	if input.CurationRequested && curating {
		return false, nil
	}
	deferredReason := ""
	if g.Project.Reason == nil && !localReason && !curating {
		if trigger := s.trigger(g, reasonCheck, previous, time.Now()); trigger != "" {
			// Between successful producers, continue the finite authorized
			// pipeline before starting another planning turn. Live producers
			// retain the existing quiet/max-wait deadline; urgent input and
			// unresolved execution failures still reach Decide first.
			preferReady := trigger == "new_facts_or_hints_or_finished_intents" && !s.producersRunning(g) && input.HintCount <= s.checkpoints[id].Hints && !s.reasonWaits[id].Urgent
			for _, step := range state.Steps {
				if len(step.InvalidSources) > 0 || step.Status == "failed" || step.Status == "needs_review" {
					preferReady = false
				}
			}
			if preferReady {
				deferredReason = trigger
			} else if ok, err := s.launch(ctx, g, "reason", nil, trigger, reasonCheck); ok || err != nil {
				return ok, err
			}
		}
	}
	var next *board.Step
	var nextCheck board.ExecutionCheck
	for _, step := range board.ExecutionSteps(input.Intents, input.Steps) {
		check, err := s.candidateCheck(ctx, input, g, "explore", &board.Intent{ID: step.ID})
		if err != nil {
			return false, err
		}
		if check.Blocked {
			continue
		}
		local := false
		for _, t := range s.running {
			if t.Job.Graph.Project.ID == id && t.Lease.Intent == step.ID {
				local = true
			}
		}
		// Explicit priority wins; equal-priority work drains oldest first so
		// newly appended Steps cannot continually overtake the ready queue.
		// Scheduling pages preserve creation/row order for equal timestamps.
		if !local && (next == nil || step.Priority > next.Priority || (step.Priority == next.Priority && step.CreatedAt < next.CreatedAt)) {
			next, nextCheck = &step, check
		}
	}
	if next != nil && s.executionCapacity(id) {
		// Registration resolves the complete immutable assignment on the Server.
		if ok, err := s.launch(ctx, g, "explore", &board.Intent{ID: next.ID}, "", nextCheck); ok || err != nil {
			return ok, err
		}
	}
	if deferredReason != "" {
		return s.launch(ctx, g, "reason", nil, deferredReason, reasonCheck)
	}
	// Execute cannot consume the reserved control slot. In a one-slot setup,
	// drain the finite authorized batch before consolidating its observations;
	// blocked/invalid Steps never bypass their existing candidate checks.
	if curationReady {
		return s.launch(ctx, g, "curate", nil, "pending_observations", curationCheck)
	}
	return false, nil
}

func (s *Scheduler) rejectKey(project, kind, name string) string {
	return project + "\x00" + kind + "\x00" + name
}
func (s *Scheduler) choose(project, kind string) *config.Worker {
	counts := map[string]int{}
	for _, t := range s.running {
		counts[t.Worker.Name]++
	}
	candidates := []config.Worker{}
	now := time.Now()
	for _, w := range s.Config.Workers {
		if w.Type != "go" {
			continue
		}
		if s.incompatible[w.Name] != "" {
			continue
		}
		if !slices.Contains(w.TaskTypes, kind) || counts[w.Name] >= w.MaxRunning {
			continue
		}
		if kind == "explore" && !s.backendExecutionCapacity(w.Name, w.MaxRunning) {
			continue
		}
		until := s.unhealthy[w.Name]
		if rejected := s.rejected[s.rejectKey(project, kind, w.Name)]; rejected.After(until) {
			until = rejected
		}
		if now.Before(until) {
			// A short readiness/refusal backoff must not inherit a much longer
			// polling interval. Capacity-bound work wakes on actual completion.
			s.wakeAt(until)
			continue
		}
		candidates = append(candidates, w)
	}
	mrand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return counts[a.Name] < counts[b.Name]
	})
	if len(candidates) == 0 {
		return nil
	}
	return &candidates[0]
}
func (s *Scheduler) launch(ctx context.Context, g board.Graph, kind string, intent *board.Intent, trigger string, check board.ExecutionCheck) (bool, error) {
	if check.Blocked || g.Project.OrchestrationVersion != 1 || !slices.Contains([]string{"reason", "curate", "explore"}, kind) {
		return false, nil
	}
	w := s.choose(g.Project.ID, kind)
	if w == nil {
		if kind == "reason" || kind == "curate" {
			configured := false
			for _, candidate := range s.Config.Workers {
				configured = configured || slices.Contains(candidate.TaskTypes, kind) && candidate.Type == "go"
			}
			if !configured {
				return false, fmt.Errorf("project requires a worker supporting %s", kind)
			}
		}
		return false, nil
	}
	var bytes [16]byte
	if _, err := crand.Read(bytes[:]); err != nil {
		return false, err
	}
	id := hex.EncodeToString(bytes[:])
	lease := Lease{Run: w.Name + "@" + id, Kind: kind}
	claim := projectPath(g.Project.ID)
	body := map[string]string{"worker": lease.Run}
	if kind == "reason" || kind == "curate" {
		claim += "/" + kind + "/claim"
		body["trigger"] = trigger
	} else {
		lease.Intent = intent.ID
		claim += "/intents/" + intent.ID + "/heartbeat"
	}
	if err := s.Client.Do(ctx, "POST", claim, body, nil, nil); err != nil {
		return false, err
	}
	budget := s.Config.Task(kind)
	// Worker owns its execution budget and the separate conclusion deadline.
	// A dispatcher deadline measured from container startup could abort before
	// a long current turn reaches the boundary where soft conclusion begins.
	t := &task{Job: worker.Job{RunID: id, Kind: kind, WorkerType: w.Type, Graph: g, Intent: intent, Budget: budget, Workspace: "/workspace", GraphRPC: true, ResultContractVersion: 2, DecisionRevision: s.stateRevisions[g.Project.ID], EnvironmentID: s.environmentID(*w)}, Worker: *w, Lease: lease}
	if kind == "reason" {
		t.Job.DecisionTrigger = trigger
		t.urgentInput = s.reasonWaits[g.Project.ID].Urgent
	}
	if err := s.register(ctx, t); err != nil {
		_ = s.Client.Do(ctx, "POST", s.leasePath(t)+"/release", map[string]string{"worker": lease.Run}, nil, nil)
		return false, err
	}
	s.start(ctx, t)
	if kind == "reason" {
		delete(s.reasonWaits, g.Project.ID)
	}
	if kind == "curate" {
		delete(s.curationWaits, g.Project.ID)
	}
	if kind == "reason" || kind == "curate" {
		delete(s.controlConflicts, g.Project.ID)
	}
	return true, nil
}
func (s *Scheduler) runTask(ctx context.Context, t *task) (outcome string, runErr error) {
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
			s.terminal(t, "cancelled", worker.Result{Status: "failed", FailureKind: "dependency_invalidated", Error: cause.Error()})
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
				s.terminal(t, "failed", worker.Result{Status: "failed", FailureKind: "state_changed", Error: cause.Error()})
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
