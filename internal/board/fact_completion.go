package board

import (
	"encoding/json"

	"pwnmesh/internal/board/internal/sqlstore"
)

// ConcludeEvidenceStep shares the process-fact command and validations. Its
// caller commits this mutation and the execution receipt in one transaction.
// An existing observation remains immutable when reused as the result anchor.
func (t *Tx) ConcludeEvidenceStep(project string, fence ExecutionFence, factID string, factPayload json.RawMessage) (out Conclusion, err error) {
	if (factID == "") == (len(factPayload) == 0) {
		return Conclusion{}, Err(422, "conclusion requires exactly one fact_id or fact")
	}
	s, err := t.State(project)
	if err != nil {
		return Conclusion{}, err
	}
	if err = s.Graph.RequireActive(); err != nil {
		return Conclusion{}, err
	}
	if fence.Run == "" || fence.Intent == "" || fence.Lease != "explore" {
		return Conclusion{}, Err(403, "evidence conclusion requires an Execute lease")
	}
	if err = t.CheckExecution(s.Graph, fence); err != nil {
		return Conclusion{}, err
	}
	if s.Graph.Project.OrchestrationVersion == 1 {
		e, err := t.executionForLease(project, fence.Run)
		if err != nil {
			return Conclusion{}, err
		}
		if err = t.CheckExecutionDependencies(e); err != nil {
			return Conclusion{}, err
		}
	}
	// A final Fact, its receipt and the Step completion event must roll back
	// together even when the enclosing caller handles a rejected conclusion.
	if _, err = t.transactionQueries.Exec("SAVEPOINT pwnmesh_management"); err != nil {
		return Conclusion{}, err
	}
	defer t.finishManagement(&err)
	if len(factPayload) != 0 {
		created, err := t.StateAction(project, fence, StateAction{Op: "fact", IdempotencyKey: fence.Run + ":final-fact", Payload: factPayload})
		if err != nil {
			return Conclusion{}, err
		}
		factID = created.ID
		s, err = t.State(project)
		if err != nil {
			return Conclusion{}, err
		}
	}
	var fact *FactRecord
	for n := range s.FactRecords {
		if s.FactRecords[n].ID == factID {
			fact = &s.FactRecords[n]
			break
		}
	}
	if fact == nil {
		return Conclusion{}, Err(404, "conclusion fact does not exist")
	}
	if fact.Legacy || fact.Status != "valid" || fact.SupportInvalid || fact.SourceStepID != fence.Intent || len(fact.Evidence) == 0 {
		return Conclusion{}, Err(409, "conclusion requires an effective evidence-backed observation from this Step")
	}
	if s.Graph.Project.OrchestrationVersion == 1 && !runMatches(fence.Run, fact.RunID) {
		return Conclusion{}, Err(409, "conclusion observation must come from the current execution")
	}
	for n := range s.Graph.Intents {
		i := &s.Graph.Intents[n]
		if i.ID != fence.Intent {
			continue
		}
		if i.To != nil || i.ConcludedAt != nil {
			return Conclusion{}, Err(409, "Step already concluded")
		}
		i.To, i.Worker, i.Heartbeat, i.ConcludedAt = Ptr(factID), Ptr(fence.Run), Ptr(t.Now), Ptr(t.Now)
		if err = sqlstore.SaveIntentConclusion(t.transactionQueries, project, *i); err != nil {
			return Conclusion{}, err
		}
		result := Conclusion{Fact: Fact{ID: fact.ID, Description: fact.Description}, Intent: *i}
		revision, err := t.advanceStateRevision(project, true)
		if err != nil {
			return Conclusion{}, err
		}
		payload, _ := json.Marshal(map[string]string{"fact_id": factID})
		encoded, _ := json.Marshal(result)
		event, _ := json.Marshal(StateEvent{Revision: revision, Op: "step_completed", ID: fence.Intent, RunID: fence.Run, CreatedAt: t.Now, Payload: payload, Result: encoded})
		err = sqlstore.InsertStateEvent(t.transactionQueries, project, revision, string(event))
		return result, err
	}
	return Conclusion{}, Err(404, "Step not found")
}
