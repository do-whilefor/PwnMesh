package board

import "pwnmesh/internal/board/internal/sqlstore"

// RestartProject begins a fresh board round, preserving human inputs. Lease
// tombstones and ID counters deliberately survive: late workers must never
// regain authority or accidentally address a replacement step with an old ID.
// The prior round is archived before resetting its current projections. This
// operation never changes project workspace files or historical snapshots.
func (t *Tx) RestartProject(project string, expected *int64) (out Graph, err error) {
	g, err := t.Load(project)
	if err != nil {
		return Graph{}, err
	}
	if expected != nil && *expected != g.Project.Generation {
		return Graph{}, Err(409, "Project was already restarted; refresh before retrying")
	}
	inputs := []Fact{}
	for _, fact := range g.Facts {
		if fact.ID == "origin" || fact.ID == "goal" {
			inputs = append(inputs, fact)
		}
	}
	if len(inputs) != 2 {
		return Graph{}, Err(409, "Project is missing its original input or goal")
	}
	if _, err = t.Exec("SAVEPOINT pwnmesh_management"); err != nil {
		return Graph{}, err
	}
	defer t.finishManagement(&err)
	if err = t.archiveRound(project); err != nil {
		return Graph{}, err
	}
	if err = t.RevokeRuns(project); err != nil {
		return Graph{}, err
	}
	g.Project.Status, g.Project.Reason = "active", nil
	g.Project.Curator = nil
	g.Project.Generation++
	g.Project.RestartedAt = t.Now
	g.Project.TerminatedAt = ""
	g.Facts, g.Intents = inputs, []Intent{}
	if err = sqlstore.ResetProjectRound(t, project, g.Project.Generation, t.Now); err != nil {
		return Graph{}, err
	}
	return g, t.Save(g)
}
