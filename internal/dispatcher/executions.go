package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/worker"
)

func digest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (s *Scheduler) namespace() string {
	if s.Config.Container.Namespace == "" {
		return "pwnmesh"
	}
	return s.Config.Container.Namespace
}
func (s *Scheduler) environmentID(w config.Worker) string {
	// Credentials are intentionally excluded; rotating a token must not change
	// the task. Provider/model/container configuration is part of its identity.
	env := map[string]string{}
	for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "PWNMESH_REASONING_EFFORT", "PWNMESH_MAX_OUTPUT_TOKENS", "PWNMESH_CONTEXT_BYTES", "PWNMESH_REQUEST_TIMEOUT"} {
		env[key] = w.Env[key]
	}
	// Keep existing identities stable when these newer settings are absent.
	for _, key := range []string{"PWNMESH_CONTEXT_TOKENS", "PWNMESH_CONTEXT_TARGET_TOKENS"} {
		if value := w.Env[key]; value != "" {
			env[key] = value
		}
	}
	return digest(struct {
		Type    string
		Image   string
		Network string
		Caps    []string
		Env     map[string]string
	}{w.Type, s.Config.Container.Image, s.Config.Container.Network, s.Config.Container.CapAdd, env})
}

func (s *Scheduler) releaseIdleAdmissions() {
	for project := range s.admitted {
		busy := false
		for _, t := range s.running {
			if t.Job.Graph.Project.ID == project {
				busy = true
				break
			}
		}
		if !busy {
			delete(s.admitted, project)
		}
	}
}
func (s *Scheduler) retryKey(g board.Graph, kind string, intent *board.Intent) string {
	if kind == "curate" {
		return s.schedules[g.Project.ID].CurationRetryKey
	}
	if kind != "reason" {
		if intent == nil {
			return kind
		}
		return kind + ":" + intent.ID
	}
	if input, ok := s.schedules[g.Project.ID]; ok {
		return input.RetryKey
	}
	return board.DecisionRetryKey(g, s.stateRevisions[g.Project.ID])
}

// Scheduling asks the registry only about this input. Historical Jobs and
// Results remain on the Server and never accumulate in a dispatcher snapshot.
func (s *Scheduler) executionCheck(ctx context.Context, g board.Graph, kind string, intent *board.Intent, stateVersion string) (board.ExecutionCheck, error) {
	query := url.Values{"namespace": {s.namespace()}, "generation": {strconv.FormatInt(g.Project.Generation, 10)}, "kind": {kind}, "retry_key": {s.retryKey(g, kind, intent)}}
	if intent != nil {
		query.Set("intent", intent.ID)
	}
	if stateVersion != "" {
		query.Set("state_version", stateVersion)
	}
	var check board.ExecutionCheck
	err := s.Client.Do(ctx, "GET", projectPath(g.Project.ID)+"/executions/check?"+query.Encode(), nil, &check, nil)
	return check, err
}

func (s *Scheduler) candidateCheck(ctx context.Context, input board.SchedulePage, g board.Graph, kind string, intent *board.Intent) (board.ExecutionCheck, error) {
	intentID := ""
	if intent != nil {
		intentID = intent.ID
	}
	if check, ok := input.ExecutionChecks[kind+":"+intentID]; ok {
		return check, nil
	}
	// Targeted callers may not carry the precomputed scheduling checks.
	return s.executionCheck(ctx, g, kind, intent, "")
}

func (s *Scheduler) restoreDecisionBoundary(project string, latest *board.ExecutionSummary) {
	if latest == nil || latest.Generation != s.generations[project] {
		return
	}
	s.checkpoints[project] = checkpoint{latest.FactCount, latest.HintCount, latest.OpenCount}
	s.decisionRevisions[project] = latest.DecisionRevision
}

func (s *Scheduler) loadExecutions(ctx context.Context) error {
	s.pendingExecutions = nil
	var after int64
	for {
		var page board.ExecutionPage
		query := url.Values{"namespace": {s.namespace()}, "after": {strconv.FormatInt(after, 10)}, "limit": {"100"}}
		if err := s.Client.Do(ctx, "GET", "/executions/pending?"+query.Encode(), nil, &page, nil); err != nil {
			return err
		}
		s.pendingExecutions = append(s.pendingExecutions, page.Items...)
		if page.NextCursor == 0 {
			return nil
		}
		if page.NextCursor <= after {
			return errors.New("execution pending cursor did not advance")
		}
		after = page.NextCursor
	}
}

func (s *Scheduler) register(ctx context.Context, t *task) error {
	// New runs have one server-owned immutable snapshot. Templates never carry
	// graph data; recovery continues to use the exact registered Job below.
	t.Job.Graph = board.Graph{Project: t.Job.Graph.Project}
	t.Job.State = nil
	raw, err := json.Marshal(t.Job)
	if err != nil {
		return err
	}
	e := board.Execution{ProjectID: t.Job.Graph.Project.ID, ID: t.Job.RunID, Namespace: s.namespace(), Backend: t.Worker.Name, Kind: t.Job.Kind, Intent: t.Lease.Intent, Lease: t.Lease.Run, Job: raw, RetryKey: s.retryKey(t.Job.Graph, t.Job.Kind, t.Job.Intent)}
	if err = s.Client.Do(ctx, "POST", projectPath(e.ProjectID)+"/executions/prepare", e, &t.Execution, &t.Lease); err != nil {
		return err
	}
	// The HTTP decoder canonicalizes embedded JSON objects. Run exactly the
	// stored job so RawMessage ordering cannot change the identity on recovery.
	if err = json.Unmarshal(t.Execution.Job, &t.Job); err != nil {
		return err
	}
	return nil
}
func (s *Scheduler) start(ctx context.Context, t *task) {
	// Dispatcher shutdown is a recoverable interruption. A project stop or
	// lease loss is a hard cancellation. Detach parent cancellation only long
	// enough to relay this distinction to the Docker process controller.
	runCtx, cancelCause := context.WithCancelCause(context.WithoutCancel(ctx))
	cancel := func() {
		if ctx.Err() != nil {
			cancelCause(worker.ErrInterrupted)
		} else {
			cancelCause(context.Canceled)
		}
	}
	t.Cancel = cancel
	t.Root = ctx
	t.LeaseTimeout = s.leaseTimeout
	go func() {
		select {
		case <-ctx.Done():
			cancelCause(worker.ErrInterrupted)
		case <-runCtx.Done():
		}
	}()
	s.running[t.Job.RunID] = t
	s.admitted[t.Job.Graph.Project.ID] = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		outcome, err := s.runTask(runCtx, t)
		s.done <- finished{t, outcome, err}
		s.wake()
	}()
}
func executionPath(t *task) string {
	return projectPath(t.Job.Graph.Project.ID) + "/executions/" + url.PathEscape(t.Job.RunID)
}
func (s *Scheduler) status(ctx context.Context, t *task, status string, result worker.Result) error {
	err := s.Client.Do(ctx, "POST", executionPath(t)+"/status", map[string]any{"status": status, "result": result}, nil, &t.Lease)
	if err == nil && status == "result_pending" {
		t.Execution.Status = status
	}
	return err
}
func (s *Scheduler) terminal(t *task, status string, result worker.Result) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.status(ctx, t, status, result); err == nil && status == "failed" && result.FailureKind == "state_changed" && immutableInputVersion(t) != "" {
		t.staleInputAt = time.Now()
	}
}
func (s *Scheduler) recoverExecutions(ctx context.Context, states map[string]string) error {
	for run, until := range s.deliveryWaits {
		if !time.Now().Before(until) {
			delete(s.deliveryWaits, run)
		}
	}
	// Resume control work before producers after a dispatcher restart. No
	// business plan is changed; these are already registered executions.
	sort.SliceStable(s.pendingExecutions, func(i, j int) bool {
		control := func(kind string) bool { return kind == "reason" || kind == "curate" }
		return control(s.pendingExecutions[i].Kind) && !control(s.pendingExecutions[j].Kind)
	})
	for _, e := range s.pendingExecutions {
		if !e.Pending() || s.running[e.ID] != nil || !slices.Contains([]string{"reason", "curate", "explore"}, e.Kind) {
			continue
		}
		project := board.Project{ID: e.ProjectID, Generation: e.Generation}
		t := &task{Job: worker.Job{RunID: e.ID, Graph: board.Graph{Project: project}}, Lease: Lease{Run: e.Lease, Kind: e.Kind, Intent: e.Intent}}
		if states[e.ProjectID] != "active" || e.Generation != s.generations[e.ProjectID] {
			s.terminal(t, "cancelled", worker.Result{Status: "failed", FailureKind: "hard_cancelled", Error: "project is not active"})
			continue
		}
		if until, waiting := s.deliveryWaits[e.ID]; waiting {
			s.wakeAt(until)
			continue
		}
		if !s.restartReady(ctx, project) {
			continue
		}
		if len(s.running) >= s.Config.Runtime.MaxWorkers {
			break
		}
		if !s.admitted[e.ProjectID] && len(s.admitted) >= s.Config.Runtime.MaxProjects {
			continue
		}
		projectCount, backendCount := 0, 0
		staleRound := false
		for _, running := range s.running {
			if running.Job.Graph.Project.ID == e.ProjectID {
				projectCount++
				staleRound = staleRound || running.Job.Graph.Project.Generation != e.Generation
			}
			if running.Worker.Name == e.Backend {
				backendCount++
			}
		}
		if staleRound || projectCount >= s.Config.Runtime.MaxProjectWorkers {
			continue
		}
		if err := s.Client.Do(ctx, "GET", executionPath(t)+"?namespace="+url.QueryEscape(s.namespace()), nil, &t.Execution, nil); err != nil {
			var pe *ProtocolError
			if errors.As(err, &pe) && pe.Status == 404 {
				continue // A project restart may have removed this page's record.
			}
			return err
		}
		if !t.Execution.Pending() {
			continue
		}
		if err := json.Unmarshal(t.Execution.Job, &t.Job); err != nil {
			return fmt.Errorf("execution %s has invalid job: %w", e.ID, err)
		}
		if t.Job.Graph.Project.OrchestrationVersion != 1 || !t.Job.GraphRPC || t.Job.ResultContractVersion != 2 {
			continue // Retained legacy inputs are history, never runnable jobs.
		}
		// A saved result only needs its registered lease and input fences. Its
		// original model/backend may have been removed since execution finished.
		t.Worker.Name = e.Backend
		if t.Execution.Status != "result_pending" {
			for _, w := range s.Config.Workers {
				if w.Name == e.Backend {
					t.Worker = w
					break
				}
			}
			if t.Worker.Type != "go" || !slices.Contains(t.Worker.TaskTypes, e.Kind) || t.Job.EnvironmentID != s.environmentID(t.Worker) {
				s.terminal(t, "failed", worker.Result{Status: "failed", FailureKind: "configuration", Error: "registered execution environment changed"})
				continue
			}
			if backendCount >= t.Worker.MaxRunning || e.Kind == "explore" && (!s.executionCapacity(e.ProjectID) || !s.backendExecutionCapacity(t.Worker.Name, t.Worker.MaxRunning)) {
				continue
			}
		}
		// Readiness only gates model work. Results already accepted by the
		// server can finish delivery even while this backend is unavailable.
		until := s.unhealthy[t.Worker.Name]
		if t.Execution.Status != "result_pending" && (s.incompatible[t.Worker.Name] != "" || time.Now().Before(until)) {
			committed, err := s.decisionCommitted(ctx, t)
			if err != nil {
				return err
			}
			if committed {
				continue // Decide commits its execution status atomically.
			}
			if t.Job.Kind == "curate" {
				committed, err = s.curationCommitted(ctx, t)
				if err != nil {
					return err
				}
			}
			if !committed {
				if s.incompatible[t.Worker.Name] == "" {
					s.wakeAt(until)
				}
				continue
			}
		}
		err := s.Client.Do(ctx, "POST", executionPath(t)+"/resume", map[string]any{}, &t.Execution, &t.Lease)
		if err != nil {
			var pe *ProtocolError
			if errors.As(err, &pe) && (pe.Status == 403 || pe.Status == 404 || pe.Status == 409) {
				failure := "session_invalid"
				if pe.Status == 409 && strings.Contains(pe.Detail, "Dispatcher recovery allowance exhausted") {
					failure = "recovery_exhausted"
				}
				s.terminal(t, "failed", worker.Result{Status: "failed", FailureKind: failure, Error: err.Error()})
				continue
			}
			return err
		}
		s.start(ctx, t)
	}
	return nil
}
func (s *Scheduler) runRegistered(ctx context.Context, t *task, stopHeartbeat func()) (string, error) {
	if ok, err := s.decisionCommitted(ctx, t); ok || err != nil {
		if ok {
			return "success", nil
		}
		return "interrupted", err
	}
	if t.Job.Kind == "curate" && t.Execution.Resumes > 0 {
		if committed, err := s.finishCommittedCuration(ctx, t); committed || err != nil {
			if err != nil {
				return "interrupted", err
			}
			return "success", nil
		}
	}
	var result worker.Result
	if t.Execution.Status == "result_pending" {
		if err := json.Unmarshal(t.Execution.Result, &result); err != nil {
			return "failed", err
		}
	}
	if t.Execution.Status != "result_pending" {
		if s.Config.Runtime.HealthMode == "startup_and_task" {
			if err := s.health(ctx, t.Worker, t.Job.Budget); err != nil {
				if ctx.Err() != nil {
					return "cancelled", ctx.Err()
				}
				return "unhealthy", err
			}
		}
		for attempt := 0; attempt <= 2; attempt++ {
			if err := s.status(ctx, t, "running", worker.Result{}); err != nil {
				var pe *ProtocolError
				if errors.As(err, &pe) && (pe.Status == 403 || pe.Status == 404 || pe.Status == 409) {
					s.terminal(t, "cancelled", worker.Result{Status: "failed", FailureKind: "invalidated_before_start", Error: err.Error()})
					return "cancelled", err
				}
				return "interrupted", err
			}
			var err error
			// Container startup and same-run recovery must not issue a request
			// against an input already superseded while the run was queued.
			if immutableInputVersion(t) != "" {
				err = s.renewLeaseWithVersion(ctx, t, true)
			}
			if err == nil {
				result, err = s.Runner.Run(ctx, t.Worker, t.Job)
			}
			if t.Job.Kind == "curate" {
				committed, receiptErr := s.curationCommitted(ctx, t)
				if receiptErr != nil {
					return "interrupted", receiptErr
				}
				if committed {
					// The durable receipt is authoritative even when the model's
					// final response or graph acknowledgement was lost.
					result, err = curationResult(result.Metrics), nil
				}
			}
			if result.Metrics != nil {
				slog.Info("decision observation", "project", t.Job.Graph.Project.ID, "run", t.Job.RunID, "metrics", result.Metrics)
			}
			if committed, receiptErr := s.decisionCommitted(ctx, t); committed || receiptErr != nil {
				if committed {
					s.observeDecision(ctx, t, result.Metrics)
					return "success", nil
				}
				return "interrupted", receiptErr
			}
			if ctx.Err() != nil {
				// Process-wide shutdown leaves a recoverable registry entry. Explicit
				// project/lease cancellation is terminal and cannot mint a fresh attempt.
				if t.Root != nil && t.Root.Err() != nil {
					return "interrupted", ctx.Err()
				}
				if decisionStateChanged(context.Cause(ctx)) {
					return "interrupted", context.Cause(ctx) // runTask persists after joining the heartbeat.
				}
				failure, detail := "hard_cancelled", ctx.Err().Error()
				if cause := context.Cause(ctx); dependencyInvalidated(cause) {
					failure, detail = "dependency_invalidated", cause.Error()
				}
				s.terminal(t, "cancelled", worker.Result{Status: "failed", FailureKind: failure, Error: detail})
				return "cancelled", ctx.Err()
			}
			if err != nil {
				result = worker.Result{Status: "failed", Retryable: true, FailureKind: "transient_infrastructure", Error: err.Error()}
				if decisionStateChanged(err) {
					result.FailureKind, result.Retryable = "state_changed", false
				}
			}
			if result.Status == "success" {
				break
			}
			if !result.Retryable || attempt == 2 {
				result.Retryable = false
				s.terminal(t, "failed", result)
				return "failed", errors.New(result.Error)
			}
			if err = s.status(ctx, t, "retryable", result); err != nil {
				return "interrupted", err
			}
			// The Worker persists its own recovery counter before issuing a request.
			// This short backoff never releases the step lease or resets its deadline.
			timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "interrupted", ctx.Err()
			case <-timer.C:
			}
		}
		if err := s.status(ctx, t, "result_pending", result); err != nil {
			return s.resultDeliveryFailure(ctx, t, result, err)
		}
	}
	// Finish the business transaction before another heartbeat can observe a
	// concluded bootstrap step and cancel its project completion.
	stopHeartbeat()
	var receipt struct {
		Status string `json:"status"`
	}
	err := s.Client.Do(ctx, "POST", executionPath(t)+"/apply", map[string]any{}, &receipt, &t.Lease)
	if err != nil {
		return s.resultDeliveryFailure(ctx, t, result, err)
	}
	if receipt.Status == "rejected" {
		return "rejected", nil
	}
	return "success", nil
}

// Publishing and applying a result share the same failure boundary. Invalid
// output cannot become valid by replaying the saved Worker result; temporary
// delivery failures must leave that result available for same-run recovery.
func (s *Scheduler) resultDeliveryFailure(ctx context.Context, t *task, result worker.Result, err error) (string, error) {
	if decisionStateChanged(context.Cause(ctx)) {
		// A cancelled HTTP call may wrap its heartbeat ProtocolError. Reconcile
		// the receipt after joining the heartbeat before recording a failure.
		return "interrupted", context.Cause(ctx)
	}
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Status < 400 || pe.Status >= 500 || pe.Status == http.StatusRequestTimeout || pe.Status == http.StatusTooManyRequests {
		return "interrupted", err
	}
	status := "failed"
	result.Status, result.Retryable, result.FailureKind, result.Error = "failed", false, "invalid_output", err.Error()
	if decisionStateChanged(err) {
		result.FailureKind = "state_changed"
	} else if dependencyInvalidated(err) {
		status, result.FailureKind = "cancelled", "dependency_invalidated"
	}
	s.terminal(t, status, result)
	return status, err
}

func dependencyInvalidated(err error) bool {
	var pe *ProtocolError
	return errors.As(err, &pe) && pe.Status == 409 && strings.Contains(pe.Detail, "dependency_invalidated:")
}

// Only roles whose entire write is bound to immutable input opt into early
// invalidation. Legacy planners and independent Execute work keep their lease.
func immutableInputVersion(t *task) string {
	if t.Job.Kind == "reason" && t.Job.Decision != nil && t.Job.Decision.Version == 2 {
		return t.Job.Decision.StateVersion
	}
	if t.Job.Kind == "curate" {
		if t.Job.InputSnapshot != nil {
			return t.Job.InputSnapshot.StateVersion
		}
		if t.Job.State != nil {
			return board.DecisionStateVersion(*t.Job.State)
		}
	}
	return ""
}

func decisionStateChanged(err error) bool {
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Status != 409 {
		return false
	}
	var response struct {
		Detail string `json:"detail"`
	}
	return json.Unmarshal([]byte(pe.Detail), &response) == nil && strings.HasPrefix(response.Detail, "state_changed:")
}

// A batch commits the graph and its execution result together. Prefer that
// receipt even if project completion cancelled the process before it replied.
func (s *Scheduler) decisionCommitted(ctx context.Context, t *task) (bool, error) {
	if t.Job.Kind != "reason" || t.Job.Decision == nil || t.Job.Decision.Version != 2 {
		return false, nil
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var receipt board.DecisionReceipt
	err := s.Client.Do(readCtx, "GET", projectPath(t.Job.Graph.Project.ID)+"/state/decisions/receipt", nil, &receipt, &t.Lease)
	if err == nil && receipt.Committed {
		t.committedAt.CompareAndSwap(0, time.Now().UnixNano())
	}
	return receipt.Committed, err
}

const decisionFinishGrace = 10 * time.Second

func (s *Scheduler) decisionFinishAllowed(ctx context.Context, t *task) bool {
	committed, err := s.decisionCommitted(ctx, t)
	if err != nil || !committed || time.Since(time.Unix(0, t.committedAt.Load())) >= decisionFinishGrace {
		return false
	}
	// A human stop or a new generation always overrides this delivery grace.
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var input board.SchedulePage
	err = s.Client.Do(readCtx, "GET", projectPath(t.Job.Graph.Project.ID)+"/scheduling", nil, &input, nil)
	return err == nil && input.Project.Generation == t.Job.Graph.Project.Generation && (input.Project.Status == "active" || input.Project.Status == "completed")
}

func (s *Scheduler) observeDecision(ctx context.Context, t *task, metrics *worker.DecisionMetrics) {
	if metrics == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Client.Do(writeCtx, "POST", executionPath(t)+"/observation", map[string]any{"metrics": metrics}, nil, &t.Lease); err != nil {
		slog.Warn("decision observation could not be saved", "project", t.Job.Graph.Project.ID, "run", t.Job.RunID, "error", err)
	}
}

func (s *Scheduler) configureGraphHandler() {
	setter, ok := s.Runner.(interface {
		SetGraphHandler(func(context.Context, worker.Job, worker.GraphRequest) (any, error))
	})
	if !ok {
		return
	}
	setter.SetGraphHandler(func(ctx context.Context, j worker.Job, request worker.GraphRequest) (any, error) {
		// The job, not model-generated fields, supplies project and lease identity.
		// Worker identity is registered on the Server; avoid reading Scheduler maps
		// from Runner goroutines, which would race with dispatch/reaping.
		var identity board.ExecutionSummary
		path := projectPath(j.Graph.Project.ID) + "/executions/" + url.PathEscape(j.RunID) + "/identity?namespace=" + url.QueryEscape(s.namespace())
		if err := s.Client.Do(ctx, "GET", path, nil, &identity, nil); err != nil {
			return nil, err
		}
		intent := ""
		if j.Intent != nil {
			intent = j.Intent.ID
		}
		if identity.ProjectID != j.Graph.Project.ID || identity.Generation != j.Graph.Project.Generation || identity.ID != j.RunID || identity.Namespace != s.namespace() || identity.Kind != j.Kind || identity.Intent != intent || identity.Lease == "" {
			return nil, errors.New("graph request has no registered execution")
		}
		lease := Lease{Run: identity.Lease, Kind: identity.Kind, Intent: identity.Intent}
		base := projectPath(j.Graph.Project.ID)
		switch request.Op {
		case "curate_receipt":
			var result board.StateActionResult
			err := s.Client.Do(ctx, "GET", base+"/state/curation/receipt", nil, &result, &lease)
			var pe *ProtocolError
			if errors.As(err, &pe) && pe.Status == 404 {
				return board.StateActionResult{}, nil
			}
			result.Result = nil
			return result, err
		case "decision_preview", "decision_commit", "decision_receipt":
			var result board.DecisionReceipt
			op := strings.TrimPrefix(request.Op, "decision_")
			method := "POST"
			var body any = request.Batch
			if op == "receipt" {
				method, body = "GET", nil
			}
			err := s.Client.Do(ctx, method, base+"/state/decisions/"+op, body, &result, &lease)
			if err == nil && op == "commit" && result.Committed && result.ChangedActions > 0 {
				// Authorized work need not wait for the planner's final model
				// response or the polling tick. Only Step reads scheduler maps.
				s.wake()
			}
			// Commit and recovery need only a compact acknowledgement. Completion
			// previews retain the authoritative review instead of repeating every
			// projected entity, which could exceed the graph bridge frame.
			if result.Committed || result.CompletionReview != nil {
				result.Results = nil
			}
			return result, err
		case "read_graph", "read_snapshot", "read_updates", "read_trace_runs":
			// Bound the HTTP response too: a current FGS may exceed the client
			// limit even though the requested graph/evidence page is small.
			var page json.RawMessage
			path := base + "/state/read"
			if request.Op == "read_snapshot" {
				path = base + "/executions/" + url.PathEscape(j.RunID) + "/input/read"
			} else if request.Op == "read_updates" {
				path = base + "/executions/" + url.PathEscape(j.RunID) + "/updates"
			} else if request.Op == "read_trace_runs" {
				path = base + "/executions/" + url.PathEscape(j.RunID) + "/traces/read"
			}
			err := s.Client.Do(ctx, "POST", path, request, &page, &lease)
			return page, err
		case "graph_action":
			var result board.StateActionResult
			err := s.Client.Do(ctx, "POST", base+"/state/actions", request.Action, &result, &lease)
			if err == nil && !result.Unchanged {
				// Intermediate observations start the existing coalescing window
				// promptly; they do not bypass readiness or dependency checks.
				s.wake()
			}
			return result, err
		default:
			return nil, errors.New("unknown graph request")
		}
	})
}
