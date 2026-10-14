package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db      *sql.DB
	readers *sql.DB
	Now     func() time.Time
}
type Tx struct {
	*sql.Tx
	Now             string
	inDecisionBatch bool
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
	}
	// Acquire the write reservation at BEGIN, including when a second process
	// opens this database. This keeps check-and-claim atomic across connections.
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	db, err := sql.Open("sqlite", path+separator+"_txlock=immediate&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, Now: time.Now}
	// Reject unsupported existing stores before WAL changes their file header.
	if err = migrateStore(db); err != nil {
		db.Close()
		return nil, err
	}
	for _, q := range []string{"PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	if path != ":memory:" {
		// WAL readers keep a consistent view without reserving the only writer.
		// This pool is query-only at the connection level, so routing a mutation
		// through View fails instead of silently weakening its write fence.
		s.readers, err = sql.Open("sqlite", path+separator+"_txlock=deferred&_pragma=query_only(1)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
		if err == nil {
			s.readers.SetMaxOpenConns(4)
			err = s.readers.Ping()
		}
		if err != nil {
			if s.readers != nil {
				s.readers.Close()
			}
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Close() error {
	var readErr error
	if s.readers != nil {
		readErr = s.readers.Close()
	}
	return errors.Join(readErr, s.db.Close())
}

// View is for observational reads only; lease expiry and state changes remain
// in Do. A private :memory: database has only one connection, so test callers
// retain its serial transaction semantics instead of opening an empty database.
func (s *Store) View(ctx context.Context, fn func(*Tx) error) error {
	if s.readers == nil {
		return s.Do(ctx, fn)
	}
	t, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer t.Rollback()
	if err := fn(&Tx{Tx: t, Now: s.Now().UTC().Format(time.RFC3339)}); err != nil {
		return err
	}
	return t.Commit()
}

// The one-connection pool serializes transactions with context-aware waiting.
// SQLite's immediate transactions also serialize other Store instances.
func (s *Store) Do(ctx context.Context, fn func(*Tx) error) error {
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer t.Rollback()
	x := &Tx{Tx: t, Now: s.Now().UTC().Format(time.RFC3339)}
	if err = fn(x); err != nil {
		return err
	}
	return t.Commit()
}
func (t *Tx) Settings() (Settings, error) {
	var s Settings
	err := t.QueryRow("SELECT intent_timeout,reason_timeout FROM settings WHERE rowid=1").Scan(&s.IntentTimeout, &s.ReasonTimeout)
	return s, err
}

// RevokeRuns invalidates execution identities while retaining worker and
// heartbeat metadata on concluded intents. A resumed project must reject
// delayed writes from revoked workers.
func (t *Tx) RevokeRuns(project string) error {
	_, err := t.Exec(`INSERT OR IGNORE INTO pwnmesh_revoked_runs(project_id,worker)
SELECT project_id,worker FROM intents WHERE project_id=? AND worker IS NOT NULL
UNION SELECT id,reason_worker FROM projects WHERE id=? AND reason_worker IS NOT NULL
UNION SELECT project_id,lease FROM pwnmesh_executions WHERE project_id=?
UNION SELECT project_id,curator_worker FROM pwnmesh_project_orchestration WHERE project_id=? AND curator_worker IS NOT NULL`, project, project, project, project)
	return err
}

func (t *Tx) RunRevoked(project, worker string) (bool, error) {
	var revoked bool
	err := t.QueryRow("SELECT EXISTS(SELECT 1 FROM pwnmesh_revoked_runs WHERE project_id=? AND worker=?)", project, worker).Scan(&revoked)
	return revoked, err
}

// ExpireProject limits lease housekeeping to the project being observed or
// mutated. Global scheduling/list requests pass an empty project and still
// expire all leases, including those whose workers have crashed.
func (t *Tx) ExpireProject(project string) error {
	s, err := t.Settings()
	if err != nil {
		return err
	}
	for _, expiry := range []struct {
		query, projectColumn string
		timeout              int
	}{
		{`UPDATE intents SET worker=NULL WHERE to_fact_id IS NULL AND worker IS NOT NULL AND (julianday(?)-julianday(last_heartbeat_at))*86400>?`, "project_id", s.IntentTimeout},
		{`UPDATE projects SET reason_worker=NULL,reason_trigger=NULL,reason_started_at=NULL,reason_last_heartbeat_at=NULL WHERE reason_worker IS NOT NULL AND (julianday(?)-julianday(reason_last_heartbeat_at))*86400>?`, "id", s.ReasonTimeout},
		{`UPDATE pwnmesh_project_orchestration SET curator_worker=NULL,curator_trigger=NULL,curator_started_at=NULL,curator_heartbeat=NULL WHERE curator_worker IS NOT NULL AND (julianday(?)-julianday(curator_heartbeat))*86400>?`, "project_id", s.ReasonTimeout},
	} {
		args := []any{t.Now, expiry.timeout}
		expiry.query += " AND " + expiry.projectColumn + " IN (SELECT project_id FROM pwnmesh_project_orchestration WHERE version=1)"
		if project != "" {
			expiry.query += " AND " + expiry.projectColumn + "=?"
			args = append(args, project)
		}
		if _, err = t.Exec(expiry.query, args...); err != nil {
			return err
		}
	}
	return nil
}
func (t *Tx) Next(project, kind string) (string, error) {
	if kind != "project" && kind != "fact" && kind != "intent" && kind != "hint" {
		return "", fmt.Errorf("unknown ID kind %q", kind)
	}
	var n int
	var err error
	if kind == "project" {
		err = t.QueryRow("UPDATE counters SET value=value+1 WHERE name='project' RETURNING value").Scan(&n)
		return fmt.Sprintf("proj_%03d", n), err
	}
	_, err = t.Exec("INSERT OR IGNORE INTO scoped_counters(project_id,kind,value) VALUES(?,?,0)", project, kind)
	if err != nil {
		return "", err
	}
	err = t.QueryRow("UPDATE scoped_counters SET value=value+1 WHERE project_id=? AND kind=? RETURNING value", project, kind).Scan(&n)
	return fmt.Sprintf("%s%03d", kind[:1], n), err
}
func (t *Tx) IDs() ([]string, error) {
	rows, err := t.Query("SELECT id FROM projects ORDER BY created_at,rowid")
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
func (t *Tx) Load(id string) (Graph, error) {
	g := Graph{Facts: []Fact{}, Intents: []Intent{}, Hints: []Hint{}}
	var rw, rt, rs, rh *string
	err := t.QueryRow(`SELECT p.id,p.title,p.status,p.bootstrap_enabled,p.created_at,p.reason_worker,p.reason_trigger,p.reason_started_at,p.reason_last_heartbeat_at,COALESCE(m.scenario,''),COALESCE(round.generation,0),COALESCE(round.restarted_at,''),COALESCE(termination.terminated_at,'') FROM projects p LEFT JOIN pwnmesh_project_metadata m ON m.project_id=p.id LEFT JOIN pwnmesh_project_rounds round ON round.project_id=p.id LEFT JOIN pwnmesh_project_termination termination ON termination.project_id=p.id WHERE p.id=?`, id).Scan(&g.Project.ID, &g.Project.Title, &g.Project.Status, &g.Project.Bootstrap, &g.Project.CreatedAt, &rw, &rt, &rs, &rh, &g.Project.Scenario, &g.Project.Generation, &g.Project.RestartedAt, &g.Project.TerminatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return g, Err(404, "Project not found")
	}
	if err != nil {
		return g, err
	}
	if rw != nil {
		g.Project.Reason = &Reason{Worker: *rw, Trigger: Value(rt), StartedAt: Value(rs), Heartbeat: Value(rh)}
	}
	if err = t.loadOrchestration(&g.Project); err != nil {
		return g, err
	}
	rows, err := t.Query("SELECT id,description FROM facts WHERE project_id=? ORDER BY rowid", id)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		var f Fact
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
	rows, err = t.Query("SELECT id,content,creator,created_at FROM hints WHERE project_id=? ORDER BY created_at,rowid", id)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		var h Hint
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
	rows, err = t.Query("SELECT id,to_fact_id,description,creator,worker,last_heartbeat_at,created_at,concluded_at FROM intents WHERE project_id=? ORDER BY created_at,rowid", id)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		i := Intent{From: []string{}}
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
	rows, err = t.Query("SELECT intent_id,fact_id FROM intent_sources WHERE project_id=? ORDER BY rowid", id)
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

func (t *Tx) RequireProject(project string) error {
	var exists bool
	if err := t.QueryRow("SELECT EXISTS(SELECT 1 FROM projects WHERE id=?)", project).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return Err(404, "Project not found")
	}
	return nil
}

func (t *Tx) Save(g Graph) error {
	p := g.Project
	if err := t.checkOrchestration(p); err != nil {
		return err
	}
	if p.Scenario != "" && !ValidScenario(p.Scenario) {
		return Err(422, "scenario must be ctf, pentest or audit")
	}
	var rw, rt, rs, rh *string
	if p.Reason != nil {
		r := p.Reason
		rw, rt, rs, rh = &r.Worker, &r.Trigger, &r.StartedAt, &r.Heartbeat
	}
	_, err := t.Exec(`INSERT INTO projects(id,title,status,bootstrap_enabled,created_at,reason_worker,reason_trigger,reason_started_at,reason_last_heartbeat_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET title=excluded.title,status=excluded.status,bootstrap_enabled=excluded.bootstrap_enabled,reason_worker=excluded.reason_worker,reason_trigger=excluded.reason_trigger,reason_started_at=excluded.reason_started_at,reason_last_heartbeat_at=excluded.reason_last_heartbeat_at`, p.ID, p.Title, p.Status, p.Bootstrap, p.CreatedAt, rw, rt, rs, rh)
	if err != nil {
		return err
	}
	if err = t.saveOrchestration(p); err != nil {
		return err
	}
	// Older graph callers omit this optional metadata. Keep their updates from
	// silently clearing the scenario chosen by the project creator.
	if p.Scenario != "" {
		if _, err = t.Exec(`INSERT INTO pwnmesh_project_metadata(project_id,scenario) VALUES(?,?) ON CONFLICT(project_id) DO UPDATE SET scenario=excluded.scenario`, p.ID, p.Scenario); err != nil {
			return err
		}
	}
	for _, f := range g.Facts {
		if _, err = t.Exec("INSERT OR IGNORE INTO facts(id,project_id,description) VALUES(?,?,?)", f.ID, p.ID, f.Description); err != nil {
			return err
		}
	}
	for _, h := range g.Hints {
		if _, err = t.Exec("INSERT OR IGNORE INTO hints(id,project_id,content,creator,created_at) VALUES(?,?,?,?,?)", h.ID, p.ID, h.Content, h.Creator, h.CreatedAt); err != nil {
			return err
		}
	}
	for _, i := range g.Intents {
		if err = t.saveIntent(p.ID, i); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tx) saveIntent(project string, i Intent) error {
	_, err := t.Exec(`INSERT INTO intents(id,project_id,to_fact_id,description,creator,worker,last_heartbeat_at,created_at,concluded_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id,project_id) DO UPDATE SET to_fact_id=excluded.to_fact_id,worker=excluded.worker,last_heartbeat_at=excluded.last_heartbeat_at,concluded_at=excluded.concluded_at`, i.ID, project, i.To, i.Description, i.Creator, i.Worker, i.Heartbeat, i.CreatedAt, i.ConcludedAt)
	if err != nil {
		return err
	}
	for _, fid := range i.From {
		if _, err = t.Exec("INSERT OR IGNORE INTO intent_sources(intent_id,project_id,fact_id) VALUES(?,?,?)", i.ID, project, fid); err != nil {
			return err
		}
	}
	return nil
}
