package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Candidate is an immutable producer judgment, never the shared conclusion.
type Candidate struct {
	ID           string        `json:"id"`
	Claim        string        `json:"claim"`
	Scope        string        `json:"scope"`
	Status       string        `json:"status"`
	Supersedes   string        `json:"supersedes,omitempty"`
	Sources      []string      `json:"sources"`
	Evidence     []EvidenceRef `json:"evidence"`
	Reason       string        `json:"reason"`
	RunID        string        `json:"run_id"`
	SourceStepID string        `json:"source_step_id"`
	Generation   int64         `json:"generation"`
	Revision     int64         `json:"revision"`
	CreatedAt    string        `json:"created_at"`
}

type CurationProgress struct {
	Generation      int64                  `json:"generation"`
	ThroughRevision int64                  `json:"through_revision"`
	RunID           string                 `json:"run_id,omitempty"`
	UpdatedAt       string                 `json:"updated_at,omitempty"`
	Request         *CurationRequest       `json:"request,omitempty"`
	RequestReview   *CurationRequestReview `json:"request_review,omitempty"`
}

// CurationRequestReview records why an explicit reconciliation request ended
// or still needs evidence. Revision is assigned by the server.
type CurationRequestReview struct {
	Revision       int64    `json:"revision,omitempty"`
	Disposition    string   `json:"disposition"`
	CheckedSources []string `json:"checked_sources"`
	Reason         string   `json:"reason"`
}

type Dispute struct {
	ID            string   `json:"id"`
	FindingID     string   `json:"finding_id"`
	CandidateIDs  []string `json:"candidate_ids"`
	Status        string   `json:"status"`
	Question      string   `json:"question"`
	Reason        string   `json:"reason"`
	ReviewStepIDs []string `json:"review_step_ids"`
	ReviewFactIDs []string `json:"review_fact_ids"`
	// A renewed evidence boundary retains old review identities while binding
	// new attempts to the reopening revision and its own bounded Step suffix.
	ReviewRevision   int64    `json:"review_revision,omitempty"`
	ReviewStepOffset int      `json:"review_step_offset,omitempty"`
	ProducerRunIDs   []string `json:"producer_run_ids"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
}

// CurationRequest lets the planner ask for evidence reconciliation without
// manufacturing matching producer claims or classifying ordinary exploration.
type CurationRequest struct {
	Sources  []string `json:"sources"`
	Reason   string   `json:"reason"`
	Revision int64    `json:"revision"`
}

func (s State) PendingCurationRequest() *CurationRequest {
	request := s.Curation.Request
	if request == nil || s.Curation.Generation != s.Graph.Project.Generation || request.Revision > s.Revision {
		return nil
	}
	if request.Revision <= s.Curation.ThroughRevision && !s.CurationRequestNeedsEvidence() {
		return nil
	}
	return request
}

func (s State) CurationRequestNeedsEvidence() bool {
	r, review := s.Curation.Request, s.Curation.RequestReview
	return r != nil && review != nil && review.Revision == r.Revision && review.Disposition == "needs_review"
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
	return CandidateView{Candidate: candidate, GroupKey: FindingIdentity(candidate.Claim, candidate.Scope), SupportValid: &support}
}

// FindingIdentity is the canonical grouping key for producer judgments.
func FindingIdentity(claim, scope string) string {
	raw, _ := json.Marshal([]string{strings.Join(strings.Fields(claim), " "), strings.TrimSpace(scope)})
	digest := sha256.Sum256(raw)
	return "finding_" + hex.EncodeToString(digest[:12])
}

// ActiveCandidates projects the current producer notes without rewriting their
// history. A revision can retire only its own producer's earlier interpretation.
func (s State) ActiveCandidates() []Candidate {
	retired := map[string]bool{}
	for _, c := range s.Candidates {
		if c.Generation == s.Graph.Project.Generation && c.Revision <= s.Revision && c.Supersedes != "" {
			retired[c.Supersedes] = true
		}
	}
	active := []Candidate{}
	for _, c := range s.Candidates {
		if c.Generation == s.Graph.Project.Generation && c.Revision <= s.Revision && !retired[c.ID] {
			active = append(active, c)
		}
	}
	return active
}

// PendingCurationCandidateIDs selects whole active identity groups that need
// reconciliation. Ordinary observations and one producer's tentative notes do
// not need to become shared findings before exploration can continue.
func (s State) PendingCurationCandidateIDs() map[string]bool {
	groups := map[string][]Candidate{}
	shared := map[string]Finding{}
	for _, finding := range s.Findings {
		shared[FindingIdentity(finding.Claim, finding.Scope)] = finding
	}
	for _, candidate := range s.ActiveCandidates() {
		key := FindingIdentity(candidate.Claim, candidate.Scope)
		groups[key] = append(groups[key], candidate)
	}
	selected := map[string]bool{}
	for key, candidates := range groups {
		finding, hasFinding := shared[key]
		// A bounded batch can finish one identity group before the global
		// scan finishes. Its own boundary prevents repeatedly scheduling it
		// while other groups are still waiting at the old global cursor.
		acknowledged := map[string]bool{}
		for _, id := range finding.CandidateIDs {
			acknowledged[id] = true
		}
		fresh, verified, refuted := false, false, false
		producers := map[string]bool{}
		for _, candidate := range candidates {
			fresh = fresh || candidate.Revision > s.Curation.ThroughRevision && (candidate.Revision > finding.CuratedRevision || !acknowledged[candidate.ID])
			verified = verified || candidate.Status == "verified"
			refuted = refuted || candidate.Status == "refuted"
			producers[candidate.RunID] = true
		}
		if fresh && (len(producers) > 1 || verified && refuted || hasFinding) {
			for _, candidate := range candidates {
				selected[candidate.ID] = true
			}
		}
	}
	return selected
}

const MaxCurationRelations = 128
