package board

import (
	"encoding/json"
	"pwnmesh/internal/board/model"
)

const DefaultContextViewBytes = model.DefaultContextViewBytes

func ContextView(state State, stepID string, maxBytes int) (json.RawMessage, error) {
	return model.ContextView(state, stepID, maxBytes)
}
func CurationContextView(state State, maxBytes int) (json.RawMessage, error) {
	return model.CurationContextView(state, maxBytes)
}
func contextActiveStep(status string) bool { return model.ActiveStepStatus(status) }
