package board

import "pwnmesh/internal/board/model"

type RootAssessment = model.RootAssessment
type RequirementGap = model.RequirementGap

func ValidateRootAssessment(a *RootAssessment, inputs []Fact, hints []Hint) error {
	return model.ValidateRootAssessment(a, inputs, hints)
}
func ValidateClosureActions(a *RootAssessment, actions []DecisionAction) error {
	return model.ValidateClosureActions(a, actions)
}
