package model

type ContextCandidate struct {
	CandidateView
	EvidenceOmitted bool `json:"evidence_omitted,omitempty"`
	EvidenceCount   int  `json:"evidence_count,omitempty"`
}

// Counts describe the relevant curation input, separately from omitted shared
// history. A supplied candidate body never implies that its support is complete.
type CurationInputCoverage struct {
	RelevantCandidates int    `json:"relevant_candidates"`
	ProvidedCandidates int    `json:"provided_candidates"`
	OmittedCandidates  int    `json:"omitted_candidates"`
	EvidenceOmitted    int    `json:"candidate_evidence_omitted"`
	ReadMore           string `json:"read_more"`
}

// Keep active groups needing reconciliation, including unsupported shared
// conclusions and unresolved disputes.
// Older unrelated candidates stay discoverable in ContextView's shared index;
// omission never deletes evidence or changes the immutable curation boundary.
func curationContextCandidates(state State) map[string]bool {
	selected, groups := state.PendingCurationCandidateIDs(), map[string]bool{}
	for _, finding := range state.Findings {
		if finding.Status != "candidate" && !finding.SupportValid {
			groups[FindingIdentity(finding.Claim, finding.Scope)] = true
		}
	}
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
			groups[FindingIdentity(candidate.Claim, candidate.Scope)] = true
		}
	}
	selected = map[string]bool{}
	for _, candidate := range active {
		if groups[FindingIdentity(candidate.Claim, candidate.Scope)] {
			selected[candidate.ID] = true
		}
	}
	return selected
}
