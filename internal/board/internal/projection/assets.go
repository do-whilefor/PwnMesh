package projection

import (
	"pwnmesh/internal/board/model"
	"slices"
)

// WithAssets derives finding associations without appending into stored anchors.
func WithAssets(s model.State, assets []model.Asset, anchors []model.AssetAnchor) model.State {
	s.Assets, s.AssetAnchors = assets, slices.Clone(anchors)
	type nodeKey struct{ kind, id string }
	byNode := map[nodeKey][]string{}
	for _, anchor := range anchors {
		key := nodeKey{anchor.NodeKind, anchor.NodeID}
		byNode[key] = append(byNode[key], anchor.AssetID)
	}
	// A finding's association follows its present support references. It is a
	// read projection, not a new claim or evidence-strength decision.
	active := map[string]model.Candidate{}
	for _, candidate := range s.ActiveCandidates() {
		active[candidate.ID] = candidate
	}
	for _, finding := range s.Findings {
		ids := []string{}
		for _, source := range finding.Sources {
			ids = append(ids, byNode[nodeKey{"fact", source}]...)
		}
		for _, candidateID := range finding.CandidateIDs {
			candidate, ok := active[candidateID]
			if !ok || candidate.Revision > finding.CuratedRevision || !candidateAssetsSupportFinding(s, candidate, finding) {
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
