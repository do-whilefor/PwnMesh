package board

import (
	"database/sql"
	"errors"
	"strings"
)

// ProjectInput contains human input; Board owns initial identities and state.
type ProjectInput struct {
	Title, Origin, Goal, Scenario string
	StartPaused                   bool
	Hints                         []HintInput
}

type HintInput struct{ Content, Creator string }

func (t *Tx) RequireCurrentProject(project string) error {
	var version int
	err := t.QueryRow(`SELECT COALESCE(o.version,0) FROM projects p LEFT JOIN pwnmesh_project_orchestration o ON o.project_id=p.id WHERE p.id=?`, project).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return Err(404, "Project not found")
	}
	if err != nil {
		return err
	}
	if version != 1 {
		return Err(409, "Unsupported project protocol; create a new project to continue")
	}
	return nil
}

func (t *Tx) UpdateSettings(settings Settings) error {
	if settings.IntentTimeout < 5 || settings.ReasonTimeout < 5 {
		return Err(422, "timeouts must be integers greater than or equal to 5")
	}
	_, err := t.Exec("UPDATE settings SET intent_timeout=?,reason_timeout=? WHERE rowid=1", settings.IntentTimeout, settings.ReasonTimeout)
	return err
}

func (t *Tx) CreateProject(input ProjectInput) (out Graph, err error) {
	if strings.TrimSpace(input.Title) == "" || strings.TrimSpace(input.Origin) == "" || strings.TrimSpace(input.Goal) == "" {
		return Graph{}, Err(422, "title, origin and goal must be non-empty strings")
	}
	if input.Scenario != "" && !ValidScenario(input.Scenario) {
		return Graph{}, Err(422, "scenario must be ctf, pentest or audit")
	}
	for _, hint := range input.Hints {
		if strings.TrimSpace(hint.Content) == "" || strings.TrimSpace(hint.Creator) == "" {
			return Graph{}, Err(422, "hint content and creator must be non-empty strings")
		}
	}
	if _, err = t.Exec("SAVEPOINT pwnmesh_management"); err != nil {
		return Graph{}, err
	}
	defer t.finishManagement(&err)
	id, err := t.Next("", "project")
	if err != nil {
		return Graph{}, err
	}
	g := Graph{Project: Project{ID: id, Title: input.Title, Status: "active", CreatedAt: t.Now, Scenario: input.Scenario, OrchestrationVersion: 1}, Facts: []Fact{{ID: "origin", Description: input.Origin}, {ID: "goal", Description: input.Goal}}, Intents: []Intent{}, Hints: []Hint{}}
	if input.StartPaused {
		g.Project.Status = "stopped"
	}
	if err = t.Save(g); err != nil {
		return Graph{}, err
	}
	for _, input := range input.Hints {
		id, err := t.Next(g.Project.ID, "hint")
		if err != nil {
			return Graph{}, err
		}
		g.Hints = append(g.Hints, Hint{ID: id, Content: input.Content, Creator: input.Creator, CreatedAt: t.Now})
	}
	if err = t.Save(g); err != nil {
		return Graph{}, err
	}
	return g, t.CheckContextCapacity(g.Project.ID)
}

func (t *Tx) DeleteProject(project string) error {
	if _, err := t.Load(project); err != nil {
		return err
	}
	_, err := t.Exec("DELETE FROM projects WHERE id=?", project)
	return err
}

func (t *Tx) SetProjectTitle(project, title string) (Project, error) {
	if strings.TrimSpace(title) == "" {
		return Project{}, Err(422, "title must be a non-empty string")
	}
	state, err := t.State(project)
	if err != nil {
		return Project{}, err
	}
	state.Graph.Project.Title = title
	if err = ValidateContextCapacity(state); err != nil {
		return Project{}, err
	}
	_, err = t.Exec("UPDATE projects SET title=? WHERE id=?", title, project)
	return state.Graph.Project, err
}

func (t *Tx) SetProjectStatus(project, status string, expected *int64) (out Project, err error) {
	g, err := t.Load(project)
	if err != nil {
		return Project{}, err
	}
	if expected != nil && *expected != g.Project.Generation {
		return Project{}, Err(409, "Project round changed; refresh before changing status")
	}
	if _, err = t.Exec("SAVEPOINT pwnmesh_management"); err != nil {
		return Project{}, err
	}
	defer t.finishManagement(&err)
	if err = t.SetStatus(&g, status); err != nil {
		return Project{}, err
	}
	return g.Project, t.Save(g)
}

func (t *Tx) AddProjectHint(project string, input HintInput, run string) (out Hint, err error) {
	if strings.TrimSpace(input.Content) == "" || strings.TrimSpace(input.Creator) == "" {
		return Hint{}, Err(422, "hint content and creator must be non-empty strings")
	}
	g, err := t.Load(project)
	if err != nil {
		return Hint{}, err
	}
	if g.Project.Status != "active" && g.Project.Status != "stopped" && g.Project.Status != "completed" {
		return Hint{}, Err(403, "Project is "+g.Project.Status)
	}
	if _, err = t.Exec("SAVEPOINT pwnmesh_management"); err != nil {
		return Hint{}, err
	}
	defer t.finishManagement(&err)
	id, err := t.Next(project, "hint")
	if err != nil {
		return Hint{}, err
	}
	hint := Hint{ID: id, Content: input.Content, Creator: input.Creator, CreatedAt: t.Now}
	g.Hints = append(g.Hints, hint)
	return hint, t.SaveUserInput(g, "hint", hint.ID, run, hint, hint)
}

func (t *Tx) ReopenProject(project, description, creator, run string) (out Reopened, err error) {
	if strings.TrimSpace(description) == "" || strings.TrimSpace(creator) == "" {
		return Reopened{}, Err(422, "description and creator must be non-empty strings")
	}
	g, err := t.Load(project)
	if err != nil {
		return Reopened{}, err
	}
	if g.Project.Status != "completed" {
		return Reopened{}, Err(403, "Project is "+g.Project.Status)
	}
	index := -1
	for n, intent := range g.Intents {
		if Value(intent.To) == "goal" {
			if index != -1 {
				return Reopened{}, Err(409, "Completed project has multiple completion intents")
			}
			index = n
		}
	}
	if index < 0 {
		return Reopened{}, Err(409, "Completed project is missing its completion intent")
	}
	old := g.Intents[index]
	if len(old.From) == 0 {
		return Reopened{}, Err(409, "Completion intent is missing its source facts")
	}
	if _, err = t.Exec("SAVEPOINT pwnmesh_management"); err != nil {
		return Reopened{}, err
	}
	defer t.finishManagement(&err)
	if err = t.RevokeRuns(project); err != nil {
		return Reopened{}, err
	}
	fid, err := t.Next(project, "fact")
	if err != nil {
		return Reopened{}, err
	}
	iid, err := t.Next(project, "intent")
	if err != nil {
		return Reopened{}, err
	}
	if _, err = t.Exec("DELETE FROM intents WHERE project_id=? AND id=?", project, old.ID); err != nil {
		return Reopened{}, err
	}
	fact := Fact{ID: fid, Description: description}
	intent := Intent{ID: iid, From: old.From, To: &fid, Description: "external_feedback", Creator: creator, Worker: &creator, Heartbeat: Ptr(t.Now), CreatedAt: t.Now, ConcludedAt: Ptr(t.Now)}
	g.Intents = append(g.Intents[:index], g.Intents[index+1:]...)
	g.Intents = append(g.Intents, intent)
	g.Facts = append(g.Facts, fact)
	g.Project.Status, g.Project.Reason = "active", nil
	result := Reopened{Project: g.Project, Fact: fact, Intent: intent}
	return result, t.SaveUserInput(g, "reopen", fact.ID, run, map[string]string{"description": description, "creator": creator}, result)
}

// Management operations may be composed inside one Store.Do callback. A caller
// handling one rejection must not commit its counters, leases or partial input.
func (t *Tx) finishManagement(resultErr *error) {
	if *resultErr != nil {
		_, rollbackErr := t.Exec("ROLLBACK TO pwnmesh_management")
		*resultErr = errors.Join(*resultErr, rollbackErr)
	}
	_, releaseErr := t.Exec("RELEASE pwnmesh_management")
	*resultErr = errors.Join(*resultErr, releaseErr)
}
