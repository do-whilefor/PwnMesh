package board

import (
	"pwnmesh/internal/board/internal/sqlstore"
	"pwnmesh/internal/board/model"
)

type StateChange = model.StateChange

// StateChanges is the bounded discovery index for a new clean Decide. Event
// bodies and past execution inputs are unnecessary: current FGS records supply
// the knowledge, including corrections and the status of abandoned plans.
func (t *Tx) StateChanges(project string, after, through int64) ([]StateChange, error) {
	if err := t.RequireProject(project); err != nil {
		return nil, err
	}
	if after < 0 || through < after {
		return nil, Err(422, "invalid change revision interval")
	}
	return sqlstore.StateChanges(t, project, after, through, 1000)
}
