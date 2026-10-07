package board

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
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
	Generation      int64            `json:"generation"`
	ThroughRevision int64            `json:"through_revision"`
	RunID           string           `json:"run_id,omitempty"`
	UpdatedAt       string           `json:"updated_at,omitempty"`
	Request         *CurationRequest `json:"request,omitempty"`
}

type Dispute struct {
	ID             string   `json:"id"`
	FindingID      string   `json:"finding_id"`
	CandidateIDs   []string `json:"candidate_ids"`
	Status         string   `json:"status"`
	Question       string   `json:"question"`
	Reason         string   `json:"reason"`
	ReviewStepIDs  []string `json:"review_step_ids"`
	ReviewFactIDs  []string `json:"review_fact_ids"`
	ProducerRunIDs []string `json:"producer_run_ids"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

type CurateGroup struct {
	CandidateIDs  []string `json:"candidate_ids"`
	Status        string   `json:"status"`
	Reason        string   `json:"reason"`
	Question      string   `json:"question,omitempty"`
	DisputeID     string   `json:"dispute_id,omitempty"`
	ReviewFactIDs []string `json:"review_fact_ids,omitempty"`
	Resolution    string   `json:"resolution,omitempty"`
}

type CuratePayload struct {
	ThroughRevision int64            `json:"through_revision"`
	Groups          []CurateGroup    `json:"groups"`
	Relations       []CurateRelation `json:"relations,omitempty"`
}

const MaxCurationRelations = 128

// CurateRelation preserves the existing relation protocol inside the one
// curation transaction, alongside its findings and acknowledged input boundary.
type CurateRelation struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Target string `json:"target"`
	Reason string `json:"reason"`
}

type CurateResult struct {
	Curation      CurationProgress `json:"curation"`
	Findings      []Finding        `json:"findings"`
	Disputes      []Dispute        `json:"disputes"`
	FactRelations []FactRelation   `json:"fact_relations,omitempty"`
}

func findingIdentity(claim, scope string) string {
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
	shared := map[string]bool{}
	for _, finding := range s.Findings {
		shared[findingIdentity(finding.Claim, finding.Scope)] = true
	}
	for _, candidate := range s.ActiveCandidates() {
		key := findingIdentity(candidate.Claim, candidate.Scope)
		groups[key] = append(groups[key], candidate)
	}
	selected := map[string]bool{}
	for key, candidates := range groups {
		fresh, verified, refuted := false, false, false
		producers := map[string]bool{}
		for _, candidate := range candidates {
			fresh = fresh || candidate.Revision > s.Curation.ThroughRevision
			verified = verified || candidate.Status == "verified"
			refuted = refuted || candidate.Status == "refuted"
			producers[candidate.RunID] = true
		}
		if fresh && (len(producers) > 1 || verified && refuted || shared[key]) {
			for _, candidate := range candidates {
				selected[candidate.ID] = true
			}
		}
	}
	return selected
}

func (t *Tx) addCandidate(s State, d *stateData, fence ExecutionFence, raw json.RawMessage) (string, any, error) {
	var input struct {
		Claim      string        `json:"claim"`
		Scope      string        `json:"scope"`
		Status     string        `json:"status"`
		Supersedes string        `json:"supersedes"`
		Sources    []string      `json:"sources"`
		Evidence   []EvidenceRef `json:"evidence"`
		Reason     string        `json:"reason"`
	}
	if err := decodeAction(raw, &input); err != nil {
		return "", nil, err
	}
	if input.Status == "" {
		input.Status = "candidate"
	}
	if !required(input.Claim, 16384) || !required(input.Scope, 4096) || !slices.Contains([]string{"candidate", "verified", "refuted"}, input.Status) || !required(input.Reason, 8192) {
		return "", nil, Err(422, "candidate requires claim,scope,reason and an optional candidate/verified/refuted status")
	}
	if input.Supersedes != "" {
		found := false
		for _, old := range s.ActiveCandidates() {
			if old.ID == input.Supersedes {
				found = old.RunID == fence.Run && findingIdentity(old.Claim, old.Scope) == findingIdentity(input.Claim, input.Scope)
				break
			}
		}
		if !found {
			return "", nil, Err(409, "a note revision must supersede an active note from the same run with identical claim and scope")
		}
	}
	if len(input.Sources) > 0 || input.Status != "candidate" {
		if err := s.ValidateFactSources(input.Sources, true); err != nil {
			return "", nil, err
		}
	}
	allowed := []EvidenceRef{}
	for _, f := range s.FactRecords {
		if slices.Contains(input.Sources, f.ID) {
			allowed = append(allowed, f.Evidence...)
		}
	}
	if err := validateEvidence(input.Evidence, fence.Run, allowed, false); err != nil {
		return "", nil, err
	}
	if input.Status != "candidate" && len(allowed)+len(input.Evidence) == 0 {
		return "", nil, Err(422, "candidate judgment requires retained evidence")
	}
	id, err := t.stateID(s.Graph.Project.ID, "candidate", "c")
	if err != nil {
		return "", nil, err
	}
	c := Candidate{ID: id, Claim: strings.Join(strings.Fields(input.Claim), " "), Scope: strings.TrimSpace(input.Scope), Status: input.Status, Supersedes: input.Supersedes, Sources: append([]string{}, input.Sources...), Evidence: append([]EvidenceRef{}, input.Evidence...), Reason: input.Reason, RunID: fence.Run, SourceStepID: fence.Intent, Generation: s.Graph.Project.Generation, Revision: s.Revision + 1, CreatedAt: t.Now}
	d.Candidates = append(d.Candidates, c)
	return id, c, nil
}

func (t *Tx) curate(s State, d *stateData, fence ExecutionFence, raw json.RawMessage) (string, any, error) {
	var input CuratePayload
	if err := decodeAction(raw, &input); err != nil {
		return "", nil, err
	}
	if input.Groups == nil || len(input.Groups) > 128 || len(input.Relations) > MaxCurationRelations || input.ThroughRevision < d.Curation.ThroughRevision || input.ThroughRevision > s.Revision {
		return "", nil, Err(422, "invalid curation input boundary, groups or relations")
	}
	// Bind the boundary to this execution's immutable input, not model fields.
	e, err := t.executionForLease(s.Graph.Project.ID, fence.Run)
	if err != nil {
		return "", nil, err
	}
	_, revision, err := CurationJobBoundary(e.Job)
	if err != nil {
		return "", nil, err
	}
	if revision != input.ThroughRevision {
		return "", nil, Err(409, "curation boundary does not match its registered input")
	}
	for _, relation := range input.Relations {
		raw, err := json.Marshal(relation)
		if err != nil {
			return "", nil, err
		}
		_, _, changed, err := t.addFactRelation(s, d, fence, raw)
		if err != nil {
			return "", nil, err
		}
		if changed {
			// Validate later relations, candidate conclusions and review facts
			// against the effective support after each preceding relation. The
			// durable state, receipt and cursor remain untouched until all pass.
			s, err = t.projectState(s.Graph, *d, s.Revision, s.DecisionRevision)
			if err != nil {
				return "", nil, err
			}
		}
	}
	if len(input.Relations) > 0 {
		for n := range d.Disputes {
			dispute := &d.Disputes[n]
			if dispute.Status != "resolved" || s.ValidateFactSources(dispute.ReviewFactIDs, true) == nil {
				continue
			}
			// Invalidating accepted review support removes the mechanical
			// prerequisite for resolution. Keep the review history and limits,
			// but allow the primary Agent to authorize a fresh independent run.
			dispute.Status, dispute.ReviewFactIDs = "open", []string{}
			dispute.Reason, dispute.UpdatedAt = "Independent review evidence was invalidated by this curation batch", t.Now
			for n := range d.Findings {
				if d.Findings[n].ID == dispute.FindingID {
					d.Findings[n].Status = "candidate"
					d.Findings[n].UpdatedAt = t.Now
					d.Findings[n].CuratedRevision = input.ThroughRevision
				}
			}
		}
	}
	byID := map[string]Candidate{}
	for _, c := range d.Candidates {
		byID[c.ID] = c
	}
	active := map[string]bool{}
	activeCandidates := s.ActiveCandidates()
	for _, candidate := range activeCandidates {
		active[candidate.ID] = true
	}
	covered := map[string]bool{}
	groupFindings := map[string]bool{}
	for _, group := range input.Groups {
		if len(group.CandidateIDs) == 0 || len(group.CandidateIDs) > 256 || !required(group.Reason, 8192) || !slices.Contains([]string{"candidate", "verified", "refuted"}, group.Status) || len(group.Question) > 8192 || !slices.Contains([]string{"", "resolved", "uncertain"}, group.Resolution) {
			return "", nil, Err(422, "invalid curation group")
		}
		selected := []Candidate{}
		id := ""
		for _, cid := range group.CandidateIDs {
			c, ok := byID[cid]
			if !ok || !active[cid] || c.Revision > input.ThroughRevision {
				return "", nil, Err(422, "candidate outside curation input")
			}
			key := findingIdentity(c.Claim, c.Scope)
			if covered[cid] || id != "" && id != key {
				return "", nil, Err(422, "curation group must contain distinct candidates with identical claim and scope")
			}
			id = key
			covered[cid] = true
			selected = append(selected, c)
		}
		if groupFindings[id] {
			return "", nil, Err(422, "one curation group per finding is required")
		}
		groupFindings[id] = true
		index := -1
		finding := Finding{ID: id, Claim: selected[0].Claim, Scope: selected[0].Scope, Status: group.Status, Reason: group.Reason, Sources: []string{}, Evidence: []EvidenceRef{}, CandidateIDs: []string{}, CreatedAt: t.Now, UpdatedAt: t.Now, CuratedRevision: input.ThroughRevision}
		for n, old := range d.Findings {
			if old.ID == id {
				index = n
				finding.CreatedAt = old.CreatedAt
				finding.CandidateIDs = append([]string{}, old.CandidateIDs...)
				finding.DisputeID = old.DisputeID
			}
		}
		for _, c := range selected {
			if !slices.Contains(finding.CandidateIDs, c.ID) {
				finding.CandidateIDs = append(finding.CandidateIDs, c.ID)
			}
		}
		for _, candidate := range activeCandidates {
			if findingIdentity(candidate.Claim, candidate.Scope) != id {
				continue
			}
			if !slices.Contains(finding.CandidateIDs, candidate.ID) {
				return "", nil, Err(409, "curation must include every active note in the selected group")
			}
			covered[candidate.ID] = true
		}
		verified, refuted := false, false
		effectiveVerified, effectiveRefuted := false, false
		for _, cid := range finding.CandidateIDs {
			c := byID[cid]
			// Keep superseded notes in provenance, while evaluating only their
			// current revisions. Existing disputes still require independent review.
			if !active[cid] {
				continue
			}
			verified = verified || c.Status == "verified"
			refuted = refuted || c.Status == "refuted"
			// A candidate's sources are one set of premises. Do not retain only
			// its valid subset, or let a stale judgment borrow newer candidates'
			// evidence to establish a conclusion it no longer supports.
			currentSupport := len(c.Sources) > 0 && s.ValidateFactSources(c.Sources, true) == nil
			if len(c.Sources) > 0 && !currentSupport {
				continue
			}
			effectiveVerified = effectiveVerified || currentSupport && c.Status == "verified"
			effectiveRefuted = effectiveRefuted || currentSupport && c.Status == "refuted"
			for _, source := range c.Sources {
				if !slices.Contains(finding.Sources, source) {
					finding.Sources = append(finding.Sources, source)
				}
			}
			for _, ref := range c.Evidence {
				if !slices.Contains(finding.Evidence, ref) {
					finding.Evidence = append(finding.Evidence, ref)
				}
			}
		}
		disputeIndex := -1
		for n, dispute := range d.Disputes {
			if dispute.FindingID == id {
				disputeIndex = n
				break
			}
		}
		if group.DisputeID != "" && (disputeIndex < 0 || d.Disputes[disputeIndex].ID != group.DisputeID) {
			return "", nil, Err(422, "dispute does not match candidate group")
		}
		if verified && refuted && disputeIndex < 0 {
			if !required(group.Question, 8192) {
				return "", nil, Err(422, "conflicting candidates require an independently verifiable question")
			}
			disputeID, err := t.stateID(s.Graph.Project.ID, "dispute", "d")
			if err != nil {
				return "", nil, err
			}
			producers := []string{}
			for _, cid := range finding.CandidateIDs {
				run := byID[cid].RunID
				if !slices.Contains(producers, run) {
					producers = append(producers, run)
				}
			}
			d.Disputes = append(d.Disputes, Dispute{ID: disputeID, FindingID: id, CandidateIDs: append([]string{}, finding.CandidateIDs...), ProducerRunIDs: producers, Status: "open", Question: group.Question, Reason: group.Reason, ReviewStepIDs: []string{}, ReviewFactIDs: []string{}, CreatedAt: t.Now, UpdatedAt: t.Now})
			disputeIndex = len(d.Disputes) - 1
		}
		if disputeIndex >= 0 {
			dispute := &d.Disputes[disputeIndex]
			newEvidence := false
			for _, cid := range finding.CandidateIDs {
				if !slices.Contains(dispute.CandidateIDs, cid) {
					newEvidence = true
					candidate := byID[cid]
					// New outside producers join the conflict's independence
					// boundary. An authorized review's own judgment is evidence
					// from that review, not a new original producer.
					if !slices.Contains(dispute.ReviewStepIDs, candidate.SourceStepID) && !slices.Contains(dispute.ProducerRunIDs, candidate.RunID) {
						dispute.ProducerRunIDs = append(dispute.ProducerRunIDs, candidate.RunID)
					}
				}
			}
			if newEvidence && dispute.Status == "resolved" {
				dispute.Status = "open"
				dispute.ReviewFactIDs = []string{}
			}
			dispute.CandidateIDs = append([]string{}, finding.CandidateIDs...)
			dispute.Reason, dispute.UpdatedAt = group.Reason, t.Now
			if group.Question != "" {
				dispute.Question = group.Question
			}
			finding.DisputeID = dispute.ID
			if group.Resolution != "" {
				if group.DisputeID != dispute.ID {
					return "", nil, Err(422, "review resolution requires dispute_id")
				}
				if group.Resolution == "resolved" {
					if group.Status == "candidate" {
						return "", nil, Err(422, "resolved dispute requires a supported verified or refuted judgment")
					}
					if err := t.validateReviewFacts(s, *dispute, group.ReviewFactIDs); err != nil {
						return "", nil, err
					}
					finding.Sources = append([]string{}, group.ReviewFactIDs...)
					finding.Evidence = []EvidenceRef{}
					for _, f := range s.FactRecords {
						if slices.Contains(group.ReviewFactIDs, f.ID) {
							finding.Evidence = append(finding.Evidence, f.Evidence...)
						}
					}
				} else if len(group.ReviewFactIDs) > 0 {
					if err := t.validateReviewFacts(s, *dispute, group.ReviewFactIDs); err != nil {
						return "", nil, err
					}
				}
				dispute.Status = group.Resolution
				dispute.ReviewFactIDs = append([]string{}, group.ReviewFactIDs...)
			}
			if dispute.Status != "resolved" {
				finding.Status = "candidate"
			} else if group.Resolution == "" {
				// Re-listing an old conflict cannot replace its independently
				// reviewed conclusion with the latest producer's judgment.
				if index >= 0 {
					old := d.Findings[index]
					finding.Status, finding.Sources, finding.Evidence = old.Status, old.Sources, old.Evidence
				}
			}
		} else {
			if group.Resolution != "" || len(group.ReviewFactIDs) > 0 {
				return "", nil, Err(422, "resolution requires an existing dispute")
			}
			if group.Status == "verified" && !verified || group.Status == "refuted" && !refuted {
				return "", nil, Err(422, "curation cannot raise a candidate beyond its producer evidence")
			}
			if group.Status == "verified" && !effectiveVerified || group.Status == "refuted" && !effectiveRefuted {
				return "", nil, Err(409, "curation requires current producer evidence for its judgment")
			}
		}
		if finding.Status != "candidate" {
			if err := s.ValidateFactSources(finding.Sources, true); err != nil {
				return "", nil, err
			}
		}
		finding.SupportValid = len(finding.Sources) > 0 && s.ValidateFactSources(finding.Sources, true) == nil
		if index < 0 {
			d.Findings = append(d.Findings, finding)
		} else {
			d.Findings[index] = finding
		}
	}
	// A scan must cover pending reconciliation work, but may leave ordinary
	// notes as notes. Advancing the cursor never hides them from future groups.
	for id := range s.PendingCurationCandidateIDs() {
		if !covered[id] {
			return "", nil, Err(409, "curation omitted a pending reconciliation candidate")
		}
	}
	d.Curation = CurationProgress{Generation: s.Graph.Project.Generation, ThroughRevision: input.ThroughRevision, RunID: fence.Run, UpdatedAt: t.Now}
	return "curation", CurateResult{Curation: d.Curation, Findings: d.Findings, Disputes: d.Disputes, FactRelations: d.FactRelations}, nil
}

func (t *Tx) executionForLease(project, lease string) (Execution, error) {
	return scanExecution(t.QueryRow("SELECT "+executionColumns+" FROM xloom_executions WHERE project_id=? AND lease=?", project, lease))
}

func (t *Tx) validateReviewFacts(s State, dispute Dispute, ids []string) error {
	if len(ids) == 0 || len(ids) > 32 {
		return Err(422, "resolution requires independent review fact IDs")
	}
	if err := s.ValidateFactSources(ids, true); err != nil {
		return err
	}
	for _, id := range ids {
		var fact *FactRecord
		for n := range s.FactRecords {
			if s.FactRecords[n].ID == id {
				fact = &s.FactRecords[n]
				break
			}
		}
		if fact == nil || fact.Legacy || len(fact.Evidence) == 0 || !slices.Contains(dispute.ReviewStepIDs, fact.SourceStepID) {
			return Err(409, "resolution evidence must come from this dispute's independent review")
		}
		e, err := t.executionForLease(s.Graph.Project.ID, fact.RunID)
		if err != nil || e.Status != "succeeded" || e.Intent != fact.SourceStepID {
			return Err(409, "review evidence requires a successfully completed independent run")
		}
		if slices.Contains(dispute.ProducerRunIDs, fact.RunID) {
			return Err(409, "producer run cannot independently review its own judgment")
		}
		var job struct {
			Graph         Graph          `json:"graph"`
			State         *State         `json:"state"`
			InputSnapshot *InputSnapshot `json:"input_snapshot"`
		}
		if json.Unmarshal(e.Job, &job) != nil || job.Graph.Project.Generation != s.Graph.Project.Generation {
			return Err(409, "review evidence belongs to a previous generation")
		}
		var boundary int64
		switch {
		case job.InputSnapshot != nil:
			if job.InputSnapshot.ProjectID != s.Graph.Project.ID || job.InputSnapshot.Generation != s.Graph.Project.Generation {
				return Err(409, "review snapshot belongs to a different project or generation")
			}
			boundary = job.InputSnapshot.Revision
		case job.State != nil:
			if job.State.Graph.Project.ID != s.Graph.Project.ID || job.State.Graph.Project.Generation != s.Graph.Project.Generation {
				return Err(409, "review state belongs to a different project or generation")
			}
			boundary = job.State.Revision
		default:
			return Err(409, "review evidence requires a registered input boundary")
		}
		for _, candidate := range s.Candidates {
			// A review can publish a judgment after reading its input. Every
			// other disputed candidate must have been available in that input;
			// an old successful review cannot close a newly reopened conflict.
			if slices.Contains(dispute.CandidateIDs, candidate.ID) && candidate.RunID != e.Lease && candidate.Revision > boundary {
				return Err(409, "review input predates a disputed candidate; a fresh independent review is required")
			}
		}
	}
	return nil
}
