package board

import "pwnmesh/internal/board/internal/sqlstore"

// RestartProject begins a fresh board round, preserving human inputs. Lease
// tombstones and ID counters deliberately survive: late workers must never
// regain authority or accidentally address a replacement step with an old ID.
// The prior round is archived before resetting its current projections. This
// operation never changes project workspace files or historical snapshots.
func (t *Tx) RestartProject(project string, expected *int64) (Graph, error) {
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
	if err = t.archiveRound(project); err != nil {
		return Graph{}, err
	}
	if err = t.RevokeRuns(project); err != nil {
		return Graph{}, err
	}
	if _, err = t.Exec("DELETE FROM pwnmesh_project_termination WHERE project_id=?", project); err != nil {
		return Graph{}, err
	}
	if err = sqlstore.DeleteStateRecords(t, project); err != nil {
		return Graph{}, err
	}
	for _, query := range []string{
		"DELETE FROM pwnmesh_executions WHERE project_id=?",
		// The generation fence rejects every old-round cursor. Keep the
		// revision sequence, but discard the deletion markers just generated.
		"DELETE FROM pwnmesh_ui_execution_changes WHERE project_id=?",
		"DELETE FROM intents WHERE project_id=?",
		"DELETE FROM facts WHERE project_id=? AND id NOT IN ('origin','goal')",
	} {
		if _, err = t.Exec(query, project); err != nil {
			return Graph{}, err
		}
	}
	g.Project.Status, g.Project.Reason = "active", nil
	g.Project.Curator = nil
	g.Project.Generation++
	g.Project.RestartedAt = t.Now
	g.Project.TerminatedAt = ""
	g.Facts, g.Intents = inputs, []Intent{}
	if _, err = t.Exec(`INSERT INTO pwnmesh_project_rounds(project_id,generation,restarted_at) VALUES(?,?,?) ON CONFLICT(project_id) DO UPDATE SET generation=excluded.generation,restarted_at=excluded.restarted_at`, project, g.Project.Generation, t.Now); err != nil {
		return Graph{}, err
	}
	return g, t.Save(g)
}
