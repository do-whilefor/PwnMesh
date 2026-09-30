package board

// Completion depends on the proposed evidence and its recorded prerequisites,
// not every interpretation explored along the way. This is a mechanical scope
// check; the model must still compare all original requirements with the proof
// and the uncertainty retained in CompletionReview.
func (s State) completionSources(from []string) map[string]bool {
	facts, steps, prerequisites := map[string]bool{}, map[string]bool{}, map[string]bool{}
	add := func(ids []string) {
		for _, id := range ids {
			if id != "" && id != "origin" && id != "goal" {
				facts[id] = true
			}
		}
	}
	add(from)
	for _, goal := range s.Goals {
		if goal.ID != "goal" && goal.Status == "achieved" {
			add(goal.Sources)
		}
	}
	for {
		before := len(facts) + len(steps) + len(prerequisites)
		for _, fact := range s.FactRecords {
			if facts[fact.ID] && fact.SourceStepID != "" {
				steps[fact.SourceStepID] = true
			}
		}
		for _, step := range s.Steps {
			if facts[Value(step.Result)] {
				steps[step.ID] = true
			}
			if steps[step.ID] {
				add(step.From)
				if prerequisites[step.ID] {
					add([]string{Value(step.Result)})
				}
				for _, id := range step.DependsOn {
					steps[id] = true
					prerequisites[id] = true
				}
			}
		}
		for _, relation := range s.FactRelations {
			if facts[relation.Source] || facts[relation.Target] {
				add([]string{relation.Source, relation.Target})
			}
		}
		if len(facts)+len(steps)+len(prerequisites) == before {
			return facts
		}
	}
}

func (s State) validateCompletionDisputes(from []string) error {
	sources := s.completionSources(from)
	relevant := func(ids []string) bool {
		for _, id := range ids {
			if sources[id] {
				return true
			}
		}
		return false
	}
	if request := s.PendingCurationRequest(); request != nil && relevant(request.Sources) {
		return Err(409, "Completion evidence has a pending curation request")
	}
	candidates := make(map[string]Candidate, len(s.Candidates))
	for _, candidate := range s.Candidates {
		candidates[candidate.ID] = candidate
	}
	findings := make(map[string]Finding, len(s.Findings))
	for _, finding := range s.Findings {
		findings[finding.ID] = finding
	}
	for _, dispute := range s.Disputes {
		finding := findings[dispute.FindingID]
		affectsProof := relevant(finding.Sources) || relevant(dispute.ReviewFactIDs)
		for _, id := range dispute.CandidateIDs {
			affectsProof = affectsProof || relevant(candidates[id].Sources)
		}
		if !affectsProof {
			continue
		}
		if dispute.Status != "resolved" {
			return Err(409, "Completion evidence depends on unresolved dispute "+dispute.ID)
		}
		if !finding.SupportValid {
			return Err(409, "Completion dispute resolution has invalid evidence support")
		}
	}
	// A conflicting producer judgment cannot evade review merely because the
	// curator has not yet turned it into a Dispute record.
	type judgment struct{ verified, refuted, relevant bool }
	groups := map[string]judgment{}
	pending := s.PendingCurationCandidateIDs()
	for _, candidate := range s.ActiveCandidates() {
		if !pending[candidate.ID] {
			continue
		}
		key := findingIdentity(candidate.Claim, candidate.Scope)
		group := groups[key]
		group.verified = group.verified || candidate.Status == "verified"
		group.refuted = group.refuted || candidate.Status == "refuted"
		group.relevant = group.relevant || relevant(candidate.Sources)
		groups[key] = group
	}
	for _, group := range groups {
		if group.verified && group.refuted && group.relevant {
			return Err(409, "Completion evidence has uncurated conflicting judgments")
		}
	}
	return nil
}
