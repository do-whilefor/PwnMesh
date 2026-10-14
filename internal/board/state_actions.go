package board

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/board/internal/sqlstore"
	"pwnmesh/internal/board/model"
)

// Only the private decision batch path may reuse its initial version check.
// Scope stays local to the call; it cannot change another operation on Tx.
type actionScope uint8

const (
	directStateAction actionScope = iota
	batchedDecisionAction
)

func (t *Tx) StateAction(project string, fence ExecutionFence, action StateAction) (out StateActionResult, err error) {
	if err := t.CheckDirectDecisionWrite(project, fence); err != nil {
		return StateActionResult{}, err
	}
	// The record, its asset anchors and the receipt form one write. Preserve
	// this boundary even when an outer caller handles a failed action itself.
	if _, err = t.Exec("SAVEPOINT pwnmesh_state_action"); err != nil {
		return out, err
	}
	defer func() {
		if err != nil {
			_, rollbackErr := t.Exec("ROLLBACK TO pwnmesh_state_action")
			err = errors.Join(err, rollbackErr)
		}
		_, releaseErr := t.Exec("RELEASE pwnmesh_state_action")
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
	out, err = t.stateAction(&s, fence, action, directStateAction)
	if err == nil && action.Op == "curate" {
		err = t.CompleteCurationExecution(project, fence.Run)
	}
	return out, err
}

// A batch owns this snapshot for one transaction and replaces it only after a
// successful action. Each next action sees the persisted result of its parent.
func (t *Tx) stateAction(snapshot *State, fence ExecutionFence, action StateAction, scope actionScope) (StateActionResult, error) {
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
	if !slices.Contains([]string{"fact", "fact_relation", "finding", "goal", "step", "candidate", "curate", "curation_request"}, action.Op) && !(scope == batchedDecisionAction && action.Op == "complete") {
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
	oldRequest, oldResponse, err := sqlstore.ActionReceipt(t, project, action.IdempotencyKey)
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
		committed, err := sqlstore.HasCurationReceipt(t, project, fence.Run)
		if err != nil {
			return StateActionResult{}, err
		}
		if committed {
			return StateActionResult{}, Err(409, "this curator execution already committed a batch")
		}
	}
	// The batch already checked its caller's version under this transaction's
	// write reservation. Its own successive versions need no Job revalidation.
	if scope != batchedDecisionAction {
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
			completed, err = t.completeProject(project, fence, input.From, input.Description, scope)
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
		if err = sqlstore.SaveStateMetadata(t, project, string(raw), s.Revision, s.DecisionRevision); err != nil {
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
	if err = sqlstore.InsertActionReceipt(t, project, action.IdempotencyKey, string(canonical), string(response)); err != nil {
		return StateActionResult{}, err
	}
	if !changed {
		*snapshot = current
		return out, nil
	}
	event, _ := json.Marshal(StateEvent{Revision: s.Revision, Op: action.Op, ID: id, RunID: fence.Run, CreatedAt: t.Now, Payload: action.Payload, Result: resultJSON})
	err = sqlstore.InsertStateEvent(t, project, s.Revision, string(event))
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
	id := model.FindingIdentity(claim, scope)
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
		if existing, ok := matchingRepairStep(*s, input.GoalID, input.From, input.Description, input.DependsOn, input.Repair, writePaths, assetIDs(assets)); ok {
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
		canRetireUnsupported := s.Graph.Project.OrchestrationVersion == 1 && input.Action == "abandon" && current.Status == "completed" && !current.SupportValid && !s.ExternalFeedbackStep(current)
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
					if _, err := t.Exec("INSERT OR IGNORE INTO pwnmesh_revoked_runs(project_id,worker) VALUES(?,?)", s.Graph.Project.ID, *i.Worker); err != nil {
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
