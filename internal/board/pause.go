package board

import (
	"encoding/json"

	"pwnmesh/internal/board/internal/sqlstore"
)

// Pause records only executions interrupted by this human operation. A later
// Continue must not grant blanket retries to unrelated failures or abandoned
// directions. The marker survives dispatcher shutdown and cascades on restart.
func (t *Tx) markPausedExecutions(g Graph) error {
	return sqlstore.MarkPausedExecutions(t.transactionQueries, g.Project.ID)
}

func (t *Tx) continuePausedExecutions(g Graph) error {
	ids, err := sqlstore.PausedExecutionIDs(t.transactionQueries, g.Project.ID)
	if err != nil {
		return err
	}
	d, _, _, err := t.stateData(g.Project.ID)
	if err != nil {
		return err
	}
	abandoned := map[string]bool{}
	for _, step := range d.Steps {
		abandoned[step.ID] = step.Status == "abandoned"
	}
	for _, id := range ids {
		e, err := t.Execution(g.Project.ID, id)
		if err != nil {
			return err
		}
		if !e.Pending() && e.Status != "cancelled" {
			continue
		}
		if !controlKind(e.Kind) {
			available := false
			for _, intent := range g.Intents {
				if intent.ID == e.Intent && intent.To == nil && intent.ConcludedAt == nil && !abandoned[intent.ID] {
					available = true
					break
				}
			}
			if !available {
				continue
			}
		}
		// Continue may arrive before the old worker acknowledges cancellation.
		// Terminalize the revoked attempt here; its late callback cannot revoke
		// this one-use grant. ExecutionStatus retains immutable pending results.
		if e.Pending() {
			failure := json.RawMessage(`{"status":"failed","failure_kind":"project_paused","error":"project paused by user"}`)
			if err = t.ExecutionStatus(e, "cancelled", failure); err != nil {
				return err
			}
		}
		if err = t.ExecutionStatus(e, "retry_requested", nil); err != nil {
			return err
		}
	}
	return sqlstore.DeletePausedExecutions(t.transactionQueries, g.Project.ID)
}
