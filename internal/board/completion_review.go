package board

import (
	"encoding/json"
	"pwnmesh/internal/board/model"
)

const MaxCompletionReviewBytes = model.MaxCompletionReviewBytes

type CompletionReview = model.CompletionReview

func HasCompletionEvidence(state State) bool { return model.HasCompletionEvidence(state) }
func UseCompletionAssessment(state State, decision *DecisionContext, assessment *CompletionReview) error {
	return model.UseCompletionAssessment(state, decision, assessment)
}
func buildCompletionReview(state State, version string, payload json.RawMessage, maxBytes int) (*CompletionReview, error) {
	return model.BuildCompletionReview(state, version, payload, maxBytes)
}
