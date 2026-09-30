package board

type contextCandidate struct {
	CandidateView
	EvidenceOmitted bool `json:"evidence_omitted,omitempty"`
	EvidenceCount   int  `json:"evidence_count,omitempty"`
}

// Counts describe the relevant curation input, separately from omitted shared
// history. A supplied candidate body never implies that its support is complete.
type curationInputCoverage struct {
	RelevantCandidates int    `json:"relevant_candidates"`
	ProvidedCandidates int    `json:"provided_candidates"`
	OmittedCandidates  int    `json:"omitted_candidates"`
	EvidenceOmitted    int    `json:"candidate_evidence_omitted"`
	ReadMore           string `json:"read_more"`
}

// CandidateView adds read-only grouping and source validity to a projection.
// The stored Candidate and its immutable snapshot/version remain unchanged.
type CandidateView struct {
	Candidate
	GroupKey     string `json:"group_key,omitempty"`
	SupportValid *bool  `json:"support_valid,omitempty"`
}

func (s State) CandidateView(candidate Candidate) CandidateView {
	support := s.ValidateFactSources(candidate.Sources, true) == nil
	return CandidateView{Candidate: candidate, GroupKey: findingIdentity(candidate.Claim, candidate.Scope), SupportValid: &support}
}

// Keep the active identity groups needing reconciliation and unresolved disputes.
// Older unrelated candidates stay discoverable in ContextView's shared index;
// omission never deletes evidence or changes the immutable curation boundary.
func curationContextCandidates(state State) map[string]bool {
	selected, groups := state.PendingCurationCandidateIDs(), map[string]bool{}
	for _, dispute := range state.Disputes {
		if dispute.Status != "resolved" {
			for _, id := range dispute.CandidateIDs {
				selected[id] = true
			}
		}
	}
	active := state.ActiveCandidates()
	for _, candidate := range active {
		if selected[candidate.ID] {
			groups[findingIdentity(candidate.Claim, candidate.Scope)] = true
		}
	}
	selected = map[string]bool{}
	for _, candidate := range active {
		if groups[findingIdentity(candidate.Claim, candidate.Scope)] {
			selected[candidate.ID] = true
		}
	}
	return selected
}
