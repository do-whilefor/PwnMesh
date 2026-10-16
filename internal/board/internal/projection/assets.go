package projection

import (
	"pwnmesh/internal/board/model"
	"slices"
)

// WithAssets derives discovery and finding associations without changing stored
// anchors. Discovery through a Step never contributes to a finding's support.
func WithAssets(s model.State, assets []model.Asset, anchors []model.AssetAnchor) model.State {
	s.Assets, s.AssetAnchors = assets, slices.Clone(anchors)
	if len(anchors) == 0 {
		return s
	}
	type nodeKey struct{ kind, id string }
	byNode := map[nodeKey][]string{}
	for _, anchor := range anchors {
		key := nodeKey{anchor.NodeKind, anchor.NodeID}
		byNode[key] = append(byNode[key], anchor.AssetID)
	}
	facts := make(map[string]model.FactRecord, len(s.FactRecords))
	for _, fact := range s.FactRecords {
		facts[fact.ID] = fact
	}
	for _, step := range s.Steps {
		if step.Result == nil {
			continue
		}
		fact, ok := facts[*step.Result]
		if !ok || fact.SourceStepID != step.ID || len(byNode[nodeKey{"fact", fact.ID}]) != 0 {
			continue // Explicit observed associations take precedence, even if different.
		}
		for _, id := range byNode[nodeKey{"step", step.ID}] {
			s.AssetAnchors = append(s.AssetAnchors, model.AssetAnchor{AssetID: id, NodeKind: "fact", NodeID: fact.ID, DiscoveryStepID: step.ID})
		}
	}
	// A finding's association follows its present support references. It is a
	// read projection, not a new claim or evidence-strength decision.
	candidates := map[string]model.Candidate{}
	retiredAt := map[string]int64{}
	for _, candidate := range s.Candidates {
		if candidate.Generation != s.Graph.Project.Generation || candidate.Revision > s.Revision {
			continue
		}
		candidates[candidate.ID] = candidate
		if candidate.Supersedes != "" {
			if revision, exists := retiredAt[candidate.Supersedes]; !exists || candidate.Revision < revision {
				retiredAt[candidate.Supersedes] = candidate.Revision
			}
		}
	}
	for _, finding := range s.Findings {
		ids := []string{}
		for _, source := range finding.Sources {
			ids = append(ids, byNode[nodeKey{"fact", source}]...)
		}
		for _, candidateID := range finding.CandidateIDs {
			candidate, ok := candidates[candidateID]
			retirement, retired := retiredAt[candidateID]
			// Producer revisions cannot change a Finding's associations before
			// curation, but current invalid evidence must still be excluded.
			if !ok || candidate.Revision > finding.CuratedRevision || retired && retirement <= finding.CuratedRevision || !candidateAssetsSupportFinding(s, candidate, finding) {
				continue
			}
			ids = append(ids, byNode[nodeKey{"candidate", candidateID}]...)
		}
		slices.Sort(ids)
		for _, id := range slices.Compact(ids) {
			s.AssetAnchors = append(s.AssetAnchors, model.AssetAnchor{AssetID: id, NodeKind: "finding", NodeID: finding.ID})
		}
	}
	return s
}

func candidateAssetsSupportFinding(s model.State, candidate model.Candidate, finding model.Finding) bool {
	if len(candidate.Sources) > 0 && s.ValidateFactSources(candidate.Sources, true) != nil {
		return false
	}
	for _, source := range candidate.Sources {
		if !slices.Contains(finding.Sources, source) {
			return false
		}
	}
	for _, evidence := range candidate.Evidence {
		if !slices.Contains(finding.Evidence, evidence) {
			return false
		}
	}
	return len(candidate.Sources)+len(candidate.Evidence) > 0 || finding.Status == "candidate"
}
