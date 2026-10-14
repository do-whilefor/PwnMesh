package board

import "pwnmesh/internal/board/model"

// Existing transactional regressions decode the canonical read projections.
// These private test names do not expand Board's production API.
type contextFact = model.ContextFact
type contextCandidate = model.ContextCandidate
type contextOverview = model.ContextOverview
type contextOverviewSection = model.ContextOverviewSection
type contextOverviewNode = model.ContextOverviewNode
type curationInputCoverage = model.CurationInputCoverage
type decisionView = model.DecisionView
type decisionChanges = model.DecisionChanges

func buildContextOverview(state State, maxBytes int, candidates map[string]bool, stepID string, relevant map[string]bool) (*contextOverview, error) {
	return model.BuildContextOverview(state, maxBytes, candidates, stepID, relevant)
}
