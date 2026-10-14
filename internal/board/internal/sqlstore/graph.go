package sqlstore

import (
	"database/sql"
	"errors"
	"fmt"

	"pwnmesh/internal/board/model"
)

// Graph row operations use the caller's existing transaction. They neither
// authorize domain changes nor create, commit or roll back transactions.
// Board validates proposed graphs and composes mutations before calling them.
func ReadSettings(q Queries) (model.Settings, error) {
	var s model.Settings
	err := q.QueryRow("SELECT intent_timeout,reason_timeout FROM settings WHERE rowid=1").Scan(&s.IntentTimeout, &s.ReasonTimeout)
	return s, err
}

func NextID(q Queries, project, kind string) (string, error) {
	if kind != "project" && kind != "fact" && kind != "intent" && kind != "hint" {
		return "", fmt.Errorf("unknown ID kind %q", kind)
	}
	var n int
	var err error
	if kind == "project" {
		err = q.QueryRow("UPDATE counters SET value=value+1 WHERE name='project' RETURNING value").Scan(&n)
		return fmt.Sprintf("proj_%03d", n), err
	}
	_, err = q.Exec("INSERT OR IGNORE INTO scoped_counters(project_id,kind,value) VALUES(?,?,0)", project, kind)
	if err != nil {
		return "", err
	}
	err = q.QueryRow("UPDATE scoped_counters SET value=value+1 WHERE project_id=? AND kind=? RETURNING value", project, kind).Scan(&n)
	return fmt.Sprintf("%s%03d", kind[:1], n), err
}

func ProjectIDs(q Queries) ([]string, error) {
	rows, err := q.Query("SELECT id FROM projects ORDER BY created_at,rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func LoadGraph(q Queries, id string) (model.Graph, error) {
	g := model.Graph{Facts: []model.Fact{}, Intents: []model.Intent{}, Hints: []model.Hint{}}
	var rw, rt, rs, rh *string
	err := q.QueryRow(`SELECT p.id,p.title,p.status,p.bootstrap_enabled,p.created_at,p.reason_worker,p.reason_trigger,p.reason_started_at,p.reason_last_heartbeat_at,COALESCE(m.scenario,''),COALESCE(round.generation,0),COALESCE(round.restarted_at,''),COALESCE(termination.terminated_at,'') FROM projects p LEFT JOIN pwnmesh_project_metadata m ON m.project_id=p.id LEFT JOIN pwnmesh_project_rounds round ON round.project_id=p.id LEFT JOIN pwnmesh_project_termination termination ON termination.project_id=p.id WHERE p.id=?`, id).Scan(&g.Project.ID, &g.Project.Title, &g.Project.Status, &g.Project.Bootstrap, &g.Project.CreatedAt, &rw, &rt, &rs, &rh, &g.Project.Scenario, &g.Project.Generation, &g.Project.RestartedAt, &g.Project.TerminatedAt)
	if err != nil {
		return g, err
	}
	if rw != nil {
		g.Project.Reason = &model.Reason{Worker: *rw, Trigger: model.Value(rt), StartedAt: model.Value(rs), Heartbeat: model.Value(rh)}
	}
	if err = loadOrchestration(q, &g.Project); err != nil {
		return g, err
	}
	rows, err := q.Query("SELECT id,description FROM facts WHERE project_id=? ORDER BY rowid", id)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		var f model.Fact
		if err = rows.Scan(&f.ID, &f.Description); err != nil {
			rows.Close()
			return g, err
		}
		g.Facts = append(g.Facts, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return g, err
	}
	rows, err = q.Query("SELECT id,content,creator,created_at FROM hints WHERE project_id=? ORDER BY created_at,rowid", id)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		var h model.Hint
		if err = rows.Scan(&h.ID, &h.Content, &h.Creator, &h.CreatedAt); err != nil {
			rows.Close()
			return g, err
		}
		g.Hints = append(g.Hints, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return g, err
	}
	rows, err = q.Query("SELECT id,to_fact_id,description,creator,worker,last_heartbeat_at,created_at,concluded_at FROM intents WHERE project_id=? ORDER BY created_at,rowid", id)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		i := model.Intent{From: []string{}}
		if err = rows.Scan(&i.ID, &i.To, &i.Description, &i.Creator, &i.Worker, &i.Heartbeat, &i.CreatedAt, &i.ConcludedAt); err != nil {
			rows.Close()
			return g, err
		}
		g.Intents = append(g.Intents, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return g, err
	}
	intentIndex := make(map[string]int, len(g.Intents))
	for n, intent := range g.Intents {
		intentIndex[intent.ID] = n
	}
	rows, err = q.Query("SELECT intent_id,fact_id FROM intent_sources WHERE project_id=? ORDER BY rowid", id)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		var intent, fact string
		if err = rows.Scan(&intent, &fact); err != nil {
			rows.Close()
			return g, err
		}
		if n, ok := intentIndex[intent]; ok {
			g.Intents[n].From = append(g.Intents[n].From, fact)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return g, err
	}
	return g, nil
}

func loadOrchestration(q Queries, p *model.Project) error {
	var worker, trigger, started, heartbeat *string
	err := q.QueryRow("SELECT version,curator_worker,curator_trigger,curator_started_at,curator_heartbeat FROM pwnmesh_project_orchestration WHERE project_id=?", p.ID).Scan(&p.OrchestrationVersion, &worker, &trigger, &started, &heartbeat)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if worker != nil {
		p.Curator = &model.Reason{Worker: *worker, Trigger: model.Value(trigger), StartedAt: model.Value(started), Heartbeat: model.Value(heartbeat)}
	}
	return nil
}

func SaveGraph(q Queries, g model.Graph) error {
	p := g.Project
	var rw, rt, rs, rh *string
	if p.Reason != nil {
		r := p.Reason
		rw, rt, rs, rh = &r.Worker, &r.Trigger, &r.StartedAt, &r.Heartbeat
	}
	_, err := q.Exec(`INSERT INTO projects(id,title,status,bootstrap_enabled,created_at,reason_worker,reason_trigger,reason_started_at,reason_last_heartbeat_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET title=excluded.title,status=excluded.status,bootstrap_enabled=excluded.bootstrap_enabled,reason_worker=excluded.reason_worker,reason_trigger=excluded.reason_trigger,reason_started_at=excluded.reason_started_at,reason_last_heartbeat_at=excluded.reason_last_heartbeat_at`, p.ID, p.Title, p.Status, p.Bootstrap, p.CreatedAt, rw, rt, rs, rh)
	if err != nil {
		return err
	}
	if err = SaveOrchestration(q, p); err != nil {
		return err
	}
	// Older graph callers omit this optional metadata. Keep their updates from
	// silently clearing the scenario chosen by the project creator.
	if p.Scenario != "" {
		if _, err = q.Exec(`INSERT INTO pwnmesh_project_metadata(project_id,scenario) VALUES(?,?) ON CONFLICT(project_id) DO UPDATE SET scenario=excluded.scenario`, p.ID, p.Scenario); err != nil {
			return err
		}
	}
	for _, f := range g.Facts {
		if _, err = q.Exec("INSERT OR IGNORE INTO facts(id,project_id,description) VALUES(?,?,?)", f.ID, p.ID, f.Description); err != nil {
			return err
		}
	}
	for _, h := range g.Hints {
		if _, err = q.Exec("INSERT OR IGNORE INTO hints(id,project_id,content,creator,created_at) VALUES(?,?,?,?,?)", h.ID, p.ID, h.Content, h.Creator, h.CreatedAt); err != nil {
			return err
		}
	}
	for _, i := range g.Intents {
		if err = SaveIntent(q, p.ID, i); err != nil {
			return err
		}
	}
	return nil
}

func SaveOrchestration(q Queries, p model.Project) error {
	var worker, trigger, started, heartbeat *string
	if p.Curator != nil {
		worker, trigger, started, heartbeat = &p.Curator.Worker, &p.Curator.Trigger, &p.Curator.StartedAt, &p.Curator.Heartbeat
	}
	_, err := q.Exec(`INSERT INTO pwnmesh_project_orchestration(project_id,version,curator_worker,curator_trigger,curator_started_at,curator_heartbeat) VALUES(?,?,?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET curator_worker=excluded.curator_worker,curator_trigger=excluded.curator_trigger,curator_started_at=excluded.curator_started_at,curator_heartbeat=excluded.curator_heartbeat`, p.ID, p.OrchestrationVersion, worker, trigger, started, heartbeat)
	return err
}

func SaveIntent(q Queries, project string, i model.Intent) error {
	_, err := q.Exec(`INSERT INTO intents(id,project_id,to_fact_id,description,creator,worker,last_heartbeat_at,created_at,concluded_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id,project_id) DO UPDATE SET to_fact_id=excluded.to_fact_id,worker=excluded.worker,last_heartbeat_at=excluded.last_heartbeat_at,concluded_at=excluded.concluded_at`, i.ID, project, i.To, i.Description, i.Creator, i.Worker, i.Heartbeat, i.CreatedAt, i.ConcludedAt)
	if err != nil {
		return err
	}
	for _, fid := range i.From {
		if _, err = q.Exec("INSERT OR IGNORE INTO intent_sources(intent_id,project_id,fact_id) VALUES(?,?,?)", i.ID, project, fid); err != nil {
			return err
		}
	}
	return nil
}

// ProjectVersion reads the persisted protocol. A missing orchestration row on
// an existing project means version zero, matching the stored graph contract.
func ProjectVersion(q Queries, project string) (int, error) {
	var version int
	err := q.QueryRow("SELECT COALESCE(o.version,0) FROM projects p LEFT JOIN pwnmesh_project_orchestration o ON o.project_id=p.id WHERE p.id=?", project).Scan(&version)
	return version, err
}

func ProjectExists(q Queries, project string) (bool, error) {
	var exists bool
	err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM projects WHERE id=?)", project).Scan(&exists)
	return exists, err
}

func WriteSettings(q Queries, settings model.Settings) error {
	_, err := q.Exec("UPDATE settings SET intent_timeout=?,reason_timeout=? WHERE rowid=1", settings.IntentTimeout, settings.ReasonTimeout)
	return err
}

func SetProjectTitle(q Queries, project, title string) error {
	_, err := q.Exec("UPDATE projects SET title=? WHERE id=?", title, project)
	return err
}

func DeleteProject(q Queries, project string) error {
	_, err := q.Exec("DELETE FROM projects WHERE id=?", project)
	return err
}
