package board

import (
	"database/sql"
	"encoding/json"
	"errors"
)

const orchestrationSchema = `CREATE TABLE IF NOT EXISTS xloom_project_orchestration(
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 version INTEGER NOT NULL,curator_worker TEXT,curator_trigger TEXT,curator_started_at TEXT,curator_heartbeat TEXT);`

// Bound the transport envelope, not the retained immutable input. New curator
// jobs use a snapshot and bounded view; legacy inline jobs keep this limit.
const MaxCurationInputBytes = 256 << 10

// CurationJobBoundary supports both retained inline jobs and server-owned
// snapshots. The revision and version come from the registered job, never from
// a model-supplied boundary. Snapshot metadata is checked against storage when
// the execution is registered.
func CurationJobBoundary(raw json.RawMessage) (string, int64, error) {
	var job struct {
		Kind          string          `json:"kind"`
		Graph         Graph           `json:"graph"`
		State         *State          `json:"state"`
		InputSnapshot *InputSnapshot  `json:"input_snapshot"`
		InputView     json.RawMessage `json:"input_view"`
	}
	if json.Unmarshal(raw, &job) != nil || job.Kind != "curate" || job.Graph.Project.OrchestrationVersion != 1 {
		return "", 0, Err(422, "invalid registered curation input")
	}
	if ref := job.InputSnapshot; ref != nil {
		if job.State != nil || ref.Version != 1 || len(ref.ID) != 64 || len(ref.StateVersion) != 64 || ref.ProjectID != job.Graph.Project.ID || ref.Generation != job.Graph.Project.Generation || ref.Revision < 0 || len(job.Graph.Facts)+len(job.Graph.Intents)+len(job.Graph.Hints) != 0 || len(job.InputView) > DefaultContextViewBytes || !json.Valid(job.InputView) {
			return "", 0, Err(422, "invalid curation snapshot binding")
		}
		return ref.StateVersion, ref.Revision, nil
	}
	if job.State == nil || len(job.InputView) != 0 || job.State.Graph.Project.ID != job.Graph.Project.ID || job.State.Graph.Project.Generation != job.Graph.Project.Generation || job.State.Graph.Project.OrchestrationVersion != 1 {
		return "", 0, Err(422, "invalid registered curation input")
	}
	bound := *job.State
	bound.Graph = job.Graph
	version := DecisionStateVersion(*job.State)
	if DecisionStateVersion(bound) != version {
		return "", 0, Err(422, "curation graph and input state do not match")
	}
	return version, job.State.Revision, nil
}

// Protocol selection is a project creation decision. An old reader cannot
// silently switch an existing project's evidence-writing authority.
func (t *Tx) checkOrchestration(p Project) error {
	if p.OrchestrationVersion < 0 || p.OrchestrationVersion > 1 {
		return Err(422, "unsupported orchestration_version")
	}
	var version int
	err := t.QueryRow(`SELECT COALESCE(o.version,0) FROM projects p LEFT JOIN xloom_project_orchestration o ON o.project_id=p.id WHERE p.id=?`, p.ID).Scan(&version)
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

func (t *Tx) loadOrchestration(p *Project) error {
	var worker, trigger, started, heartbeat *string
	err := t.QueryRow("SELECT version,curator_worker,curator_trigger,curator_started_at,curator_heartbeat FROM xloom_project_orchestration WHERE project_id=?", p.ID).Scan(&p.OrchestrationVersion, &worker, &trigger, &started, &heartbeat)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if p.OrchestrationVersion != 0 && p.OrchestrationVersion != 1 {
		return Err(422, "unsupported orchestration_version")
	}
	if worker != nil {
		p.Curator = &Reason{Worker: *worker, Trigger: Value(trigger), StartedAt: Value(started), Heartbeat: Value(heartbeat)}
	}
	return nil
}

func (t *Tx) saveOrchestration(p Project) error {
	var worker, trigger, started, heartbeat *string
	if p.Curator != nil {
		if p.OrchestrationVersion != 1 {
			return Err(422, "curator requires orchestration version 1")
		}
		worker, trigger, started, heartbeat = &p.Curator.Worker, &p.Curator.Trigger, &p.Curator.StartedAt, &p.Curator.Heartbeat
	}
	_, err := t.Exec(`INSERT INTO xloom_project_orchestration(project_id,version,curator_worker,curator_trigger,curator_started_at,curator_heartbeat) VALUES(?,?,?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET curator_worker=excluded.curator_worker,curator_trigger=excluded.curator_trigger,curator_started_at=excluded.curator_started_at,curator_heartbeat=excluded.curator_heartbeat`, p.ID, p.OrchestrationVersion, worker, trigger, started, heartbeat)
	return err
}

func (t *Tx) ClaimCurator(project, worker, trigger string) (Reason, error) {
	g, err := t.Load(project)
	if err != nil {
		return Reason{}, err
	}
	if err = g.RequireActive(); err != nil {
		return Reason{}, err
	}
	if g.Project.OrchestrationVersion != 1 || !required(worker, 256) || !required(trigger, 4096) {
		return Reason{}, Err(422, "curator requires version 1, worker and trigger")
	}
	if revoked, err := t.RunRevoked(project, worker); err != nil {
		return Reason{}, err
	} else if revoked {
		return Reason{}, Err(409, "Execution was revoked")
	}
	if current := g.Project.Curator; current != nil {
		if current.Worker != worker {
			return Reason{}, Err(409, "Another execution owns curation")
		}
		return *current, nil
	}
	lease := Reason{Worker: worker, Trigger: trigger, StartedAt: t.Now, Heartbeat: t.Now}
	g.Project.Curator = &lease
	return lease, t.saveOrchestration(g.Project)
}

func (t *Tx) HeartbeatCurator(project, worker string) error {
	g, err := t.Load(project)
	if err != nil {
		return err
	}
	if err = g.RequireActive(); err != nil {
		return err
	}
	if err = t.CheckExecution(g, ExecutionFence{Run: worker, Lease: "curate"}); err != nil {
		return err
	}
	g.Project.Curator.Heartbeat = t.Now
	return t.saveOrchestration(g.Project)
}

func (t *Tx) ReleaseCurator(project, worker string) error {
	// Conditional release is safe after a successful execution revoked its run.
	_, err := t.Exec(`UPDATE xloom_project_orchestration SET curator_worker=NULL,curator_trigger=NULL,curator_started_at=NULL,curator_heartbeat=NULL WHERE project_id=? AND curator_worker=?`, project, worker)
	return err
}

func controlKind(kind string) bool { return kind == "reason" || kind == "curate" }

// CurationNeeded discovers reconciliation work. Ordinary evidence and completed
// exploration steps can return directly to planning without a curator turn.
func (t *Tx) CurationNeeded(project string) (bool, error) {
	s, err := t.State(project)
	if err != nil {
		return false, err
	}
	return t.curationNeeded(s)
}

func (t *Tx) curationNeeded(s State) (bool, error) {
	if s.Graph.Project.OrchestrationVersion != 1 {
		return false, nil
	}
	if s.PendingCurationRequest() != nil {
		return true, nil
	}
	if len(s.PendingCurationCandidateIDs()) > 0 {
		return true, nil
	}
	// Ordinary tasks may never move the curation cursor. Do not rescan their
	// complete event history unless there is a review or conclusion to revisit.
	reviews := []Dispute{}
	unsupported := false
	for _, dispute := range s.Disputes {
		if dispute.Status == "resolved" {
			unsupported = unsupported || s.ValidateFactSources(dispute.ReviewFactIDs, true) != nil
		} else if len(dispute.ReviewStepIDs) > 0 {
			reviews = append(reviews, dispute)
		}
	}
	for _, finding := range s.Findings {
		unsupported = unsupported || finding.Status != "candidate" && !finding.SupportValid
	}
	if len(reviews) == 0 && !unsupported {
		return false, nil
	}
	// A review fact is usable only after its independent execution succeeds.
	// Completion remains durable even if an earlier optional scan saw the fact
	// while that execution was running. Normal producer completion is irrelevant.
	rows, err := t.Query(`SELECT json_extract(event,'$.op'),json_extract(event,'$.id') FROM xloom_state_events WHERE project_id=? AND revision>? AND json_extract(event,'$.op') IN ('step_completed','fact_relation')`, s.Graph.Project.ID, s.Curation.ThroughRevision)
	if err != nil {
		return false, err
	}
	completed, invalidated := map[string]bool{}, false
	for rows.Next() {
		var op, id string
		if err := rows.Scan(&op, &id); err != nil {
			rows.Close()
			return false, err
		}
		if op == "step_completed" {
			completed[id] = true
		} else {
			invalidated = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if invalidated && unsupported {
		return true, nil
	}
	for _, dispute := range reviews {
		for _, fact := range s.FactRecords {
			if completed[fact.SourceStepID] && t.validateReviewFacts(s, dispute, []string{fact.ID}) == nil {
				return true, nil
			}
		}
	}
	return false, nil
}

func CurationRetryKey(s State) string { return "curate:" + DecisionStateVersion(s) }

// Receipt reads remain available after the live lease ends. The HTTP layer
// checks that the caller possesses the registered immutable run identity.
func (t *Tx) CurationReceipt(project, run string) (StateActionResult, error) {
	var response string
	err := t.QueryRow(`SELECT a.response FROM xloom_state_actions a WHERE a.project_id=? AND json_extract(a.request,'$.Run')=? AND json_extract(a.request,'$.Op')='curate' LIMIT 1`, project, run).Scan(&response)
	if errors.Is(err, sql.ErrNoRows) {
		return StateActionResult{}, Err(404, "Curation receipt not found")
	}
	if err != nil {
		return StateActionResult{}, err
	}
	var result StateActionResult
	err = json.Unmarshal([]byte(response), &result)
	return result, err
}
