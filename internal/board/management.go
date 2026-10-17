package board

import (
	"database/sql"
	"errors"
	"strings"

	"pwnmesh/internal/board/internal/sqlstore"
	"pwnmesh/internal/board/model"
)

// ProjectInput contains human input; Board owns initial identities and state.
type ProjectInput struct {
	Title, Origin, Goal, Scenario string
	StartPaused                   bool
	Hints                         []HintInput
}

type HintInput struct{ Content, Creator string }

func (t *Tx) RequireCurrentProject(project string) error {
	version, err := sqlstore.ProjectVersion(t.transactionQueries, project)
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
	if int64(settings.IntentTimeout) > model.MaxDurationSeconds || int64(settings.ReasonTimeout) > model.MaxDurationSeconds {
		return Err(422, "lease timeouts exceed the supported duration")
	}
	return sqlstore.WriteSettings(t.transactionQueries, settings)
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
	err = sqlstore.Atomic(t.transactionQueries, func() error {
		id, err := t.Next("", "project")
		if err != nil {
			return err
		}
		out = Graph{Project: Project{ID: id, Title: input.Title, Status: "active", CreatedAt: t.Now, Scenario: input.Scenario, OrchestrationVersion: 1}, Facts: []Fact{{ID: "origin", Description: input.Origin}, {ID: "goal", Description: input.Goal}}, Intents: []Intent{}, Hints: []Hint{}}
		if input.StartPaused {
			out.Project.Status = "stopped"
		}
		if err := t.Save(out); err != nil {
			return err
		}
		for _, input := range input.Hints {
			id, err := t.Next(out.Project.ID, "hint")
			if err != nil {
				return err
			}
			out.Hints = append(out.Hints, Hint{ID: id, Content: input.Content, Creator: input.Creator, CreatedAt: t.Now})
		}
		if err := t.Save(out); err != nil {
			return err
		}
		return t.CheckContextCapacity(out.Project.ID)
	})
	if err != nil {
		return Graph{}, err
	}
	return out, nil
}

// Management operations may be composed inside one Store.Do callback. A caller
// handling one rejection must not commit its counters, leases or partial input.
func (t *Tx) DeleteProject(project string) error {
	if _, err := t.Load(project); err != nil {
		return err
	}
	return sqlstore.DeleteProject(t.transactionQueries, project)
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
	err = sqlstore.SetProjectTitle(t.transactionQueries, project, title)
	return state.Graph.Project, err
}

func (t *Tx) SetProjectStatus(project, status string, expected *int64) (Project, error) {
	g, err := t.Load(project)
	if err != nil {
		return Project{}, err
	}
	if expected != nil && *expected != g.Project.Generation {
		return Project{}, Err(409, "Project round changed; refresh before changing status")
	}
	err = sqlstore.Atomic(t.transactionQueries, func() error {
		if err := t.SetStatus(&g, status); err != nil {
			return err
		}
		return t.Save(g)
	})
	if err != nil {
		return Project{}, err
	}
	return g.Project, nil
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
	err = sqlstore.Atomic(t.transactionQueries, func() error {
		id, err := t.Next(project, "hint")
		if err != nil {
			return err
		}
		out = Hint{ID: id, Content: input.Content, Creator: input.Creator, CreatedAt: t.Now}
		g.Hints = append(g.Hints, out)
		return t.SaveUserInput(g, "hint", out.ID, run, out, out)
	})
	if err != nil {
		return Hint{}, err
	}
	return out, nil
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
	err = sqlstore.Atomic(t.transactionQueries, func() error {
		if err := t.RevokeRuns(project); err != nil {
			return err
		}
		fid, err := t.Next(project, "fact")
		if err != nil {
			return err
		}
		iid, err := t.Next(project, "intent")
		if err != nil {
			return err
		}
		if err := sqlstore.DeleteIntent(t.transactionQueries, project, old.ID); err != nil {
			return err
		}
		fact := Fact{ID: fid, Description: description}
		intent := Intent{ID: iid, From: old.From, To: &fid, Description: "external_feedback", Creator: creator, Worker: &creator, Heartbeat: Ptr(t.Now), CreatedAt: t.Now, ConcludedAt: Ptr(t.Now)}
		g.Intents = append(g.Intents[:index], g.Intents[index+1:]...)
		g.Intents = append(g.Intents, intent)
		g.Facts = append(g.Facts, fact)
		g.Project.Status, g.Project.Reason = "active", nil
		out = Reopened{Project: g.Project, Fact: fact, Intent: intent}
		return t.SaveUserInput(g, "reopen", fact.ID, run, map[string]string{"description": description, "creator": creator}, out)
	})
	if err != nil {
		return Reopened{}, err
	}
	return out, nil
}
