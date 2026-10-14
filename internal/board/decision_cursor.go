package board

import "pwnmesh/internal/board/model"

type DecisionCursor = model.DecisionCursor

func BuildDecisionContextFromCursor(current State, cursor *DecisionCursor, events []StateEvent, maxBytes int) (*DecisionContext, error) {
	return model.BuildDecisionContextFromCursor(current, cursor, events, maxBytes)
}
