package board

import (
	"encoding/json"

	"pwnmesh/internal/board/internal/sqlstore"
)

// Termination ends an unfinished project without asserting that its Goal was
// achieved. The graph, evidence, history and workspace survive for inspection.
// Only an explicit restart may start another execution round afterward.
func (t *Tx) TerminateProject(project string, expected *int64) (out Graph, err error) {
	g, err := t.Load(project)
	if err != nil {
		return Graph{}, err
	}
	if expected != nil && *expected != g.Project.Generation {
		return Graph{}, Err(409, "Project was restarted; refresh before terminating")
	}
	if g.Project.Status == "terminated" {
		return g, nil
	}
	if g.Project.Status != "active" && g.Project.Status != "stopped" {
		return Graph{}, Err(409, "Only active or stopped projects can be terminated")
	}
	if _, err = t.transactionQueries.Exec("SAVEPOINT pwnmesh_management"); err != nil {
		return Graph{}, err
	}
	defer t.finishManagement(&err)
	if err = t.RevokeRuns(project); err != nil {
		return Graph{}, err
	}
	ids, err := sqlstore.PendingProjectExecutionIDs(t.transactionQueries, project)
	if err != nil {
		return Graph{}, err
	}
	for _, id := range ids {
		e, err := t.Execution(project, id)
		if err != nil {
			return Graph{}, err
		}
		failure := json.RawMessage(`{"status":"failed","failure_kind":"project_terminated","error":"project terminated by user"}`)
		// A result_pending execution keeps its immutable saved result. Its
		// cancellation diagnostic is added to history, never applied as a Fact.
		if err = t.ExecutionStatus(e, "cancelled", failure); err != nil {
			return Graph{}, err
		}
	}
	if err = sqlstore.DeletePausedExecutions(t.transactionQueries, project); err != nil {
		return Graph{}, err
	}
	// Cancel outstanding one-use retry grants as well as running work, while
	// retaining the attempt's stored result and original failure history.
	if err = sqlstore.CancelProjectRetries(t.transactionQueries, project, t.Now); err != nil {
		return Graph{}, err
	}
	g.Project.Status, g.Project.Reason, g.Project.TerminatedAt = "terminated", nil, t.Now
	g.Project.Curator = nil
	for index := range g.Intents {
		if g.Intents[index].ConcludedAt == nil {
			g.Intents[index].Worker = nil
		}
	}
	if err = sqlstore.RecordProjectTermination(t.transactionQueries, project, t.Now); err != nil {
		return Graph{}, err
	}
	return g, t.Save(g)
}
