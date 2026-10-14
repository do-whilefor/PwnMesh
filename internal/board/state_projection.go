package board

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

func (t *Tx) stateData(project string) (stateData, int64, int64, error) {
	d := stateData{Goals: []Goal{}, Steps: []stepMetadata{}, Facts: []FactRecord{}, Findings: []Finding{}, FactRelations: []FactRelation{}}
	var raw string
	var revision, decision int64
	err := t.QueryRow("SELECT data,revision,decision_revision FROM pwnmesh_state WHERE project_id=?", project).Scan(&raw, &revision, &decision)
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
	rows, err := t.Query(`SELECT e.id,e.intent,e.lease,e.status,e.generation FROM intents i JOIN pwnmesh_executions e ON e.rowid=(
		SELECT rowid FROM pwnmesh_executions WHERE project_id=i.project_id AND intent=i.id AND kind!='reason' ORDER BY rowid DESC LIMIT 1
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
			err := t.QueryRow("SELECT json_extract(event,'$.payload.reason') FROM pwnmesh_state_events WHERE project_id=? AND json_extract(event,'$.op')='execution_failed' AND json_extract(event,'$.run_id')=? ORDER BY revision DESC LIMIT 1", project, execution.Lease).Scan(&detail)
			if err == nil && detail != "" {
				step.Reason = detail
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return State{}, err
			} else {
				step.Reason = executionFailureDescription(execution.Status, nil)
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
