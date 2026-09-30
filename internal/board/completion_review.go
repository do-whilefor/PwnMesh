package board

import (
	"encoding/json"
	"errors"
)

const MaxCompletionReviewBytes = 48 << 10

// CompletionReview presents the original requirements and cited observations.
// It is a review input, not a machine judgment that the proof satisfies them.
type CompletionReview struct {
	StateVersion        string             `json:"state_version"`
	Acceptance          string             `json:"acceptance"`
	UserInputs          []Fact             `json:"user_inputs"`
	Hints               []Hint             `json:"hints"`
	From                []string           `json:"from"`
	Description         string             `json:"description"`
	FactRecords         []FactRecord       `json:"fact_records"`
	Candidates          []contextCandidate `json:"candidates,omitempty"`
	Disputes            []Dispute          `json:"disputes,omitempty"`
	CurationRequest     *CurationRequest   `json:"curation_request,omitempty"`
	OmittedFactIDs      []string           `json:"omitted_fact_ids,omitempty"`
	OmittedCandidateIDs []string           `json:"omitted_candidate_ids,omitempty"`
	OmittedDisputeIDs   []string           `json:"omitted_dispute_ids,omitempty"`
	ReadMore            string             `json:"read_more,omitempty"`
}

const completionReviewReadMore = "Read omitted facts, candidates and disputes using read_graph with these IDs before accepting completion; read missing candidate or curation-request source facts and evidence by ID. Candidate notes are revisable interpretations, not proof. Assess unresolved uncertainty against the original requirements even when it does not mechanically block completion; omission is not acceptance."

// CompletionAssessment supplies observations before the model proposes a
// completion. Passing the mechanical gates does not establish that the user's
// requirements are satisfied. Preview and commit still validate the live state.
func (t *Tx) CompletionAssessment(state State) (*CompletionReview, error) {
	if state.Graph.Project.OrchestrationVersion != 1 || state.Graph.Project.Status != "active" {
		return nil, nil
	}
	for _, step := range state.Steps {
		if step.Status == "open" || step.Status == "running" || step.Status == "needs_review" || step.Status == "blocked" {
			return nil, nil
		}
	}
	ids := []string{}
	for _, fact := range state.FactRecords {
		if fact.ID != "origin" && fact.ID != "goal" && !fact.Legacy && !fact.SupportInvalid && fact.Status == "valid" && len(fact.Evidence) > 0 {
			ids = append(ids, fact.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if err := t.ValidateStateCompletion(state.Graph.Project.ID, ids); err != nil {
		var api *APIError
		if errors.As(err, &api) {
			return nil, nil // Ordinary planning handles the unresolved condition.
		}
		return nil, err
	}
	payload, _ := json.Marshal(struct {
		From        []string `json:"from"`
		Description string   `json:"description"`
	}{From: ids})
	review, err := buildCompletionReview(state, DecisionStateVersion(state), payload, MaxCompletionReviewBytes)
	if err != nil {
		var api *APIError
		if errors.As(err, &api) {
			return nil, nil // Oversized mandatory input retains the normal preview path.
		}
		return nil, err
	}
	return review, nil
}

// UseCompletionAssessment keeps one copy of user requirements and observation
// bodies. Retain every goal and Step (including failed/withdrawn work), so the
// model must still assess coverage. Oversized coverage metadata uses the normal
// planning view instead of silently dropping a requirement or unresolved task.
func UseCompletionAssessment(state State, decision *DecisionContext, assessment *CompletionReview) error {
	if assessment == nil {
		return nil
	}
	view := struct {
		Version      int              `json:"version"`
		Revision     int64            `json:"revision"`
		Project      Project          `json:"project"`
		Goals        []Goal           `json:"goals"`
		Steps        []Step           `json:"steps"`
		Findings     []Finding        `json:"findings"`
		Relations    []FactRelation   `json:"fact_relations"`
		Curation     CurationProgress `json:"curation"`
		OtherFactIDs []string         `json:"other_fact_ids"`
		ReadMore     string           `json:"read_more"`
	}{Version: 1, Revision: state.Revision, Project: state.Graph.Project,
		Goals: state.Goals, Steps: state.Steps, Findings: state.Findings, Relations: state.FactRelations,
		Curation: state.Curation, OtherFactIDs: []string{},
		ReadMore: "Original user_inputs, hints, available evidence, active candidate notes and unresolved disputes are in completion_assessment. Coverage is not accepted. Read omitted facts or candidate history by ID when needed; failed or withdrawn work does not discharge the original requirements."}
	view.Project.Reason, view.Project.Curator = nil, nil
	view.Curation.Request = nil // The pending request is in the assessment packet.
	selected := map[string]bool{}
	for _, id := range assessment.From {
		selected[id] = true
	}
	_, inputIDs := state.UserInputFacts()
	for _, fact := range state.FactRecords {
		if !inputIDs[fact.ID] && !selected[fact.ID] {
			view.OtherFactIDs = append(view.OtherFactIDs, fact.ID)
		}
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return err
	}
	budget := DefaultContextViewBytes - len(raw) - 512 // reserve the DecisionContext envelope
	if budget <= 0 {
		return nil
	}
	payload, _ := json.Marshal(struct {
		From        []string `json:"from"`
		Description string   `json:"description"`
	}{From: assessment.From})
	bounded, err := buildCompletionReview(state, decision.StateVersion, payload, min(budget, MaxCompletionReviewBytes))
	if err != nil {
		var api *APIError
		if errors.As(err, &api) {
			return nil
		}
		return err
	}
	decision.View, decision.CompletionAssessment = raw, bounded
	decision.Mode, decision.Fallback = "completion", ""
	return nil
}

func buildCompletionReview(state State, version string, payload json.RawMessage, maxBytes int) (*CompletionReview, error) {
	var proposed struct {
		From        []string `json:"from"`
		Description string   `json:"description"`
	}
	if err := decodeAction(payload, &proposed); err != nil {
		return nil, err
	}
	review := &CompletionReview{
		StateVersion: version, Acceptance: "not_checked",
		UserInputs: []Fact{}, Hints: append([]Hint{}, state.Graph.Hints...),
		From: append([]string{}, proposed.From...), Description: proposed.Description,
		FactRecords: []FactRecord{}, OmittedFactIDs: append([]string{}, proposed.From...),
		ReadMore: completionReviewReadMore,
	}
	review.UserInputs, _ = state.UserInputFacts()
	review.CurationRequest = state.PendingCurationRequest()
	candidates := state.ActiveCandidates()
	for _, candidate := range candidates {
		review.OmittedCandidateIDs = append(review.OmittedCandidateIDs, candidate.ID)
	}
	for _, dispute := range state.Disputes {
		if dispute.Status != "resolved" {
			review.OmittedDisputeIDs = append(review.OmittedDisputeIDs, dispute.ID)
		}
	}
	// Reserve all omitted IDs up front. No original requirement, proof or
	// citation can silently disappear when individual observations are large.
	if raw, err := json.Marshal(review); err != nil {
		return nil, err
	} else if len(raw) > maxBytes {
		return nil, Err(422, "completion review requirements and proposal exceed the review byte budget")
	}
	// Reserve every uncertainty ID before fitting bodies. Large observations
	// must never make an unresolved question disappear from completion review.
	for _, dispute := range state.Disputes {
		if dispute.Status == "resolved" {
			continue
		}
		review.Disputes = append(review.Disputes, dispute)
		raw, err := json.Marshal(review)
		if err != nil {
			return nil, err
		}
		if len(raw) > maxBytes {
			review.Disputes = review.Disputes[:len(review.Disputes)-1]
			continue
		}
		review.OmittedDisputeIDs = removeReviewID(review.OmittedDisputeIDs, dispute.ID)
	}
	for _, candidate := range candidates {
		projected := contextCandidate{CandidateView: state.CandidateView(candidate), EvidenceCount: len(candidate.Evidence), EvidenceOmitted: len(candidate.Evidence) > 0}
		projected.Evidence = nil
		review.Candidates = append(review.Candidates, projected)
		raw, err := json.Marshal(review)
		if err != nil {
			return nil, err
		}
		if len(raw) > maxBytes {
			review.Candidates = review.Candidates[:len(review.Candidates)-1]
			continue
		}
		review.OmittedCandidateIDs = removeReviewID(review.OmittedCandidateIDs, candidate.ID)
	}
	facts := make(map[string]FactRecord, len(state.FactRecords))
	for _, fact := range state.FactRecords {
		facts[fact.ID] = fact
	}
	for _, id := range proposed.From {
		fact, ok := facts[id]
		if !ok {
			return nil, Err(404, "completion review Fact "+id+" not found")
		}
		review.FactRecords = append(review.FactRecords, fact)
		// Conservatively retain the reserved omissions while testing capacity;
		// a successful inclusion may only reduce the final encoded size.
		raw, err := json.Marshal(review)
		if err != nil {
			return nil, err
		}
		if len(raw) > maxBytes {
			review.FactRecords = review.FactRecords[:len(review.FactRecords)-1]
			continue
		}
		for n, omitted := range review.OmittedFactIDs {
			if omitted == id {
				review.OmittedFactIDs = append(review.OmittedFactIDs[:n], review.OmittedFactIDs[n+1:]...)
				break
			}
		}
	}
	if len(review.OmittedFactIDs)+len(candidates)+len(review.Disputes)+len(review.OmittedDisputeIDs) == 0 && review.CurationRequest == nil {
		review.ReadMore = ""
	}
	return review, nil
}

func removeReviewID(ids []string, id string) []string {
	for n, omitted := range ids {
		if omitted == id {
			return append(ids[:n], ids[n+1:]...)
		}
	}
	return ids
}
