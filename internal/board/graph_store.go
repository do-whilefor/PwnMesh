package board

import (
	"database/sql"
	"errors"

	"pwnmesh/internal/board/internal/sqlstore"
)

func (t *Tx) Settings() (Settings, error) {
	return sqlstore.ReadSettings(t.transactionQueries)
}

func (t *Tx) Next(project, kind string) (string, error) {
	return sqlstore.NextID(t.transactionQueries, project, kind)
}

func (t *Tx) IDs() ([]string, error) {
	return sqlstore.ProjectIDs(t.transactionQueries)
}

func (t *Tx) Load(id string) (Graph, error) {
	g, err := sqlstore.LoadGraph(t.transactionQueries, id)
	if errors.Is(err, sql.ErrNoRows) {
		return g, Err(404, "Project not found")
	}
	if err == nil && g.Project.OrchestrationVersion != 0 && g.Project.OrchestrationVersion != 1 {
		err = Err(422, "unsupported orchestration_version")
	}
	return g, err
}

func (t *Tx) RequireProject(project string) error {
	exists, err := sqlstore.ProjectExists(t.transactionQueries, project)
	if err != nil {
		return err
	}
	if !exists {
		return Err(404, "Project not found")
	}
	return nil
}

// Board owns admission; storage owns row mapping. Reject every domain error
// before writing any graph rows, including when a caller handles that error.
func (t *Tx) Save(g Graph) error {
	if err := t.checkOrchestration(g.Project); err != nil {
		return err
	}
	if g.Project.Scenario != "" && !ValidScenario(g.Project.Scenario) {
		return Err(422, "scenario must be ctf, pentest or audit")
	}
	if err := validateCuratorVersion(g.Project); err != nil {
		return err
	}
	return sqlstore.SaveGraph(t.transactionQueries, g)
}

// Protocol selection is immutable after project creation.
func (t *Tx) checkOrchestration(p Project) error {
	if p.OrchestrationVersion < 0 || p.OrchestrationVersion > 1 {
		return Err(422, "unsupported orchestration_version")
	}
	version, err := sqlstore.ProjectVersion(t.transactionQueries, p.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if version != p.OrchestrationVersion {
		return Err(409, "orchestration_version is immutable after project creation")
	}
	return nil
}

func validateCuratorVersion(p Project) error {
	if p.Curator != nil && p.OrchestrationVersion != 1 {
		return Err(422, "curator requires orchestration version 1")
	}
	return nil
}

func (t *Tx) saveOrchestration(p Project) error {
	if err := validateCuratorVersion(p); err != nil {
		return err
	}
	return sqlstore.SaveOrchestration(t.transactionQueries, p)
}

func (t *Tx) saveIntent(project string, intent Intent) error {
	return sqlstore.SaveIntent(t.transactionQueries, project, intent)
}
