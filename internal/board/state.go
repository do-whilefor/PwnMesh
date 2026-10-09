package board

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"pwnmesh/internal/artifactcheck"
)

const stateSchema = `
CREATE TABLE IF NOT EXISTS xloom_state(project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,data TEXT NOT NULL,revision INTEGER NOT NULL DEFAULT 0,decision_revision INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS xloom_state_actions(project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,idempotency_key TEXT NOT NULL,request TEXT NOT NULL,response TEXT NOT NULL,PRIMARY KEY(project_id,idempotency_key));
CREATE TABLE IF NOT EXISTS xloom_state_events(project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,revision INTEGER NOT NULL,event TEXT NOT NULL,PRIMARY KEY(project_id,revision));`

// State extends the Cairn graph without changing the legacy JSON representation.
type State struct {
	Graph            Graph            `json:"graph"`
	Goals            []Goal           `json:"goals"`
	Steps            []Step           `json:"steps"`
	FactRecords      []FactRecord     `json:"fact_records"`
	Findings         []Finding        `json:"findings"`
	FactRelations    []FactRelation   `json:"fact_relations"`
	Revision         int64            `json:"revision"`
	DecisionRevision int64            `json:"decision_revision"`
	Candidates       []Candidate      `json:"candidates,omitempty"`
	Disputes         []Dispute        `json:"disputes,omitempty"`
	Curation         CurationProgress `json:"curation,omitempty"`
	Assets           []Asset          `json:"assets,omitempty"`
	AssetAnchors     []AssetAnchor    `json:"asset_anchors,omitempty"`
}

// Omit the new zero-value curation view from legacy wire snapshots, preserving
// persisted decision hashes across an upgrade from the previous protocol.
func (s State) MarshalJSON() ([]byte, error) {
	type stateAlias State
	view := struct {
		*stateAlias
		Curation *CurationProgress `json:"curation,omitempty"`
	}{stateAlias: (*stateAlias)(&s)}
	if s.Graph.Project.OrchestrationVersion == 1 {
		view.Curation = &s.Curation
	}
	return json.Marshal(view)
}

type EvidenceRef struct {
	RunID     string `json:"run_id"`
	Path      string `json:"path"`
	Excerpt   string `json:"excerpt"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}
type FactRecord struct {
	ID             string        `json:"id"`
	Description    string        `json:"description"`
	Scope          string        `json:"scope"`
	ObservedAt     string        `json:"observed_at"`
	Evidence       []EvidenceRef `json:"evidence"`
	Status         string        `json:"status"`
	RunID          string        `json:"run_id,omitempty"`
	SourceStepID   string        `json:"source_step_id,omitempty"`
	Legacy         bool          `json:"legacy"`
	SupportInvalid bool          `json:"support_invalid,omitempty"`
}
type FactRelation struct {
	Kind      string `json:"kind"`
	Source    string `json:"source"`
	Target    string `json:"target"`
	Reason    string `json:"reason"`
	RunID     string `json:"run_id"`
	CreatedAt string `json:"created_at"`
}
type Goal struct {
	ID           string   `json:"id"`
	ParentID     string   `json:"parent_id,omitempty"`
	Condition    string   `json:"condition"`
	Status       string   `json:"status"`
	Sources      []string `json:"sources"`
	Reason       string   `json:"reason,omitempty"`
	CreatedAt    string   `json:"created_at"`
	SupportValid bool     `json:"support_valid"`
}
type Step struct {
	Repair         *artifactcheck.Spec `json:"repair,omitempty"`
	ID             string              `json:"id"`
	From           []string            `json:"from"`
	GoalID         string              `json:"goal_id"`
	Description    string              `json:"description"`
	Status         string              `json:"status"`
	Priority       int                 `json:"priority"`
	Result         *string             `json:"result"`
	Worker         *string             `json:"worker"`
	Reason         string              `json:"reason,omitempty"`
	CreatedAt      string              `json:"created_at"`
	InvalidSources []string            `json:"invalid_sources,omitempty"`
	DisputeID      string              `json:"dispute_id,omitempty"`
	DependsOn      []string            `json:"depends_on,omitempty"`
	WritePaths     []string            `json:"write_paths,omitempty"`
	BlockedBy      []string            `json:"blocked_by,omitempty"`
	SupportValid   bool                `json:"support_valid,omitempty"`
	// The latest failed or retry-authorized attempt. Ordinary first starts do
	// not stale plans; consuming an explicit retry grant does change this ID.
	LatestRunID string `json:"latest_run_id,omitempty"`
}
type Finding struct {
	ID              string        `json:"id"`
	Claim           string        `json:"claim"`
	Scope           string        `json:"scope"`
	Status          string        `json:"status"`
	Sources         []string      `json:"sources"`
	Evidence        []EvidenceRef `json:"evidence"`
	Reason          string        `json:"reason,omitempty"`
	CreatedAt       string        `json:"created_at"`
	UpdatedAt       string        `json:"updated_at"`
	SupportValid    bool          `json:"support_valid"`
	CandidateIDs    []string      `json:"candidate_ids,omitempty"`
	DisputeID       string        `json:"dispute_id,omitempty"`
	CuratedRevision int64         `json:"curated_revision,omitempty"`
}
type StateAction struct {
	Op              string          `json:"op"`
	IdempotencyKey  string          `json:"idempotency_key"`
	Payload         json.RawMessage `json:"payload"`
	ExpectedVersion string          `json:"expected_version,omitempty"`
}
type StateActionResult struct {
	Op           string          `json:"op"`
	ID           string          `json:"id"`
	Revision     int64           `json:"revision"`
	Result       json.RawMessage `json:"result"`
	StateVersion string          `json:"state_version,omitempty"`
	Unchanged    bool            `json:"unchanged,omitempty"`
	Committed    bool            `json:"committed,omitempty"`
	AssetIDs     []string        `json:"asset_ids,omitempty"`
}
type StateEvent struct {
	Revision  int64           `json:"revision"`
	Op        string          `json:"op"`
	ID        string          `json:"id"`
	RunID     string          `json:"run_id"`
	CreatedAt string          `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
	Result    json.RawMessage `json:"result"`
}
type stateData struct {
	Goals         []Goal           `json:"goals"`
	Steps         []stepMetadata   `json:"steps"`
	Facts         []FactRecord     `json:"facts"`
	Findings      []Finding        `json:"findings"`
	FactRelations []FactRelation   `json:"fact_relations"`
	Candidates    []Candidate      `json:"candidates,omitempty"`
	Disputes      []Dispute        `json:"disputes,omitempty"`
	Curation      CurationProgress `json:"curation,omitempty"`
}

// Keep the legacy status key for rollback readers, but persist only the one
// status not projected from the authoritative Intent and execution records.
type stepMetadata struct {
	Repair     *artifactcheck.Spec `json:"repair,omitempty"`
	ID         string              `json:"id"`
	GoalID     string              `json:"goal_id"`
	Priority   int                 `json:"priority"`
	Reason     string              `json:"reason,omitempty"`
	Status     string              `json:"status,omitempty"`
	DisputeID  string              `json:"dispute_id,omitempty"`
	DependsOn  []string            `json:"depends_on,omitempty"`
	WritePaths []string            `json:"write_paths,omitempty"`
}

func stepMetadataFrom(step Step) stepMetadata {
	metadata := stepMetadata{ID: step.ID, GoalID: step.GoalID, Priority: step.Priority, Reason: step.Reason, DisputeID: step.DisputeID, DependsOn: append([]string(nil), step.DependsOn...), WritePaths: append([]string(nil), step.WritePaths...), Repair: step.Repair}
	if step.Status == "abandoned" {
		metadata.Status = "abandoned"
	}
	return metadata
}

func (t *Tx) stateData(project string) (stateData, int64, int64, error) {
	d := stateData{Goals: []Goal{}, Steps: []stepMetadata{}, Facts: []FactRecord{}, Findings: []Finding{}, FactRelations: []FactRelation{}}
	var raw string
	var revision, decision int64
	err := t.QueryRow("SELECT data,revision,decision_revision FROM xloom_state WHERE project_id=?", project).Scan(&raw, &revision, &decision)
	if errors.Is(err, sql.ErrNoRows) {
		return d, 0, 0, nil
	}
	if err == nil {
		err = json.Unmarshal([]byte(raw), &d)
		for n := range d.Steps {
			if d.Steps[n].Status != "abandoned" {
				d.Steps[n].Status = ""
			}
		}
	}
	return d, revision, decision, err
}

func (t *Tx) State(project string) (State, error) {
	g, err := t.Load(project)
	if err != nil {
		return State{}, err
	}
	d, revision, decision, err := t.stateData(project)
	if err != nil {
		return State{}, err
	}
	return t.projectState(g, d, revision, decision)
}

// projectState builds the same effective evidence and dependency view for
// persisted reads and a curator's pending atomic batch. Relations never
// rewrite the original observations stored in d.Facts.
func (t *Tx) projectState(g Graph, d stateData, revision, decision int64) (State, error) {
	project := g.Project.ID
	s := State{Graph: g, Goals: []Goal{}, Steps: []Step{}, FactRecords: []FactRecord{}, Findings: d.Findings, FactRelations: d.FactRelations, Revision: revision, DecisionRevision: decision, Candidates: d.Candidates, Disputes: d.Disputes, Curation: d.Curation}
	root := Goal{ID: "goal", Status: "open", Sources: []string{}, CreatedAt: g.Project.CreatedAt}
	facts := make(map[string]FactRecord, len(d.Facts))
	for _, record := range d.Facts {
		if _, exists := facts[record.ID]; !exists {
			facts[record.ID] = record
		}
	}
	statuses := make(map[string]string, len(d.FactRelations))
	for _, relation := range d.FactRelations {
		statuses[relation.Target] = map[string]string{"supersedes": "superseded", "refutes": "refuted", "narrows": "narrowed"}[relation.Kind]
	}
	for _, f := range g.Facts {
		if f.ID == "goal" {
			root.Condition = f.Description
		}
		record := FactRecord{ID: f.ID, Description: f.Description, Status: "valid", Evidence: []EvidenceRef{}, Legacy: true}
		if f.ID == "origin" || f.ID == "goal" {
			record.Status = "input"
		}
		if existing, ok := facts[f.ID]; ok {
			record = existing
		}
		if status, ok := statuses[f.ID]; ok {
			record.Status = status
		}
		s.FactRecords = append(s.FactRecords, record)
	}
	if g.Project.Status == "completed" {
		root.Status = "achieved"
		for _, i := range g.Intents {
			if Value(i.To) == "goal" {
				root.Sources = i.From
			}
		}
	}
	s.Goals = append(s.Goals, root)
	s.Goals = append(s.Goals, d.Goals...)
	latest := map[string]Execution{}
	currentRuns := map[string]bool{}
	// A graph read needs only the latest runtime state of each existing Step.
	// Never materialize archived Job/Result bodies to build the current FGS.
	rows, err := t.Query(`SELECT e.id,e.intent,e.lease,e.status,e.generation FROM intents i JOIN xloom_executions e ON e.rowid=(
		SELECT rowid FROM xloom_executions WHERE project_id=i.project_id AND intent=i.id AND kind!='reason' ORDER BY rowid DESC LIMIT 1
	) WHERE i.project_id=?`, project)
	if err != nil {
		return State{}, err
	}
	for rows.Next() {
		var execution Execution
		var generation int64
		err := rows.Scan(&execution.ID, &execution.Intent, &execution.Lease, &execution.Status, &generation)
		if err != nil {
			rows.Close()
			return State{}, err
		}
		latest[execution.Intent] = execution
		currentRuns[execution.Intent] = generation == g.Project.Generation
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return State{}, err
	}
	metadataByID := make(map[string]stepMetadata, len(d.Steps))
	for _, metadata := range d.Steps {
		// Preserve historical duplicate records: the last metadata wins, while
		// any retained abandonment still overrides the runtime projection.
		if metadataByID[metadata.ID].Status == "abandoned" {
			metadata.Status = "abandoned"
		}
		metadataByID[metadata.ID] = metadata
	}
	for _, i := range g.Intents {
		step := Step{ID: i.ID, From: i.From, GoalID: "goal", Description: i.Description, Status: "open", Result: i.To, Worker: i.Worker, CreatedAt: i.CreatedAt}
		if i.Worker != nil {
			step.Status = "running"
		}
		if i.To != nil {
			step.Status = "completed"
		}
		if metadata, ok := metadataByID[i.ID]; ok {
			step.GoalID, step.Priority, step.Reason = metadata.GoalID, metadata.Priority, metadata.Reason
			step.DisputeID = metadata.DisputeID
			step.Repair = metadata.Repair
			step.DependsOn = append([]string(nil), metadata.DependsOn...)
			step.WritePaths = append([]string(nil), metadata.WritePaths...)
			if metadata.Status == "abandoned" {
				step.Status = "abandoned"
			}
		}
		if execution, ok := latest[i.ID]; ok && i.To == nil && step.Status != "abandoned" && slices.Contains([]string{"failed", "rejected", "cancelled"}, execution.Status) {
			step.Status = "failed"
			// A terminal delivery error cannot overwrite a pending result. Its
			// diagnostic lives in the failure event instead of that result blob.
			var detail string
			err := t.QueryRow("SELECT json_extract(event,'$.payload.reason') FROM xloom_state_events WHERE project_id=? AND json_extract(event,'$.op')='execution_failed' AND json_extract(event,'$.run_id')=? ORDER BY revision DESC LIMIT 1", project, execution.Lease).Scan(&detail)
			if err == nil && detail != "" {
				step.Reason = detail
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return State{}, err
			} else {
				// Older runs predate failure events. Project only their bounded
				// failure fields, not the saved input or complete model response.
				step.Reason, err = t.legacyExecutionFailure(project, execution)
				if err != nil {
					return State{}, err
				}
			}
		}
		if execution, ok := latest[i.ID]; ok && g.Project.OrchestrationVersion == 1 && slices.Contains([]string{"failed", "rejected", "cancelled", "retry_requested"}, execution.Status) {
			step.LatestRunID = execution.ID
		}
		if g.Project.OrchestrationVersion != 1 {
			for _, id := range step.From {
				if s.ValidateFactSources([]string{id}, false) != nil {
					step.InvalidSources = append(step.InvalidSources, id)
				}
			}
			if step.Status == "open" && len(step.InvalidSources) > 0 {
				step.Status = "needs_review"
			}
		}
		s.Steps = append(s.Steps, step)
	}
	if g.Project.OrchestrationVersion == 1 {
		s.projectStepSupport(latest, currentRuns)
	}
	// Assess conclusions once, after Step dependencies have established which
	// accepted result Facts still have valid support.
	for n := range s.Goals {
		s.Goals[n].SupportValid = s.Goals[n].Status == "achieved" && s.ValidateFactSources(s.Goals[n].Sources, true) == nil
	}
	for n := range s.Findings {
		s.Findings[n].SupportValid = len(s.Findings[n].Sources) > 0 && s.ValidateFactSources(s.Findings[n].Sources, true) == nil
	}
	if err := t.projectAssets(&s); err != nil {
		return State{}, err
	}
	return s, nil
}

func (t *Tx) legacyExecutionFailure(project string, execution Execution) (string, error) {
	var raw []byte
	err := t.QueryRow(`WITH failure AS (
		SELECT CASE WHEN json_valid(result) THEN result ELSE '{}' END AS body,
		char(9,10,11,12,13,32,133,160,5760,8192,8193,8194,8195,8196,8197,8198,8199,8200,8201,8202,8232,8233,8239,8287,12288) AS whitespace
		FROM xloom_executions WHERE project_id=? AND id=?)
		SELECT json_object(
		'error',substr(trim(json_extract(body,'$.error'),whitespace),1,2048),
		'failure_kind',substr(json_extract(body,'$.failure_kind'),1,2048),
		'text',json_object('reason',substr(trim(json_extract(CASE WHEN json_valid(json_extract(body,'$.text')) THEN json_extract(body,'$.text') ELSE '{}' END,'$.reason'),whitespace),1,2048)))
		FROM failure`, project, execution.ID).Scan(&raw)
	if err != nil {
		return "", err
	}
	// The legacy result's text field is a JSON string, not an embedded object.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", err
	}
	fields["text"], _ = json.Marshal(string(fields["text"]))
	raw, err = json.Marshal(fields)
	return executionFailureDescription(execution.Status, raw), err
}

func executionFailureDescription(status string, raw json.RawMessage) string {
	var result struct {
		Error       string `json:"error"`
		FailureKind string `json:"failure_kind"`
		Text        string `json:"text"`
	}
	_ = json.Unmarshal(raw, &result)
	description := strings.TrimSpace(result.Error)
	if description == "" && status == "rejected" {
		var declined struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal([]byte(result.Text), &declined)
		description = strings.TrimSpace(declined.Reason)
	}
	if result.FailureKind != "" {
		description = result.FailureKind + ": " + description
	}
	if description == "" {
		description = "execution " + status
	}
	if len(description) > 2048 {
		description = strings.ToValidUTF8(description[:2048], "�") + "…"
	}
	return description
}

func (t *Tx) recordExecutionFailure(e Execution, status string, failure json.RawMessage) error {
	d, _, _, err := t.stateData(e.ProjectID)
	if err != nil {
		return err
	}
	triggerDecision := true
	for _, step := range d.Steps {
		if step.ID == e.Intent && step.Status == "abandoned" {
			// This terminal event merely acknowledges Decide's own explicit
			// cancellation. It must not trigger another decision about itself.
			triggerDecision = false
		}
	}
	revision, err := t.advanceStateRevision(e.ProjectID, triggerDecision)
	if err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{"run_id": e.ID, "status": status, "reason": executionFailureDescription(status, failure), "result": failure})
	event, _ := json.Marshal(StateEvent{Revision: revision, Op: "execution_failed", ID: e.Intent, RunID: e.Lease, CreatedAt: t.Now, Payload: detail, Result: detail})
	_, err = t.Exec("INSERT INTO xloom_state_events(project_id,revision,event) VALUES(?,?,?)", e.ProjectID, revision, string(event))
	return err
}

// Event-only changes preserve metadata byte-for-byte, including unknown fields
// retained by older databases. The surrounding transaction owns the event too.
func (t *Tx) advanceStateRevision(project string, decisionChange bool) (int64, error) {
	increment := 0
	if decisionChange {
		increment = 1
	}
	var revision int64
	err := t.QueryRow(`INSERT INTO xloom_state(project_id,data,revision,decision_revision)
		VALUES(?,'{"goals":[],"steps":[],"facts":[],"findings":[],"fact_relations":[]}',1,?)
		ON CONFLICT(project_id) DO UPDATE SET revision=revision+1,decision_revision=decision_revision+excluded.decision_revision
		RETURNING revision`, project, increment).Scan(&revision)
	return revision, err
}

func (s State) ValidateFactSources(ids []string, requireObservation bool) error {
	if len(ids) == 0 {
		return Err(422, "at least one source fact is required")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return Err(422, "duplicate source fact")
		}
		seen[id] = true
		found := false
		for _, f := range s.FactRecords {
			if f.ID != id {
				continue
			}
			found = true
			if f.SupportInvalid || (f.Status != "valid" && !(id == "origin" && !requireObservation)) {
				return Err(409, "Source fact "+id+" is not effective evidence")
			}
		}
		if !found {
			return Err(404, "Fact "+id+" not found")
		}
	}
	return nil
}

func decodeAction(raw json.RawMessage, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return Err(422, "Invalid action payload: "+err.Error())
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Err(422, "Action payload must contain one JSON object")
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return Err(422, "Action payload must be an object")
	}
	return nil
}

func required(value string, max int) bool { return strings.TrimSpace(value) != "" && len(value) <= max }
func runMatches(identity, id string) bool {
	return identity == id || (id != "" && strings.HasSuffix(identity, "@"+id))
}
func validateEvidence(refs []EvidenceRef, run string, allowed []EvidenceRef, requiredRefs bool) error {
	if requiredRefs && len(refs) == 0 {
		return Err(422, "evidence must include an execution, path and necessary excerpt")
	}
	if len(refs) > 32 {
		return Err(422, "at most 32 evidence references are allowed")
	}
	for _, ref := range refs {
		if !required(ref.RunID, 256) || !required(ref.Path, 4096) || !required(ref.Excerpt, 8192) || strings.ContainsAny(ref.Path, "\x00\r\n") || ref.StartLine < 0 || ref.EndLine < ref.StartLine || (ref.StartLine == 0 && ref.EndLine != 0) {
			return Err(422, "invalid evidence reference")
		}
		if path.Clean(ref.Path) == "." || strings.Contains(ref.Path, "://") {
			return Err(422, "evidence path must locate a retained execution artifact")
		}
		if !runMatches(run, ref.RunID) && !slices.Contains(allowed, ref) {
			return Err(403, "evidence must belong to this execution or a cited source fact")
		}
	}
	return nil
}

func (t *Tx) StateAction(project string, fence ExecutionFence, action StateAction) (out StateActionResult, err error) {
	if !t.inDecisionBatch {
		if err := t.CheckDirectDecisionWrite(project, fence); err != nil {
			return StateActionResult{}, err
		}
	}
	// The record, its asset anchors and the receipt form one write. Preserve
	// this boundary even when an outer caller handles a failed action itself.
	if _, err = t.Exec("SAVEPOINT xloom_state_action"); err != nil {
		return out, err
	}
	defer func() {
		if err != nil {
			_, rollbackErr := t.Exec("ROLLBACK TO xloom_state_action")
			err = errors.Join(err, rollbackErr)
		}
		_, releaseErr := t.Exec("RELEASE xloom_state_action")
		err = errors.Join(err, releaseErr)
	}()
	if action.Op == "curate" {
		if receipt, found, replayErr := t.replayCuration(project, fence, action); found || replayErr != nil {
			return receipt, replayErr
		}
	}
	s, err := t.State(project)
	if err != nil {
		return StateActionResult{}, err
	}
	out, err = t.stateAction(&s, fence, action)
	if err == nil && action.Op == "curate" {
		err = t.CompleteCurationExecution(project, fence.Run)
	}
	return out, err
}

// A batch owns this snapshot for one transaction and replaces it only after a
// successful action. Each next action sees the persisted result of its parent.
func (t *Tx) stateAction(snapshot *State, fence ExecutionFence, action StateAction) (StateActionResult, error) {
	s := *snapshot
	project := s.Graph.Project.ID
	var err error
	if err = s.Graph.RequireActive(); err != nil {
		return StateActionResult{}, err
	}
	if fence.Run == "" || !slices.Contains([]string{"reason", "explore", "bootstrap", "intent", "curate"}, fence.Lease) {
		return StateActionResult{}, Err(403, "state actions require an execution lease")
	}
	if err = t.CheckExecution(s.Graph, fence); err != nil {
		return StateActionResult{}, err
	}
	if !required(action.IdempotencyKey, 256) {
		return StateActionResult{}, Err(422, "idempotency_key is required and must be at most 256 bytes")
	}
	if !slices.Contains([]string{"fact", "fact_relation", "finding", "goal", "step", "candidate", "curate", "curation_request"}, action.Op) && !(t.inDecisionBatch && action.Op == "complete") {
		return StateActionResult{}, Err(422, "unknown state action")
	}
	if s.Graph.Project.OrchestrationVersion == 1 {
		allowed := false
		switch fence.Lease {
		case "reason":
			allowed = action.Op == "goal" || action.Op == "step" || action.Op == "complete" || action.Op == "curation_request"
		case "curate":
			allowed = action.Op == "curate"
		case "explore", "bootstrap", "intent":
			allowed = action.Op == "fact" || action.Op == "candidate"
		}
		if !allowed {
			return StateActionResult{}, Err(403, "operation is not permitted for this orchestration role")
		}
		if fence.Lease == "curate" && action.ExpectedVersion == "" {
			return StateActionResult{}, Err(422, "curation writes require expected_version")
		}
	} else if action.Op == "candidate" || action.Op == "curate" || action.Op == "curation_request" || fence.Lease == "curate" {
		return StateActionResult{}, Err(403, "operation requires orchestration version 1")
	}
	if (action.Op == "goal" || action.Op == "step") && fence.Lease != "reason" {
		return StateActionResult{}, Err(403, "only Decide can modify goals or steps")
	}
	if (action.Op == "fact" || action.Op == "finding") && fence.Lease == "reason" {
		return StateActionResult{}, Err(403, "Decide cannot turn planning into observed evidence")
	}
	canonical, err := canonicalStateAction(fence, action)
	if err != nil {
		return StateActionResult{}, err
	}
	var oldRequest, oldResponse string
	err = t.QueryRow("SELECT request,response FROM xloom_state_actions WHERE project_id=? AND idempotency_key=?", project, action.IdempotencyKey).Scan(&oldRequest, &oldResponse)
	if err == nil {
		if oldRequest != string(canonical) {
			return StateActionResult{}, Err(409, "idempotency key was used for a different action")
		}
		var result StateActionResult
		err = json.Unmarshal([]byte(oldResponse), &result)
		return result, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return StateActionResult{}, err
	}
	if action.Op == "curate" {
		var committed bool
		if err = t.QueryRow(`SELECT EXISTS(SELECT 1 FROM xloom_state_actions WHERE project_id=? AND json_extract(request,'$.Run')=? AND json_extract(request,'$.Op')='curate')`, project, fence.Run).Scan(&committed); err != nil {
			return StateActionResult{}, err
		}
		if committed {
			return StateActionResult{}, Err(409, "this curator execution already committed a batch")
		}
	}
	// The batch already checked its caller's version under this transaction's
	// write reservation. Its own successive versions need no Job revalidation.
	if !t.inDecisionBatch {
		if err = t.CheckDecisionStateVersion(s, fence, action.ExpectedVersion); err != nil {
			return StateActionResult{}, err
		}
	}
	d, _, _, err := t.stateData(project)
	if err != nil {
		return StateActionResult{}, err
	}
	var id string
	var result any
	changed := true
	switch action.Op {
	case "fact":
		id, result, err = t.addStateFact(&s, &d, fence, action.Payload)
	case "fact_relation":
		id, result, changed, err = t.addFactRelation(s, &d, fence, action.Payload)
	case "finding":
		id, result, changed, err = t.upsertFinding(s, &d, fence, action.Payload)
	case "candidate":
		id, result, err = t.addCandidate(s, &d, fence, action.Payload)
	case "curate":
		id, result, err = t.curate(s, &d, fence, action.Payload)
	case "curation_request":
		id, result, changed, err = t.requestCuration(s, &d, action.Payload)
	case "goal":
		id, result, changed, err = t.changeGoal(s, &d, action.Payload)
	case "step":
		id, result, changed, err = t.changeStep(&s, &d, fence, action.Payload)
	case "complete":
		var input struct {
			From        []string `json:"from"`
			Description string   `json:"description"`
		}
		if err = decodeAction(action.Payload, &input); err == nil {
			var completed Intent
			completed, err = t.CompleteProject(project, fence, input.From, input.Description)
			id, result = completed.ID, completed
		}
	}
	if err != nil {
		return StateActionResult{}, err
	}
	if changed {
		s.Revision++
		if action.Op == "fact" || action.Op == "finding" || action.Op == "candidate" || action.Op == "curate" || (action.Op == "fact_relation" && fence.Lease != "reason") {
			s.DecisionRevision++
		}
		raw, err := json.Marshal(d)
		if err != nil {
			return StateActionResult{}, err
		}
		if _, err = t.Exec("INSERT INTO xloom_state(project_id,data,revision,decision_revision) VALUES(?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET data=excluded.data,revision=excluded.revision,decision_revision=excluded.decision_revision", project, string(raw), s.Revision, s.DecisionRevision); err != nil {
			return StateActionResult{}, err
		}
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return StateActionResult{}, err
	}
	current, err := t.State(project)
	if err != nil {
		return StateActionResult{}, err
	}
	if changed && (action.Op == "goal" || action.Op == "step" || action.Op == "curation_request") {
		var transition struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(action.Payload, &transition)
		// Releasing old work must remain possible even for pre-admission data
		// whose requirements were already over budget. Terminal transitions do
		// not introduce a new runnable Step or increase its mandatory ancestry.
		if transition.Action != "abandon" && transition.Action != "withdraw" && transition.Action != "achieve" {
			if err = ValidateContextCapacity(current); err != nil {
				return StateActionResult{}, err
			}
		}
	}
	out := StateActionResult{Op: action.Op, ID: id, Revision: s.Revision, Result: resultJSON, StateVersion: DecisionStateVersion(current), Unchanged: !changed}
	out.AssetIDs = current.AssetIDs(action.Op, id)
	out.Committed = action.Op == "curate"
	response, _ := json.Marshal(out)
	if _, err = t.Exec("INSERT INTO xloom_state_actions(project_id,idempotency_key,request,response) VALUES(?,?,?,?)", project, action.IdempotencyKey, string(canonical), string(response)); err != nil {
		return StateActionResult{}, err
	}
	if !changed {
		*snapshot = current
		return out, nil
	}
	event, _ := json.Marshal(StateEvent{Revision: s.Revision, Op: action.Op, ID: id, RunID: fence.Run, CreatedAt: t.Now, Payload: action.Payload, Result: resultJSON})
	_, err = t.Exec("INSERT INTO xloom_state_events(project_id,revision,event) VALUES(?,?,?)", project, s.Revision, string(event))
	if err == nil {
		*snapshot = current
	}
	return out, err
}

func (t *Tx) addStateFact(s *State, d *stateData, fence ExecutionFence, raw json.RawMessage) (string, any, error) {
	var input struct {
		Description string        `json:"description"`
		Scope       string        `json:"scope"`
		ObservedAt  string        `json:"observed_at"`
		Evidence    []EvidenceRef `json:"evidence"`
		Assets      []AssetSpec   `json:"assets"`
	}
	if err := decodeAction(raw, &input); err != nil {
		return "", nil, err
	}
	if !required(input.Description, 16384) || !required(input.Scope, 4096) {
		return "", nil, Err(422, "fact description and scope are required")
	}
	observed, err := time.Parse(time.RFC3339, input.ObservedAt)
	if err != nil {
		return "", nil, Err(422, "observed_at must be RFC3339")
	}
	now, _ := time.Parse(time.RFC3339, t.Now)
	if observed.After(now.Add(time.Minute)) {
		return "", nil, Err(422, "observation cannot be in the future")
	}
	if err = validateEvidence(input.Evidence, fence.Run, nil, true); err != nil {
		return "", nil, err
	}
	assets, err := normalizeActionAssets(input.Assets)
	if err != nil {
		return "", nil, err
	}
	id, err := t.Next(s.Graph.Project.ID, "fact")
	if err != nil {
		return "", nil, err
	}
	f := FactRecord{ID: id, Description: strings.TrimSpace(input.Description), Scope: strings.TrimSpace(input.Scope), ObservedAt: observed.UTC().Format(time.RFC3339Nano), Evidence: input.Evidence, Status: "valid", RunID: fence.Run, SourceStepID: fence.Intent}
	// The duplicated base fields are required by rollback readers. Accept a
	// new record only when its immutable SQL Fact was inserted from this same
	// value; a stale allocation counter must not create two observations for
	// one ID. Limit conflict handling to the primary key, not other failures.
	inserted, err := t.Exec("INSERT INTO facts(id,project_id,description) VALUES(?,?,?) ON CONFLICT(id,project_id) DO NOTHING", id, s.Graph.Project.ID, f.Description)
	if err != nil {
		return "", nil, err
	}
	count, err := inserted.RowsAffected()
	if err != nil {
		return "", nil, err
	}
	if count != 1 {
		return "", nil, Err(409, "Fact ID "+id+" already exists; its allocation counter is inconsistent")
	}
	d.Facts = append(d.Facts, f)
	s.Graph.Facts = append(s.Graph.Facts, Fact{ID: id, Description: f.Description})
	err = t.addAssetAnchors(s.Graph.Project.ID, s.Graph.Project.Generation, "fact", id, assets)
	return id, f, err
}

func (t *Tx) addFactRelation(s State, d *stateData, fence ExecutionFence, raw json.RawMessage) (string, any, bool, error) {
	var input struct {
		Kind   string `json:"kind"`
		Source string `json:"source"`
		Target string `json:"target"`
		Reason string `json:"reason"`
	}
	if err := decodeAction(raw, &input); err != nil {
		return "", nil, false, err
	}
	if !slices.Contains([]string{"supersedes", "refutes", "narrows"}, input.Kind) || !required(input.Reason, 8192) || input.Source == input.Target {
		return "", nil, false, Err(422, "invalid fact relation")
	}
	if err := s.ValidateFactSources([]string{input.Source}, true); err != nil {
		return "", nil, false, err
	}
	if err := s.Graph.ValidateSources([]string{input.Target}); err != nil {
		return "", nil, false, err
	}
	if input.Target == "origin" {
		return "", nil, false, Err(403, "user input cannot be rewritten as a fact relation")
	}
	for _, relation := range d.FactRelations {
		// Curators publish one semantic edge per kind and pair of facts. A
		// rephrased explanation remains in that batch's request/event history;
		// it must not duplicate the edge or replace its original provenance.
		// Legacy fact_relation clients retain their reason-sensitive identity.
		if relation.Kind == input.Kind && relation.Source == input.Source && relation.Target == input.Target && (s.Graph.Project.OrchestrationVersion == 1 || relation.Reason == input.Reason) {
			return input.Target, relation, false, nil
		}
	}
	// Relations point from old target to its new corrective source. Reject
	// cycles so an observation can never invalidate itself through a chain.
	seen := map[string]bool{}
	var reaches func(string) bool
	reaches = func(id string) bool {
		if id == input.Target {
			return true
		}
		if seen[id] {
			return false
		}
		seen[id] = true
		for _, relation := range d.FactRelations {
			if relation.Target == id && reaches(relation.Source) {
				return true
			}
		}
		return false
	}
	if reaches(input.Source) {
		return "", nil, false, Err(409, "fact relation would create a cycle")
	}
	relation := FactRelation{Kind: input.Kind, Source: input.Source, Target: input.Target, Reason: input.Reason, RunID: fence.Run, CreatedAt: t.Now}
	d.FactRelations = append(d.FactRelations, relation)
	return input.Target, relation, true, nil
}

func (t *Tx) upsertFinding(s State, d *stateData, fence ExecutionFence, raw json.RawMessage) (string, any, bool, error) {
	var input struct {
		Claim          string          `json:"claim"`
		Scope          string          `json:"scope"`
		Status         string          `json:"status"`
		Sources        []string        `json:"sources"`
		Evidence       []EvidenceRef   `json:"evidence"`
		Reason         string          `json:"reason"`
		ReplaceSupport json.RawMessage `json:"replace_support"`
	}
	if err := decodeAction(raw, &input); err != nil {
		return "", nil, false, err
	}
	if !required(input.Claim, 16384) || !required(input.Scope, 4096) || !slices.Contains([]string{"candidate", "verified", "refuted"}, input.Status) || len(input.Reason) > 8192 {
		return "", nil, false, Err(422, "finding requires claim, scope and candidate/verified/refuted status")
	}
	replaceSupport := false
	switch string(bytes.TrimSpace(input.ReplaceSupport)) {
	case "", "false":
	case "true":
		replaceSupport = true
	default:
		return "", nil, false, Err(422, "replace_support must be a boolean")
	}
	if replaceSupport && !required(input.Reason, 8192) {
		return "", nil, false, Err(422, "replacing finding support requires a reason")
	}
	if len(input.Sources) > 0 || input.Status != "candidate" || replaceSupport {
		if err := s.ValidateFactSources(input.Sources, true); err != nil {
			return "", nil, false, err
		}
	}
	allowed := []EvidenceRef{}
	for _, f := range s.FactRecords {
		if slices.Contains(input.Sources, f.ID) {
			allowed = append(allowed, f.Evidence...)
		}
	}
	if err := validateEvidence(input.Evidence, fence.Run, allowed, false); err != nil {
		return "", nil, false, err
	}
	if input.Status != "candidate" && len(allowed)+len(input.Evidence) == 0 {
		return "", nil, false, Err(422, "verified or refuted findings require retained evidence excerpts")
	}
	claim, scope := strings.Join(strings.Fields(input.Claim), " "), strings.TrimSpace(input.Scope)
	key, _ := json.Marshal([]string{claim, scope})
	digest := sha256.Sum256(key)
	id := "finding_" + hex.EncodeToString(digest[:12])
	finding := Finding{ID: id, Claim: claim, Scope: scope, Status: input.Status, Sources: []string{}, Evidence: []EvidenceRef{}, CreatedAt: t.Now, UpdatedAt: t.Now, Reason: input.Reason}
	index := -1
	for n, old := range d.Findings {
		if old.ID == id {
			finding.CreatedAt = old.CreatedAt
			if !replaceSupport {
				finding.Sources, finding.Evidence = append([]string{}, old.Sources...), append([]EvidenceRef{}, old.Evidence...)
			}
			index = n
			break
		}
	}
	if replaceSupport && index < 0 {
		return "", nil, false, Err(409, "support replacement requires an existing finding")
	}
	for _, source := range input.Sources {
		if !slices.Contains(finding.Sources, source) {
			finding.Sources = append(finding.Sources, source)
		}
	}
	for _, ref := range input.Evidence {
		if !slices.Contains(finding.Evidence, ref) {
			finding.Evidence = append(finding.Evidence, ref)
		}
	}
	finding.SupportValid = len(finding.Sources) > 0 && s.ValidateFactSources(finding.Sources, true) == nil
	if index >= 0 {
		old := d.Findings[index]
		if old.Status == finding.Status && old.Reason == finding.Reason && sameSupportSet(old.Sources, finding.Sources) && sameSupportSet(old.Evidence, finding.Evidence) {
			old.SupportValid = finding.SupportValid
			return id, old, false, nil
		}
		// Explicit replacement updates current support only. Earlier immutable
		// action events retain the previous sources and evidence references.
		d.Findings[index] = finding
	} else {
		d.Findings = append(d.Findings, finding)
	}
	return id, finding, true, nil
}

func sameSupportSet[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for _, item := range a {
		if !slices.Contains(b, item) {
			return false
		}
	}
	return true
}

func (t *Tx) stateID(project, kind, prefix string) (string, error) {
	if _, err := t.Exec("INSERT OR IGNORE INTO scoped_counters(project_id,kind,value) VALUES(?,?,0)", project, kind); err != nil {
		return "", err
	}
	var n int
	err := t.QueryRow("UPDATE scoped_counters SET value=value+1 WHERE project_id=? AND kind=? RETURNING value", project, kind).Scan(&n)
	return fmt.Sprintf("%s%03d", prefix, n), err
}

func (t *Tx) changeGoal(s State, d *stateData, raw json.RawMessage) (string, any, bool, error) {
	var input struct {
		Action    string   `json:"action"`
		ID        string   `json:"id"`
		ParentID  string   `json:"parent_id"`
		Condition string   `json:"condition"`
		Reason    string   `json:"reason"`
		Sources   []string `json:"sources"`
	}
	if err := decodeAction(raw, &input); err != nil {
		return "", nil, false, err
	}
	if input.ID == "goal" {
		return "", nil, false, Err(403, "root goal is user-owned; completion uses the project completion contract")
	}
	if input.Action == "add" {
		if input.ID != "" || !required(input.Condition, 16384) {
			return "", nil, false, Err(422, "new goal requires condition and a server-assigned ID")
		}
		if input.ParentID == "" {
			input.ParentID = "goal"
		}
		parent := false
		for _, g := range s.Goals {
			parent = parent || (g.ID == input.ParentID && g.Status == "open")
		}
		if !parent {
			return "", nil, false, Err(409, "parent goal is not open")
		}
		for _, goal := range s.Goals {
			if goal.ParentID == input.ParentID && goal.Condition == strings.TrimSpace(input.Condition) {
				return goal.ID, goal, false, nil
			}
		}
		id, err := t.stateID(s.Graph.Project.ID, "goal", "g")
		goal := Goal{ID: id, ParentID: input.ParentID, Condition: strings.TrimSpace(input.Condition), Status: "open", Sources: []string{}, CreatedAt: t.Now}
		if err == nil {
			d.Goals = append(d.Goals, goal)
		}
		return id, goal, true, err
	}
	if !slices.Contains([]string{"withdraw", "achieve"}, input.Action) || !required(input.Reason, 8192) || input.Condition != "" || input.ParentID != "" {
		return "", nil, false, Err(422, "goal transition requires a reason and cannot rewrite its condition or parent")
	}
	for n := range d.Goals {
		goal := &d.Goals[n]
		if goal.ID != input.ID {
			continue
		}
		canRevalidate := false
		for _, current := range s.Goals {
			canRevalidate = canRevalidate || (current.ID == goal.ID && current.Status == "achieved" && !current.SupportValid)
		}
		if goal.Status != "open" && !canRevalidate {
			return "", nil, false, Err(409, "goal is no longer open")
		}
		for _, child := range d.Goals {
			if child.ParentID == goal.ID && child.Status == "open" {
				return "", nil, false, Err(409, "goal has an open child; resolve it explicitly first")
			}
		}
		for _, step := range s.Steps {
			if step.GoalID == goal.ID && contextActiveStep(step.Status) {
				return "", nil, false, Err(409, "goal has an active step; resolve it explicitly first")
			}
		}
		if input.Action == "achieve" {
			if err := s.ValidateFactSources(input.Sources, true); err != nil {
				return "", nil, false, err
			}
			goal.Status, goal.Sources = "achieved", input.Sources
		} else {
			goal.Status = "withdrawn"
		}
		goal.Reason = input.Reason
		return goal.ID, *goal, true, nil
	}
	return "", nil, false, Err(404, "Goal not found")
}

func (t *Tx) changeStep(s *State, d *stateData, fence ExecutionFence, raw json.RawMessage) (string, any, bool, error) {
	var input struct {
		Action      string              `json:"action"`
		ID          string              `json:"id"`
		GoalID      string              `json:"goal_id"`
		From        []string            `json:"from"`
		Description string              `json:"description"`
		Priority    int                 `json:"priority"`
		Reason      string              `json:"reason"`
		DisputeID   string              `json:"dispute_id"`
		DependsOn   []string            `json:"depends_on"`
		WritePaths  []string            `json:"write_paths"`
		LatestRunID string              `json:"latest_run_id"`
		Repair      *artifactcheck.Spec `json:"repair"`
		Assets      []AssetSpec         `json:"assets"`
	}
	if err := decodeAction(raw, &input); err != nil {
		return "", nil, false, err
	}
	if input.Priority < 0 || input.Priority > 1000000 {
		return "", nil, false, Err(422, "step priority must be between 0 and 1000000")
	}
	if input.Action == "add" {
		assets, err := normalizeActionAssets(input.Assets)
		if err != nil {
			return "", nil, false, err
		}
		if input.Repair != nil {
			if s.Graph.Project.OrchestrationVersion != 1 || input.DisputeID != "" {
				return "", nil, false, Err(422, "repair requires orchestration version 1 and cannot be an independent dispute review")
			}
			if err := artifactcheck.Validate(*input.Repair); err != nil {
				return "", nil, false, Err(422, err.Error())
			}
		}
		if input.ID != "" || input.LatestRunID != "" || !required(input.Description, 16384) {
			return "", nil, false, Err(422, "new step requires description and a server-assigned ID")
		}
		if err := s.ValidateFactSources(input.From, false); err != nil {
			return "", nil, false, err
		}
		if err := validateStepDependencies(*s, input.DependsOn); err != nil {
			return "", nil, false, err
		}
		writePaths, err := normalizeStepWritePaths(input.WritePaths, input.Repair)
		if err != nil {
			return "", nil, false, err
		}
		if input.GoalID == "" {
			input.GoalID = "goal"
		}
		goalOpen := false
		for _, goal := range s.Goals {
			goalOpen = goalOpen || (goal.ID == input.GoalID && goal.Status == "open")
		}
		if !goalOpen {
			return "", nil, false, Err(409, "step requires an open goal")
		}
		if input.DisputeID != "" {
			if err := t.validateReviewStep(*s, d, input.DisputeID); err != nil {
				return "", nil, false, err
			}
		}
		if existing, ok := s.matchingRepairStep(input.GoalID, input.From, input.Description, input.DependsOn, input.Repair, writePaths, assetIDs(assets)); ok {
			if existing.DisputeID != input.DisputeID {
				return "", nil, false, Err(409, "matching task has a different dispute binding")
			}
			return existing.ID, existing, false, nil
		}
		if err := t.CheckNewStepLimit(s.Graph.Project.ID, fence.Run); err != nil {
			return "", nil, false, err
		}
		id, err := t.Next(s.Graph.Project.ID, "intent")
		if err != nil {
			return "", nil, false, err
		}
		step := Step{ID: id, From: input.From, GoalID: input.GoalID, Description: strings.TrimSpace(input.Description), Status: "open", Priority: input.Priority, CreatedAt: t.Now, DisputeID: input.DisputeID, DependsOn: append([]string(nil), input.DependsOn...), WritePaths: writePaths, Repair: input.Repair}
		if input.DisputeID != "" {
			for n := range d.Disputes {
				if d.Disputes[n].ID == input.DisputeID {
					d.Disputes[n].ReviewStepIDs = append(d.Disputes[n].ReviewStepIDs, id)
					d.Disputes[n].Status = "pending_review"
					d.Disputes[n].UpdatedAt = t.Now
				}
			}
		}
		d.Steps = append(d.Steps, stepMetadataFrom(step))
		intent := Intent{ID: id, From: input.From, Description: step.Description, Creator: fence.Run, CreatedAt: t.Now}
		s.Graph.Intents = append(s.Graph.Intents, intent)
		err = t.saveIntent(s.Graph.Project.ID, intent)
		if err == nil {
			err = t.addAssetAnchors(s.Graph.Project.ID, s.Graph.Project.Generation, "step", id, assets)
		}
		return id, step, true, err
	}
	if input.Assets != nil {
		return "", nil, false, Err(422, "assets is an immutable step add contract")
	}
	if input.Repair != nil {
		return "", nil, false, Err(422, "repair is an immutable step add contract")
	}
	if input.WritePaths != nil {
		return "", nil, false, Err(422, "write_paths is an immutable step add contract")
	}
	if input.Action == "retry" {
		if !required(input.ID, 256) || !ValidExecutionID(input.LatestRunID) || !required(input.Reason, 8192) || input.GoalID != "" || len(input.From) != 0 || input.Description != "" || input.DisputeID != "" || input.DependsOn != nil || input.Priority != 0 {
			return "", nil, false, Err(422, "step retry requires only id,latest_run_id and reason; task inputs are immutable")
		}
		return t.retryStep(*s, d, input.ID, input.LatestRunID, input.Reason)
	}
	if !slices.Contains([]string{"priority", "abandon"}, input.Action) || !required(input.Reason, 8192) || input.GoalID != "" || len(input.From) != 0 || input.Description != "" || input.DisputeID != "" || input.DependsOn != nil || input.LatestRunID != "" {
		return "", nil, false, Err(422, "step change requires a reason; existing task inputs are immutable")
	}
	for _, current := range s.Steps {
		if current.ID != input.ID {
			continue
		}
		canRetireUnsupported := s.Graph.Project.OrchestrationVersion == 1 && input.Action == "abandon" && current.Status == "completed" && !current.SupportValid && !s.externalFeedbackStep(current)
		if current.Status == "abandoned" || (current.Status == "completed" && !canRetireUnsupported) {
			return "", nil, false, Err(409, "step is already terminal")
		}
		if input.Action == "priority" && current.Status == "running" {
			return "", nil, false, Err(409, "running step inputs cannot be changed")
		}
		// Keep the successful action available for idempotent replay without
		// adding an event when neither the priority nor its rationale changed.
		if input.Action == "priority" && current.Priority == input.Priority && current.Reason == input.Reason {
			return current.ID, current, false, nil
		}
		current.Reason = input.Reason
		if input.Action == "priority" {
			current.Priority = input.Priority
		} else {
			current.Status = "abandoned"
			for n := range s.Graph.Intents {
				i := &s.Graph.Intents[n]
				if i.ID != current.ID {
					continue
				}
				if canRetireUnsupported {
					// Retire its current authority without changing the prior
					// success record, accepted Fact or completion timestamp.
					continue
				}
				if i.Worker != nil {
					if _, err := t.Exec("INSERT OR IGNORE INTO xloom_revoked_runs(project_id,worker) VALUES(?,?)", s.Graph.Project.ID, *i.Worker); err != nil {
						return "", nil, false, err
					}
				}
				i.Worker, current.Worker, i.ConcludedAt = nil, nil, Ptr(t.Now)
				if _, err := t.Exec("UPDATE intents SET worker=NULL,concluded_at=? WHERE project_id=? AND id=?", t.Now, s.Graph.Project.ID, i.ID); err != nil {
					return "", nil, false, err
				}
			}
		}
		found := false
		for n := range d.Steps {
			if d.Steps[n].ID == current.ID {
				d.Steps[n] = stepMetadataFrom(current)
				found = true
			}
		}
		if !found {
			d.Steps = append(d.Steps, stepMetadataFrom(current))
		}
		return current.ID, current, true, nil
	}
	return "", nil, false, Err(404, "Step not found")
}

func (t *Tx) StateEvents(project string, after int64) ([]StateEvent, error) {
	if err := t.RequireProject(project); err != nil {
		return nil, err
	}
	rows, err := t.Query("SELECT event FROM xloom_state_events WHERE project_id=? AND revision>? ORDER BY revision LIMIT 1000", project, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StateEvent{}
	for rows.Next() {
		var raw string
		var event StateEvent
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func (t *Tx) StepAvailable(project, id string) error {
	d, _, _, err := t.stateData(project)
	if err != nil {
		return err
	}
	for _, step := range d.Steps {
		if step.ID == id && step.Status == "abandoned" {
			return Err(409, "Step was abandoned")
		}
	}
	return nil
}

// ValidateStateCompletion adds checks only when extended state is present;
// legacy clients without FGS metadata retain their original contract.
func (t *Tx) ValidateStateCompletion(project string, from []string) error {
	current, loadErr := t.State(project)
	if loadErr != nil {
		return loadErr
	}
	if current.Graph.Project.OrchestrationVersion == 1 {
		for _, step := range current.Steps {
			if step.Status == "blocked" || (step.Status == "completed" && !step.SupportValid && Value(step.Result) != "goal" && !current.externalFeedbackStep(step)) {
				return Err(409, "Project has a blocked or unsupported Step "+step.ID)
			}
		}
		if err := current.validateCompletionDisputes(from); err != nil {
			return err
		}
		if current.Graph.Project.Curator != nil {
			return Err(409, "Project has an active curator")
		}
	}
	d, _, _, err := t.stateData(project)
	if err != nil || (len(d.Facts) == 0 && len(d.Goals) == 0 && len(d.Steps) == 0 && len(d.Findings) == 0 && len(d.FactRelations) == 0) {
		return err
	}
	if err = current.ValidateFactSources(from, true); err != nil {
		return err
	}
	for _, goal := range current.Goals {
		if goal.ID != "goal" && (goal.Status == "open" || (goal.Status == "achieved" && !goal.SupportValid)) {
			return Err(409, "Project has an unresolved child goal")
		}
	}
	return nil
}
