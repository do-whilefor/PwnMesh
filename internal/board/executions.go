package board

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"pwnmesh/internal/board/internal/sqlstore"
	"pwnmesh/internal/board/model"
	"slices"
	"strings"
)

// Execution contains no backend environment or model credentials. Job is the
// immutable input supplied to the Worker, not a newly loaded project snapshot.
type Execution = model.Execution

func (t *Tx) Execution(project, id string) (Execution, error) {
	e, err := sqlstore.Execution(t.transactionQueries, project, id)
	if errors.Is(err, sql.ErrNoRows) {
		err = Err(404, "Execution not found")
	}
	return e, err
}
func (t *Tx) RegisterExecution(e Execution) (err error) {
	var snapshotJob struct {
		InputSnapshot *InputSnapshot `json:"input_snapshot"`
		Graph         Graph          `json:"graph"`
	}
	if err := json.Unmarshal(e.Job, &snapshotJob); err != nil {
		return Err(422, "invalid execution job")
	}
	if ref := snapshotJob.InputSnapshot; ref != nil {
		saved, err := t.InputSnapshotMetadata(e.ProjectID, ref.ID)
		if err != nil {
			return err
		}
		if *saved != *ref {
			return Err(422, "execution snapshot binding mismatch")
		}
	}
	if _, err := DecisionJobVersion(e.Job); err != nil {
		return err
	}
	metadata, err := executionInputMetadata(e.Job)
	if err != nil {
		return Err(422, "invalid execution job")
	}
	g, err := t.Load(e.ProjectID)
	if err != nil {
		return err
	}
	if err = g.RequireActive(); err != nil {
		return err
	}
	// A new lease can race a restart after the dispatcher read its input. Check
	// the immutable job's round, even if that newly claimed lease is valid.
	if metadata.Generation != g.Project.Generation {
		return Err(409, "Execution input belongs to a previous project round")
	}
	if g.Project.OrchestrationVersion != 1 {
		return Err(409, "execution registration requires orchestration version 1")
	}
	var policy struct {
		Graph                 Graph  `json:"graph"`
		Kind                  string `json:"kind"`
		GraphRPC              bool   `json:"graph_rpc"`
		ResultContractVersion int    `json:"result_contract_version"`
		WorkerType            string `json:"worker_type"`
		Decision              *struct {
			Version int `json:"version"`
		} `json:"decision"`
	}
	if json.Unmarshal(e.Job, &policy) != nil || policy.Graph.Project.OrchestrationVersion != 1 || policy.Kind != e.Kind || !policy.GraphRPC || policy.ResultContractVersion != 2 || policy.WorkerType == "mock" || !slices.Contains([]string{"reason", "curate", "explore"}, e.Kind) || (e.Kind == "reason" && (policy.Decision == nil || policy.Decision.Version != 2)) {
		return Err(422, "orchestration version 1 requires graph RPC, evidence result version 2 and registered role protocols")
	}
	if e.Kind == "curate" {
		if len(e.Job) > MaxCurationInputBytes {
			return Err(422, "curation job exceeds 256 KiB; use a bounded snapshot input")
		}
		version, _, err := CurationJobBoundary(e.Job)
		if err != nil {
			return err
		}
		if e.Intent != "" || snapshotJob.Graph.Project.ID != e.ProjectID || g.Project.OrchestrationVersion != 1 || e.RetryKey != "curate:"+version {
			return Err(422, "invalid registered curation input")
		}
	}
	if err = t.CheckExecution(g, e.Fence()); err != nil {
		return err
	}
	if e.ID == "" || e.Lease != e.Backend+"@"+e.ID || e.RetryKey == "" || e.Namespace == "" {
		return Err(422, "invalid execution identity")
	}
	if !controlKind(e.Kind) {
		if e.RetryKey != e.Kind+":"+e.Intent {
			return Err(422, "Execute retry key must be bound to its step")
		}
	}
	prior, err := t.Execution(e.ProjectID, e.ID)
	if err == nil {
		if prior.Namespace != e.Namespace || prior.Backend != e.Backend || prior.Kind != e.Kind || prior.Intent != e.Intent || prior.Lease != e.Lease || string(prior.Job) != string(e.Job) || prior.RetryKey != e.RetryKey {
			return Err(409, "Execution input is immutable")
		}
		return nil
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 {
		return err
	}
	if !controlKind(e.Kind) {
		if err = t.StepReady(e.ProjectID, e.Intent); err != nil {
			return err
		}
		if err = t.CheckExecutionDependencies(e); err != nil {
			return err
		}
		if err = t.validateReviewExecution(e); err != nil {
			return err
		}
	}
	var job struct {
		PreviousRunID string `json:"previous_run_id"`
	}
	if err = json.Unmarshal(e.Job, &job); err != nil {
		return Err(422, "invalid execution job")
	}
	var grant *ExecutionSummary
	if job.PreviousRunID != "" {
		previous, err := t.GetExecutionSummary(e.Namespace, e.ProjectID, job.PreviousRunID)
		if err != nil {
			return err
		}
		if previous.Status != "retry_requested" || previous.Namespace != e.Namespace || previous.Kind != e.Kind || previous.Intent != e.Intent {
			return Err(409, "previous_run_id does not identify an available retry for this task")
		}
		if e.Kind == "curate" && (previous.RetryKey != e.RetryKey || previous.Generation != metadata.Generation || previous.StateVersion != metadata.StateVersion) {
			return CodedErr(409, CodeStateChanged, "state_changed: curation retry grant belongs to a different input")
		}
		grant = &previous
	}
	blocked, err := sqlstore.ExecutionRetryKeyRecorded(t.transactionQueries, e.ProjectID, e.Namespace, e.RetryKey)
	if err != nil {
		return err
	}
	if blocked && grant == nil {
		return Err(409, "Execution requires new task evidence or an explicit retry")
	}
	// Consume the one-use authorization in the same transaction as the new
	// registration and its review transition. Keep this operation atomic even
	// when the caller handles its error and commits unrelated work.
	return sqlstore.Atomic(t.transactionQueries, func() error {
		e.Status, e.Result, e.Resumes = "prepared", nil, 0
		e.CreatedAt, e.UpdatedAt = t.Now, t.Now
		err := sqlstore.InsertExecution(t.transactionQueries, e, metadata, job.PreviousRunID)
		if err == nil && !controlKind(e.Kind) {
			err = t.reviewExecutionTransition(e, "prepared")
		}
		return err
	})
}
func (t *Tx) ExecutionStatus(e Execution, status string, result json.RawMessage) error {
	// Re-read rather than trusting a caller's earlier snapshot. State transitions
	// and pending-result immutability are enforced inside the same transaction.
	current, err := t.Execution(e.ProjectID, e.ID)
	if err != nil {
		return err
	}
	if len(result) == 0 {
		result = current.Result
	}
	if current.Status != "result_pending" && len(current.Result) != 0 {
		// Management and metadata-only recovery can replace a result with a
		// cancellation diagnostic. Preserve its independent observed usage;
		// business content and failure classification still come from the update.
		var update, previous map[string]json.RawMessage
		if json.Unmarshal(result, &update) == nil && update != nil && json.Unmarshal(current.Result, &previous) == nil {
			missing := func(raw json.RawMessage) bool {
				return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
			}
			if missing(update["metrics"]) && !missing(previous["metrics"]) {
				update["metrics"] = previous["metrics"]
				result, err = json.Marshal(update)
				if err != nil {
					return err
				}
			}
		}
	}
	failure := result
	if current.Status == status && bytes.Equal(current.Result, result) {
		return nil
	}
	if !slices.Contains([]string{"running", "retryable", "result_pending", "failed", "cancelled", "rejected", "succeeded", "retry_requested"}, status) {
		return Err(422, "invalid execution status")
	}
	if status == "retry_requested" {
		if current.Status != "failed" && current.Status != "rejected" && current.Status != "cancelled" {
			return Err(409, "only a failed, rejected or cancelled execution can authorize a new attempt")
		}
	} else if !current.Pending() {
		return Err(409, "execution is terminal")
	}
	if current.Status == "result_pending" {
		if status == "failed" || status == "cancelled" {
			// A failed delivery or cancellation terminates this attempt, but
			// cannot replace the already durable business result with an error.
			result = current.Result
		}
		if !bytes.Equal(current.Result, result) || status == "running" || status == "retryable" {
			return Err(409, "pending result is immutable and cannot return to execution")
		}
	}
	if status == "succeeded" && current.Status != "result_pending" {
		return Err(409, "only a pending result can be applied successfully")
	}
	if status == "running" && current.Status != "running" && !controlKind(current.Kind) {
		if err = t.StepReady(current.ProjectID, current.Intent); err != nil {
			return err
		}
		if err = t.CheckExecutionDependencies(current); err != nil {
			return err
		}
	}
	if status == "succeeded" && !controlKind(current.Kind) {
		if err = t.CheckExecutionDependencies(current); err != nil {
			return err
		}
	}
	updated := current
	updated.Status, updated.Result, updated.UpdatedAt = status, result, t.Now
	return sqlstore.Atomic(t.transactionQueries, func() error {
		if err := sqlstore.SaveExecutionOutcome(t.transactionQueries, updated, slices.Contains([]string{"failed", "rejected", "cancelled", "succeeded"}, status)); err != nil {
			return err
		}
		if !controlKind(current.Kind) && slices.Contains([]string{"failed", "rejected", "cancelled"}, status) {
			if err := t.recordExecutionFailure(current, status, failure); err != nil {
				return err
			}
			return t.reviewExecutionTransition(current, status)
		}
		return nil
	})
}

// Resume reclaims only an unowned or same-owner lease. A human stop or another
// execution's lease cannot be overridden by a saved transcript.
func (t *Tx) ResumeExecution(e Execution) error {
	current, err := t.Execution(e.ProjectID, e.ID)
	if err != nil {
		return err
	}
	e = current
	if !e.Pending() {
		return Err(409, "Execution is terminal")
	}
	// Reclaiming a lease to apply an immutable result does not restart a
	// Worker. Temporary delivery failures must not consume execution recovery.
	resume := 0
	if e.Status != "result_pending" {
		resume = 1
	}
	if resume != 0 && e.Resumes >= 2 {
		return CodedErr(409, CodeRecoveryExhausted, "Dispatcher recovery allowance exhausted")
	}
	g, err := t.Load(e.ProjectID)
	if err != nil {
		return err
	}
	if err = g.RequireActive(); err != nil {
		return err
	}
	revoked, err := t.RunRevoked(e.ProjectID, e.Lease)
	if err != nil {
		return err
	}
	if revoked {
		return Err(409, "Execution was revoked")
	}
	if e.Kind == "curate" {
		if g.Project.OrchestrationVersion != 1 {
			return Err(409, "curation protocol is no longer available")
		}
		if g.Project.Curator != nil && g.Project.Curator.Worker != e.Lease {
			return Err(409, "Another execution owns curation")
		}
		if g.Project.Curator == nil {
			g.Project.Curator = &Reason{Worker: e.Lease, Trigger: "recovery", StartedAt: e.CreatedAt, Heartbeat: t.Now}
		} else {
			g.Project.Curator.Heartbeat = t.Now
		}
	} else if e.Kind == "reason" {
		if g.Project.Reason != nil && g.Project.Reason.Worker != e.Lease {
			return Err(409, "Another execution owns the decision")
		}
		if g.Project.Reason == nil {
			g.Project.Reason = &Reason{Worker: e.Lease, Trigger: "recovery", StartedAt: e.CreatedAt, Heartbeat: t.Now}
		} else {
			g.Project.Reason.Heartbeat = t.Now
		}
	} else {
		if err = t.StepAvailable(e.ProjectID, e.Intent); err != nil {
			return err
		}
		if err = t.CheckExecutionDependencies(e); err != nil {
			return err
		}
		if e.Status != "result_pending" {
			if err = t.StepReady(e.ProjectID, e.Intent); err != nil {
				return err
			}
		}
		found := false
		for _, i := range g.Intents {
			if i.ID != e.Intent {
				continue
			}
			found = true
			if i.To != nil || i.ConcludedAt != nil {
				return Err(409, "Step has already ended")
			}
			if i.Worker != nil && *i.Worker != e.Lease {
				return Err(409, "Another execution owns the step")
			}
		}
		if !found {
			return Err(404, "Step not found")
		}
	}
	// Reclaiming authority and consuming recovery allowance are one operation.
	// Only the selected lease changes; retained graph content stays untouched.
	return sqlstore.Atomic(t.transactionQueries, func() error {
		var err error
		switch e.Kind {
		case "reason":
			err = sqlstore.SaveReasonLease(t.transactionQueries, e.ProjectID, g.Project.Reason)
		case "curate":
			err = sqlstore.SaveCuratorLease(t.transactionQueries, e.ProjectID, *g.Project.Curator)
		default:
			err = sqlstore.SaveIntentLease(t.transactionQueries, e.ProjectID, e.Intent, e.Lease, t.Now)
		}
		if err != nil {
			return err
		}
		return sqlstore.AddExecutionResume(t.transactionQueries, e.ProjectID, e.ID, t.Now, resume)
	})
}

// CheckNewStepLimit counts every direction this decision already created,
// including subsequently abandoned steps. Tool submissions and the final
// structured result share one registered budget.
func (t *Tx) CheckNewStepLimit(project, run string) error {
	raw, err := sqlstore.LatestExecutionJob(t.transactionQueries, project, run, "reason")
	if errors.Is(err, sql.ErrNoRows) {
		return nil // Legacy clients may use leases without execution registration.
	}
	if err != nil {
		return err
	}
	var job struct {
		Budget struct {
			MaxIntents int `json:"max_intents"`
		} `json:"budget"`
	}
	if err = json.Unmarshal(raw, &job); err != nil {
		return err
	}
	if job.Budget.MaxIntents <= 0 {
		return Err(422, "registered decision has no valid direction budget")
	}
	count, err := sqlstore.CreatedDirectionCount(t.transactionQueries, project, run)
	if err != nil {
		return err
	}
	if count >= job.Budget.MaxIntents {
		return Err(409, "decision reached its new-step budget")
	}
	return nil
}

func ValidExecutionID(id string) bool {
	return required(id, 128) && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00\r\n")
}
