package board

import (
	"encoding/json"
	"slices"

	"pwnmesh/internal/board/internal/sqlstore"
)

const MaxDisputeReviews = 2

func currentDisputeReviewSteps(dispute Dispute) ([]string, error) {
	if dispute.ReviewStepOffset < 0 || dispute.ReviewStepOffset > len(dispute.ReviewStepIDs) {
		return nil, Err(409, "invalid dispute review boundary")
	}
	return dispute.ReviewStepIDs[dispute.ReviewStepOffset:], nil
}

func (t *Tx) reopenDispute(s State, dispute *Dispute, renew bool) error {
	for _, step := range dispute.ReviewStepIDs {
		if err := sqlstore.RevokePendingStepRuns(t.transactionQueries, s.Graph.Project.ID, step, s.Graph.Project.Generation); err != nil {
			return err
		}
	}
	dispute.ReviewRevision = s.Revision + 1
	if renew {
		dispute.ReviewStepOffset = len(dispute.ReviewStepIDs)
	}
	dispute.Status, dispute.ReviewFactIDs = "open", []string{}
	return nil
}

// A new note or Fact ID alone is not new retained support. Compare effective
// evidence references with all earlier producer and independent-review support.
func disputeHasNewEvidence(s State, dispute Dispute, incoming []Candidate, sources []string, priorRelations []FactRelation) bool {
	if len(incoming)+len(sources) == 0 {
		return false
	}
	facts := make(map[string]FactRecord, len(s.FactRecords))
	known := map[EvidenceRef]bool{}
	retain := func(refs []EvidenceRef) {
		for _, ref := range refs {
			known[ref] = true
		}
	}
	var pending []string
	for _, fact := range s.FactRecords {
		facts[fact.ID] = fact
		if slices.Contains(dispute.ReviewStepIDs, fact.SourceStepID) {
			pending = append(pending, fact.ID)
		}
	}
	for _, candidate := range s.Candidates {
		if slices.Contains(dispute.CandidateIDs, candidate.ID) {
			retain(candidate.Evidence)
			pending = append(pending, candidate.Sources...)
		}
	}
	// Prior corrections are part of the reviewed evidence too. Exclude this
	// batch's new relations so a fresh correction can still renew once.
	dependencies := s.FactDependencies()
	seen := map[string]bool{}
	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		retain(facts[id].Evidence)
		for _, dependency := range dependencies[id] {
			pending = append(pending, dependency.ID)
		}
		for _, relation := range priorRelations {
			if relation.Target == id {
				pending = append(pending, relation.Source)
			}
		}
	}
	fresh := func(refs []EvidenceRef) bool {
		for _, ref := range refs {
			if !known[ref] {
				return true
			}
		}
		return false
	}
	freshSources := func(ids []string) bool {
		if s.ValidateFactSources(ids, true) != nil {
			return false
		}
		for _, id := range ids {
			if fresh(facts[id].Evidence) {
				return true
			}
		}
		return false
	}
	for _, candidate := range incoming {
		if len(candidate.Sources) > 0 && s.ValidateFactSources(candidate.Sources, true) != nil {
			continue
		}
		if fresh(candidate.Evidence) || freshSources(candidate.Sources) {
			return true
		}
	}
	return freshSources(sources)
}

func disputeReviewCorrectionSources(before State, dispute Dispute, relations []FactRelation) []string {
	dependencies := before.FactDependencies()
	relevant := map[string]bool{}
	pending := append([]string(nil), dispute.ReviewFactIDs...)
	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if relevant[id] {
			continue
		}
		relevant[id] = true
		for _, dependency := range dependencies[id] {
			pending = append(pending, dependency.ID)
		}
	}
	var sources []string
	for _, relation := range relations {
		if relevant[relation.Target] && !slices.Contains(before.FactRelations, relation) && !slices.Contains(sources, relation.Source) {
			sources = append(sources, relation.Source)
		}
	}
	return sources
}

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
		reviews, err := currentDisputeReviewSteps(dispute)
		if err != nil {
			return err
		}
		for _, step := range s.Steps {
			if slices.Contains(reviews, step.ID) && contextActiveStep(step.Status) {
				return Err(409, "dispute already has an active independent review")
			}
		}
		if len(reviews) >= MaxDisputeReviews {
			return Err(409, "dispute review limit reached; record uncertainty and request user guidance")
		}
		attempts := 0
		for _, stepID := range reviews {
			count, err := sqlstore.IntentExecutionCount(t.transactionQueries, s.Graph.Project.ID, stepID)
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
	reviews, err := checkReviewExecutionScope(s, e)
	if err != nil {
		return err
	}
	count := 0
	for _, id := range reviews {
		attempts, err := sqlstore.IntentExecutionCount(t.transactionQueries, e.ProjectID, id)
		if err != nil {
			return err
		}
		count += attempts
	}
	if count >= MaxDisputeReviews {
		return Err(409, "dispute review execution limit reached")
	}
	return nil
}

func checkReviewInputBoundary(dispute Dispute, e Execution) error {
	if len(e.Job) > 0 && dispute.ReviewRevision > 0 {
		input, err := executionInputMetadata(e.Job)
		if err != nil || !input.HasState || input.InputRevision < dispute.ReviewRevision {
			return Err(409, "review input predates the current dispute evidence boundary")
		}
	}
	return nil
}

// Scope checks apply to existing runs without recounting their already-used
// attempt. Registration and explicit retry authorization own the budget check.
func checkReviewExecutionScope(s State, e Execution) ([]string, error) {
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
			return nil, Err(409, "review dispute is unavailable")
		}
		reviews, err := currentDisputeReviewSteps(*dispute)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(reviews, step.ID) {
			return nil, Err(409, "review Step predates the current dispute evidence boundary")
		}
		if err := checkReviewInputBoundary(*dispute, e); err != nil {
			return nil, err
		}
		if slices.Contains(dispute.ProducerRunIDs, e.Lease) {
			return nil, Err(409, "independent review requires a new run")
		}
		return reviews, nil
	}
	return nil, nil
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
		reviews, err := currentDisputeReviewSteps(*dispute)
		if err != nil {
			return err
		}
		if !slices.Contains(reviews, e.Intent) || checkReviewInputBoundary(*dispute, e) != nil {
			return nil
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
				decisionChanged, err = sqlstore.PendingDecisionExecution(t.transactionQueries, sqlstore.DecisionExecutionInput{
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
		if err = sqlstore.UpdateStateMetadata(t.transactionQueries, e.ProjectID, string(raw)); err != nil {
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
		return sqlstore.InsertStateEvent(t.transactionQueries, e.ProjectID, revision, string(event))
	}
	return nil
}
