package board

import (
	"database/sql"
	"encoding/json"
	"errors"
	"pwnmesh/internal/board/internal/sqlstore"
	"pwnmesh/internal/board/model"
	"slices"
)

// ExecutionSummary is the scheduling identity and immutable input boundary.
// It deliberately contains neither the saved Job nor the business Result.
type ExecutionSummary = model.ExecutionSummary

type ExecutionPage = model.ExecutionPage

type ExecutionCheckQuery struct {
	ProjectID, Namespace, Kind, Intent, RetryKey, StateVersion string
	Generation                                                 int64
}

type ExecutionCheck = model.ExecutionCheck

func executionInputMetadata(raw json.RawMessage) (ExecutionSummary, error) {
	var job struct {
		Graph            Graph          `json:"graph"`
		State            *State         `json:"state"`
		DecisionRevision int64          `json:"decision_revision"`
		InputSnapshot    *InputSnapshot `json:"input_snapshot"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return ExecutionSummary{}, err
	}
	e := ExecutionSummary{Generation: job.Graph.Project.Generation, DecisionRevision: job.DecisionRevision,
		FactCount: len(job.Graph.Facts), HintCount: len(job.Graph.Hints), OpenCount: job.Graph.OpenCount(), HasState: job.State != nil}
	if job.State != nil {
		e.InputRevision = job.State.Revision
		// Legacy jobs can contain State without Decision. Repeated-input checks
		// must hash that state too, rather than depending on a protocol marker.
		e.StateVersion = DecisionStateVersion(*job.State)
	}
	if ref := job.InputSnapshot; ref != nil {
		e.Generation, e.InputRevision, e.DecisionRevision = ref.Generation, ref.Revision, ref.DecisionRevision
		e.StateVersion, e.HasState = ref.StateVersion, true
		e.FactCount, e.HintCount, e.OpenCount = ref.FactCount, ref.HintCount, ref.OpenCount
	}
	return e, nil
}

func (t *Tx) PendingExecutions(namespace string, after int64, limit int) (ExecutionPage, error) {
	if namespace == "" || after < 0 || limit < 1 || limit > 100 {
		return ExecutionPage{Items: []ExecutionSummary{}}, Err(422, "namespace, nonnegative after and limit between 1 and 100 are required")
	}
	return sqlstore.PendingExecutions(t, namespace, after, limit)
}

func (t *Tx) GetExecutionSummary(namespace, project, id string) (ExecutionSummary, error) {
	e, err := sqlstore.ExecutionSummary(t, namespace, project, id)
	if errors.Is(err, sql.ErrNoRows) {
		err = Err(404, "Execution not found")
	}
	return e, err
}

func (t *Tx) CheckExecutions(q ExecutionCheckQuery) (ExecutionCheck, error) {
	var out ExecutionCheck
	// Existing pending work and human grants remain visible across generations;
	// recovery is responsible for cancelling stale pending work.
	admission, err := sqlstore.ReadExecutionAdmission(t, sqlstore.ExecutionAdmissionFilter{
		ProjectID: q.ProjectID, Namespace: q.Namespace, Generation: q.Generation,
		ExecutionCandidate: sqlstore.ExecutionCandidate{Kind: q.Kind, Intent: q.Intent, RetryKey: q.RetryKey},
		AnyPendingIntent:   q.Kind == "reason", GrantMatchesInput: q.Kind == "curate",
	})
	out.Pending, out.PreviousRunID = admission.Pending, admission.PreviousRunID
	if err != nil {
		return out, err
	}
	out.Blocked = out.Pending || (admission.Tried && out.PreviousRunID == "")
	if !out.Blocked && q.Kind == "explore" {
		out.Blocked, err = t.stepWriteBlocked(q.ProjectID, q.Intent)
		if err != nil {
			return out, err
		}
	}
	out.Attempts, err = sqlstore.ExecutionAttempts(t, q.Namespace, q.ProjectID, q.Kind, q.RetryKey, q.Generation)
	if err != nil {
		return out, err
	}
	if q.StateVersion != "" {
		// The state hash already binds its own generation. Old jobs without a
		// Decision marker could carry a State whose graph differs from Job.Graph;
		// repeated-input detection historically followed that State, not the job.
		out.Repeated, err = sqlstore.ExecutionStateExists(t, q.Namespace, q.ProjectID, "reason", q.StateVersion)
		if err != nil {
			return out, err
		}
	}
	if q.Kind == "reason" {
		latest, err := sqlstore.LatestSucceededExecution(t, q.Namespace, q.ProjectID, "reason", q.Generation)
		if err == nil {
			out.LatestDecision = &latest
		} else if !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
	}
	if !controlKind(q.Kind) || out.Attempts != 1 || out.Pending || out.PreviousRunID != "" {
		return out, nil
	}
	// Extract only the small failure classification. Never materialize Result
	// or Job just to decide whether this one candidate merits a retry request.
	id, status, failure, err := sqlstore.ExecutionInputFailure(t, q.Namespace, q.ProjectID, q.Kind, q.RetryKey, q.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if automaticDecisionRetryEligible(q.Kind, status, failure.ResultStatus, failure.Kind, failure.Cause) {
		out.AutomaticRetryID = id
	}
	return out, nil
}

// Scheduling needs only admission and retry grants for the current page's
// Execute candidates. Registration still verifies their leases and identity.
func (t *Tx) ScheduleExecutionChecks(project, namespace string, steps []ScheduleStep) (map[string]ExecutionCheck, error) {
	checks := make(map[string]ExecutionCheck, len(steps))
	if len(steps) == 0 {
		return checks, nil
	}
	var writeOwners []Step
	if slices.ContainsFunc(steps, func(step ScheduleStep) bool { return step.Ready && len(step.WritePaths) != 0 }) {
		data, _, _, err := t.stateData(project)
		if err != nil {
			return nil, err
		}
		writeOwners, err = t.activeStepWriteOwners(project, data.Steps)
		if err != nil {
			return nil, err
		}
	}
	candidates := make([]sqlstore.ExecutionCandidate, 0, len(steps))
	for _, step := range steps {
		if !step.Ready {
			continue
		}
		if stepWriteConflict(Step{ID: step.ID, WritePaths: step.WritePaths}, writeOwners) != "" {
			checks["explore:"+step.ID] = ExecutionCheck{Blocked: true}
			continue
		}
		candidates = append(candidates, sqlstore.ExecutionCandidate{Kind: "explore", Intent: step.ID, RetryKey: "explore:" + step.ID})
	}
	admissions, err := sqlstore.ReadExecutionAdmissions(t, project, namespace, candidates)
	if admissions == nil {
		return nil, err
	}
	for key, admission := range admissions {
		checks[key] = ExecutionCheck{Pending: admission.Pending, PreviousRunID: admission.PreviousRunID,
			Blocked: admission.Pending || (admission.Tried && admission.PreviousRunID == "")}
	}
	return checks, err
}
