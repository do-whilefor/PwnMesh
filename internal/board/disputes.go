package board

import (
	"encoding/json"
	"slices"

	"pwnmesh/internal/board/internal/sqlstore"
)

const MaxDisputeReviews = 2

func (t *Tx) validateReviewStep(s State, d *stateData, id string) error {
	if s.Graph.Project.OrchestrationVersion != 1 {
		return Err(422, "review task requires orchestration version 1")
	}
	for _, dispute := range d.Disputes {
		if dispute.ID != id {
			continue
		}
		if dispute.Status == "resolved" {
			return Err(409, "resolved dispute does not need a review")
		}
		for _, step := range s.Steps {
			if slices.Contains(dispute.ReviewStepIDs, step.ID) && contextActiveStep(step.Status) {
				return Err(409, "dispute already has an active independent review")
			}
		}
		if len(dispute.ReviewStepIDs) >= MaxDisputeReviews {
			return Err(409, "dispute review limit reached; record uncertainty and request user guidance")
		}
		attempts := 0
		for _, stepID := range dispute.ReviewStepIDs {
			count, err := sqlstore.IntentExecutionCount(t, s.Graph.Project.ID, stepID)
			if err != nil {
				return err
			}
			attempts += count
		}
		if attempts >= MaxDisputeReviews {
			return Err(409, "dispute review execution limit reached; preserve uncertainty")
		}
		return nil
	}
	return Err(404, "Dispute not found")
}

func (t *Tx) validateReviewExecution(e Execution) error {
	s, err := t.State(e.ProjectID)
	if err != nil {
		return err
	}
	for _, step := range s.Steps {
		if step.ID != e.Intent || step.DisputeID == "" {
			continue
		}
		var dispute *Dispute
		for n := range s.Disputes {
			if s.Disputes[n].ID == step.DisputeID {
				dispute = &s.Disputes[n]
				break
			}
		}
		if dispute == nil || dispute.Status == "resolved" {
			return Err(409, "review dispute is unavailable")
		}
		if slices.Contains(dispute.ProducerRunIDs, e.Lease) {
			return Err(409, "independent review requires a new run")
		}
		count := 0
		for _, id := range dispute.ReviewStepIDs {
			attempts, err := sqlstore.IntentExecutionCount(t, e.ProjectID, id)
			if err != nil {
				return err
			}
			count += attempts
		}
		if count >= MaxDisputeReviews {
			return Err(409, "dispute review execution limit reached")
		}
	}
	return nil
}

func (t *Tx) reviewExecutionTransition(e Execution, status string) error {
	d, _, _, err := t.stateData(e.ProjectID)
	if err != nil {
		return err
	}
	disputeID := ""
	for _, step := range d.Steps {
		if step.ID == e.Intent {
			disputeID = step.DisputeID
		}
	}
	if disputeID == "" {
		return nil
	}
	for n := range d.Disputes {
		dispute := &d.Disputes[n]
		if dispute.ID != disputeID || dispute.Status == "resolved" {
			continue
		}
		next := "reviewing"
		if slices.Contains([]string{"failed", "rejected", "cancelled"}, status) {
			next = "uncertain"
		}
		if dispute.Status == next {
			return nil
		}
		decisionChanged := next != "reviewing"
		if !decisionChanged {
			before, stateErr := t.State(e.ProjectID)
			if stateErr != nil {
				return stateErr
			}
			if planner := before.Graph.Project.Reason; planner != nil {
				// The strict CAS still includes dispute status. If this mechanical
				// transition alone invalidates a live, uncommitted planner, preserve
				// its outstanding planning signal under a fresh input retry key.
				// Already stale or committed planners cannot renew that allowance.
				decisionChanged, err = sqlstore.PendingDecisionExecution(t, sqlstore.DecisionExecutionInput{
					ProjectID: e.ProjectID, Lease: planner.Worker, Generation: before.Graph.Project.Generation,
					DecisionRevision: before.DecisionRevision, StateVersion: DecisionStateVersion(before),
				})
				if err != nil {
					return err
				}
			}
		}
		dispute.Status, dispute.UpdatedAt = next, t.Now
		raw, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if err = sqlstore.UpdateStateMetadata(t, e.ProjectID, string(raw)); err != nil {
			return err
		}
		// Without an invalidated in-flight plan, authorized startup is execution
		// progress only. Failure back to uncertainty always needs a fresh decision.
		revision, err := t.advanceStateRevision(e.ProjectID, decisionChanged)
		if err != nil {
			return err
		}
		result, _ := json.Marshal(dispute)
		payload, _ := json.Marshal(map[string]string{"step_id": e.Intent, "status": status})
		event, _ := json.Marshal(StateEvent{Revision: revision, Op: "dispute", ID: dispute.ID, RunID: e.Lease, CreatedAt: t.Now, Payload: payload, Result: result})
		return sqlstore.InsertStateEvent(t, e.ProjectID, revision, string(event))
	}
	return nil
}
