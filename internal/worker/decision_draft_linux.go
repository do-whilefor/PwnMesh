package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/board"
)

const committedDecisionText = `{"accepted":true,"data":{"decided":true}}`

// The draft is intentionally process-local. Its only durable authority is the
// server receipt; transcripts do not re-authorize a lost or stale draft.
type decisionDraft struct {
	orchestration      bool
	version            string
	keys               []string
	actions            []board.DecisionAction
	committed          bool
	uncertain          bool
	reread             bool
	overviewRead       bool
	detailRead         bool
	refreshData        []string
	reviewData         string
	reviewReady        bool
	assessment         *board.CompletionReview
	assessmentReady    bool
	assessmentDisabled bool
	closureProtocol    bool
	rootAssessment     *board.RootAssessment
	rootReady          bool
	request            func(context.Context, GraphRequest) (string, error)
}

func (d *decisionDraft) invalidate() {
	d.clearRootAssessment()
	d.keys, d.actions, d.version = nil, nil, ""
	d.reviewData, d.reviewReady = "", false
	d.reread, d.overviewRead, d.detailRead = true, false, false
	d.refreshData = nil
	d.assessmentReady, d.assessmentDisabled = false, true
}

// A tool result is not yet a model observation. Keep the authoritative review
// through compaction and allow completion only from a subsequent request.
// The private draft and review are deliberately discarded together on resume.
func (d *decisionDraft) beforeRequest(loop *agent.Loop) {
	loop.ContextData = nil
	// Fresh tool results only become evidence seen by the model in the next
	// request. A preplanned tool group cannot refresh a hash and immediately
	// publish conclusions generated before those results were available.
	if d.reread && d.overviewRead && d.detailRead {
		// BeforeRequest runs before compaction. Pin the exact refreshed pages
		// for this request so a summary cannot replace unread corrections.
		// The normal context budget fails closed if they do not fit.
		loop.ContextData = append(loop.ContextData, d.refreshData...)
		d.refreshData = nil
		d.reread = false
	}
	d.assessmentReady = false
	if d.assessment != nil && !d.assessmentDisabled {
		raw, err := json.Marshal(d.assessment)
		if err == nil && strings.Contains(loop.TaskPrompt, string(raw)) && containsInstruction(loop.History, loop.TaskPrompt) {
			// Only the task already in the actual request authorizes reuse.
			// ContextData alone is not sent unless compaction occurs. The task
			// itself is retained verbatim if this request needs compaction.
			d.assessmentReady = true
		}
	}
	if d.reviewData != "" {
		loop.ContextData = append(loop.ContextData, d.reviewData)
		d.reviewReady = true
	}
	d.observeRootAssessment(loop)
}

func (d *decisionDraft) completes() bool {
	return len(d.actions) > 0 && d.actions[len(d.actions)-1].Op == "complete"
}

func (d *decisionDraft) observeRead(section string, raw ...string) {
	if !d.reread {
		return
	}
	if section == "" || section == "overview" {
		d.overviewRead = true
		d.detailRead = false
	} else if d.overviewRead {
		d.detailRead = true
	}
	if d.overviewRead && len(raw) > 0 {
		d.refreshData = append(d.refreshData, "<decision_refresh>\n"+raw[0]+"\n</decision_refresh>")
	}
}

func batchDecision(j Job) bool {
	return j.Kind == "reason" && j.Decision != nil && j.Decision.Version == 2
}

func (d *decisionDraft) result() (string, bool) { return committedDecisionText, d.committed }

func (d *decisionDraft) recover(ctx context.Context) (string, error) {
	raw, err := d.request(ctx, GraphRequest{Op: "decision_receipt"})
	if err != nil {
		return "", err
	}
	var receipt board.DecisionReceipt
	if err = json.Unmarshal([]byte(raw), &receipt); err != nil {
		return "", err
	}
	if receipt.Committed {
		d.committed = true
	}
	d.uncertain = false
	return raw, nil
}

var draftRef = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

func (d *decisionDraft) action(ctx context.Context, a board.StateAction, currentVersion string, gapIDs ...string) (string, error) {
	if d.committed {
		return "", errors.New("decision already committed; no further actions are allowed")
	}
	if d.uncertain {
		if receipt, err := d.recover(ctx); err != nil {
			return "", err
		} else if d.committed {
			return receipt, nil
		}
	}
	if d.reread && a.Op != "reset" {
		return "", errors.New("read the current overview and affected graph section after state_changed or recovery before rebuilding the draft")
	}
	if d.closureProtocol && a.Op != "reset" && (d.rootAssessment == nil || !d.rootReady) {
		return "", errors.New("assess_root must evaluate the original requirements before planning; read its result in a subsequent model request")
	}
	if a.Op == "reset" || a.Op == "preview" || a.Op == "commit" {
		var payload map[string]json.RawMessage
		if len(a.Payload) != 0 && (json.Unmarshal(a.Payload, &payload) != nil || payload == nil || len(payload) != 0) {
			return "", errors.New("preview/commit/reset payload must be omitted or {}; draft unchanged")
		}
	}
	switch a.Op {
	case "reset":
		d.clearRootAssessment()
		d.keys, d.actions, d.version = nil, nil, ""
		d.reviewData, d.reviewReady = "", false
		d.assessmentReady, d.assessmentDisabled = false, true
		return `{"draft":true,"reset":true}`, nil
	case "preview", "commit":
		if d.closureProtocol {
			if err := board.ValidateClosureActions(d.rootAssessment, d.actions); err != nil {
				return "", err
			}
		}
		if a.Op == "commit" && d.completes() && !d.reviewReady {
			if !d.canAssessCompletion() {
				return "", errors.New("completion requires preview and a subsequent model request to review its evidence before commit; draft unchanged")
			}
			// The model already saw every cited observation and original
			// requirement. Still run the real transactional preview, then compare
			// its authoritative packet before reusing that model assessment.
			raw, err := d.action(ctx, board.StateAction{Op: "preview"}, currentVersion)
			if err != nil {
				d.assessmentDisabled = true
				return "", err
			}
			var preview board.DecisionReceipt
			if json.Unmarshal([]byte(raw), &preview) != nil || !d.matchesAssessment(preview.CompletionReview) {
				d.assessmentDisabled = true
				// This internal preview is hidden by the tool error below. It
				// cannot authorize a later request that never received its result.
				d.reviewData, d.reviewReady = "", false
				return "", errors.New("completion preview changed or omitted evidence; call preview explicitly and review its result in a subsequent model request before commit; draft unchanged")
			}
			d.reviewReady = true
		}
		if a.Op == "preview" {
			d.reviewData, d.reviewReady = "", false
		}
		if d.version == "" {
			d.version = currentVersion
		}
		batch := board.DecisionBatch{ExpectedVersion: d.version, Actions: append([]board.DecisionAction{}, d.actions...), Assessment: d.rootAssessment}
		if a.Op == "commit" {
			d.uncertain = true
		}
		raw, err := d.request(ctx, GraphRequest{Op: "decision_" + a.Op, Batch: &batch})
		if err != nil {
			// A definitive version conflict did not apply any writes. Force the
			// model to read new information and rebuild, not just change a hash.
			if graphStateConflict(err) {
				d.uncertain = false
				d.invalidate()
			}
			return "", err
		}
		var receipt board.DecisionReceipt
		if err = json.Unmarshal([]byte(raw), &receipt); err != nil {
			return "", err
		}
		if a.Op == "preview" && d.completes() {
			if receipt.CompletionReview == nil || receipt.ValidationScope != "protocol_only" || receipt.CompletionReview.StateVersion != d.version || receipt.CompletionReview.Acceptance != "not_checked" {
				return "", errors.New("completion preview omitted its evidence review; completion remains unreviewed")
			}
			review, err := json.Marshal(receipt.CompletionReview)
			if err != nil {
				return "", err
			}
			d.reviewData = "<completion_review>\n" + string(review) + "\n</completion_review>"
		}
		if a.Op == "commit" {
			if !receipt.Committed {
				return "", errors.New("commit returned no committed receipt")
			}
			d.committed, d.uncertain = true, false
		}
		return raw, nil
	case "goal", "step", "fact_relation", "curation_request", "complete":
	default:
		return "", errors.New("unknown draft operation")
	}
	if !draftRef.MatchString(a.IdempotencyKey) {
		return "", errors.New("draft key must start with a letter and use at most 64 letters, digits, underscore or hyphen")
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(a.Payload))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil || payload == nil || !json.Valid(a.Payload) {
		return "", errors.New("draft payload must be an object")
	}
	if _, exists := payload["repair"]; exists {
		if !d.orchestration || a.Op != "step" || payload["action"] != "add" {
			return "", errors.New("repair is only available for orchestration step add; draft unchanged")
		}
		// Preserve exact JSON numbers in the predicate through draft canonicalization.
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(a.Payload, &fields)
		var spec artifactcheck.Spec
		if err := json.Unmarshal(fields["repair"], &spec); err != nil {
			return "", err
		}
		if err := artifactcheck.Validate(spec); err != nil {
			return "", err
		}
		payload["repair"] = fields["repair"]
	}
	if a.Op == "goal" && payload["id"] == "goal" {
		return "", errors.New("root goal cannot be changed by goal actions; use complete with supporting facts and proof; draft unchanged")
	}
	if a.Op == "curation_request" && !d.orchestration {
		return "", errors.New("curation_request requires orchestration version 1; draft unchanged")
	}
	if a.Op == "step" && payload["action"] == "retry" {
		if !d.orchestration {
			return "", errors.New("step retry requires orchestration version 1; draft unchanged")
		}
		for field := range payload {
			if field != "action" && field != "id" && field != "latest_run_id" && field != "reason" {
				return "", errors.New("step retry requires only action, id, latest_run_id and reason; existing task inputs remain immutable; draft unchanged")
			}
		}
	}
	if err := validateDraftFields(a.Op, payload); err != nil {
		return "", err
	}
	if err := d.validateDependencies(a.Op, payload); err != nil {
		return "", err
	}
	if a.Op == "complete" {
		var completion struct {
			From        []string `json:"from"`
			Description string   `json:"description"`
		}
		decoder := json.NewDecoder(bytes.NewReader(a.Payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&completion); err != nil || len(completion.From) == 0 || strings.TrimSpace(completion.Description) == "" {
			return "", errors.New("complete payload requires only from:[fact IDs] and description:proof; draft unchanged")
		}
	}
	if a.Op == "step" && payload["action"] == "add" {
		from, _ := payload["from"].([]any)
		for _, source := range from {
			if source == "goal" {
				return "", errors.New("step from cannot include root goal; use goal_id to bind the target; from accepts evidence (origin is allowed); draft unchanged")
			}
			if id, ok := source.(string); ok && strings.HasPrefix(id, "$") {
				if d.orchestration {
					return "", errors.New("step from requires published fact IDs or origin; put prerequisite Step IDs or earlier Step $aliases in depends_on; draft unchanged")
				}
				return "", errors.New("step from requires published fact IDs or origin, not draft goal/step aliases; wait for a Step to publish evidence before planning work that depends on it; draft unchanged")
			}
		}
	}
	canonical, _ := json.Marshal(payload)
	item := board.DecisionAction{Op: a.Op, Payload: canonical}
	if len(gapIDs) > 0 {
		item.GapID = gapIDs[0]
	}
	if err := d.checkRootAction(item, payload); err != nil {
		return "", err
	}
	if (a.Op == "goal" || a.Op == "step") && payload["action"] == "add" {
		item.Ref = a.IdempotencyKey
	}
	for n, key := range d.keys {
		if key == a.IdempotencyKey {
			old, _ := json.Marshal(d.actions[n])
			next, _ := json.Marshal(item)
			if string(old) != string(next) {
				return "", errors.New("draft key already identifies another action; reset before replacing a plan")
			}
			return draftReply(item), nil
		}
	}
	if d.completes() {
		return "", errors.New("complete must be the last action; reset before changing the proposed completion; draft unchanged")
	}
	if len(d.actions) >= 64 {
		return "", errors.New("at most 64 draft actions are allowed")
	}
	if d.version == "" {
		d.version = currentVersion
	}
	d.keys, d.actions = append(d.keys, a.IdempotencyKey), append(d.actions, item)
	d.reviewData, d.reviewReady = "", false
	if a.Op != "complete" || len(d.actions) != 1 {
		d.assessmentReady, d.assessmentDisabled = false, true
	}
	return draftReply(item), nil
}

func (d *decisionDraft) canAssessCompletion() bool {
	a := d.assessment
	if !d.orchestration || !d.assessmentReady || d.assessmentDisabled || a == nil || a.Acceptance != "not_checked" || a.StateVersion != d.version || !completionReviewProvided(a) || len(d.actions) != 1 || !d.completes() {
		return false
	}
	origin, goal := false, false
	provided := map[string]bool{}
	for _, input := range a.UserInputs {
		origin = origin || input.ID == "origin"
		goal = goal || input.ID == "goal"
		provided[input.ID] = true
	}
	for _, fact := range a.FactRecords {
		provided[fact.ID] = true
	}
	sourcesProvided := func(ids []string) bool {
		for _, id := range ids {
			if !provided[id] {
				return false
			}
		}
		return true
	}
	for _, candidate := range a.Candidates {
		if !sourcesProvided(candidate.Sources) {
			return false
		}
	}
	for _, dispute := range a.Disputes {
		if !sourcesProvided(dispute.ReviewFactIDs) {
			return false
		}
	}
	if a.CurationRequest != nil && !sourcesProvided(a.CurationRequest.Sources) {
		return false
	}
	return origin && goal
}

func (d *decisionDraft) matchesAssessment(review *board.CompletionReview) bool {
	if !d.canAssessCompletion() || review == nil || review.StateVersion != d.version || review.Acceptance != "not_checked" || !completionReviewProvided(review) || !reflect.DeepEqual(review.UserInputs, d.assessment.UserInputs) || !reflect.DeepEqual(review.Hints, d.assessment.Hints) || !reflect.DeepEqual(review.Candidates, d.assessment.Candidates) || !reflect.DeepEqual(review.Disputes, d.assessment.Disputes) || !reflect.DeepEqual(review.CurationRequest, d.assessment.CurationRequest) {
		return false
	}
	var proposed struct {
		From        []string `json:"from"`
		Description string   `json:"description"`
	}
	if json.Unmarshal(d.actions[0].Payload, &proposed) != nil || !reflect.DeepEqual(review.From, proposed.From) || review.Description != proposed.Description || len(review.FactRecords) != len(proposed.From) {
		return false
	}
	provided := map[string]board.FactRecord{}
	for _, fact := range d.assessment.FactRecords {
		if _, duplicate := provided[fact.ID]; duplicate || fact.Legacy || fact.SupportInvalid || fact.Status != "valid" || len(fact.Evidence) == 0 {
			return false
		}
		provided[fact.ID] = fact
	}
	seen := map[string]bool{}
	for n, fact := range review.FactRecords {
		original, ok := provided[fact.ID]
		if !ok || seen[fact.ID] || fact.ID != proposed.From[n] || !reflect.DeepEqual(fact, original) {
			return false
		}
		seen[fact.ID] = true
	}
	return true
}

func completionReviewProvided(review *board.CompletionReview) bool {
	if len(review.OmittedFactIDs)+len(review.OmittedCandidateIDs)+len(review.OmittedDisputeIDs) != 0 {
		return false
	}
	for _, candidate := range review.Candidates {
		if candidate.EvidenceOmitted || candidate.EvidenceCount > len(candidate.Evidence) {
			return false
		}
	}
	return true
}

// Existing IDs and accepted outcomes are checked by the board. Draft aliases
// can be checked locally without a graph read or an extra planning turn.
func (d *decisionDraft) validateDependencies(op string, payload map[string]any) error {
	value, exists := payload["depends_on"]
	if !exists {
		return nil
	}
	if !d.orchestration || op != "step" || payload["action"] != "add" {
		return errors.New("depends_on is only available for orchestration step add; draft unchanged")
	}
	values, ok := value.([]any)
	if !ok {
		return errors.New("depends_on requires an array of Step IDs or earlier Step $aliases; draft unchanged")
	}
	seen := map[string]bool{}
	for _, value := range values {
		id, ok := value.(string)
		if !ok || strings.TrimSpace(id) == "" || seen[id] || id == "origin" || id == "goal" {
			return errors.New("depends_on requires unique Step IDs, never fact or goal IDs; draft unchanged")
		}
		seen[id] = true
		if strings.HasPrefix(id, "$") {
			found := false
			for _, action := range d.actions {
				if action.Op == "step" && action.Ref == strings.TrimPrefix(id, "$") {
					found = true
					break
				}
			}
			if !found {
				return errors.New("depends_on aliases must reference earlier step add actions in this draft; draft unchanged")
			}
		}
	}
	return nil
}

// Reject missing action fields before a bad draft reserves its key. The board
// remains authoritative for references, state transitions and evidence.
func validateDraftFields(op string, payload map[string]any) error {
	text := func(key string) bool {
		value, ok := payload[key].(string)
		return ok && strings.TrimSpace(value) != ""
	}
	ids := func(key string) bool {
		values, ok := payload[key].([]any)
		if !ok || len(values) == 0 {
			return false
		}
		for _, value := range values {
			id, ok := value.(string)
			if !ok || strings.TrimSpace(id) == "" {
				return false
			}
		}
		return true
	}
	valid := true
	switch op {
	case "goal":
		switch payload["action"] {
		case "add":
			valid = text("condition")
		case "achieve":
			valid = text("id") && text("reason") && ids("sources")
		case "withdraw":
			valid = text("id") && text("reason")
		default:
			valid = false
		}
	case "step":
		if value, exists := payload["priority"]; exists {
			priority, ok := value.(float64)
			if number, isNumber := value.(json.Number); isNumber {
				var err error
				priority, err = number.Float64()
				ok = err == nil
			}
			if !ok || priority < 0 || priority > 1000000 || priority != float64(int(priority)) {
				return errors.New("step priority must be an integer between 0 and 1000000; draft unchanged")
			}
		}
		switch payload["action"] {
		case "add":
			valid = ids("from") && text("description")
		case "priority", "abandon":
			valid = text("id") && text("reason")
		case "retry":
			valid = text("id") && text("latest_run_id") && text("reason")
		default:
			valid = false
		}
	case "fact_relation":
		kind, _ := payload["kind"].(string)
		valid = (kind == "supersedes" || kind == "refutes" || kind == "narrows") && text("source") && text("target") && text("reason")
	case "curation_request":
		for field := range payload {
			if field != "sources" && field != "reason" {
				return errors.New("curation_request requires only sources:[Fact IDs] and reason; draft unchanged")
			}
		}
		valid = text("reason") && ids("sources")
		if valid {
			sources := payload["sources"].([]any)
			valid = len(sources) <= 32 && len(payload["reason"].(string)) <= 8192
			seen := map[string]bool{}
			for _, value := range sources {
				id := value.(string)
				if len(id) > 256 || id == "origin" || id == "goal" || strings.HasPrefix(id, "$") || seen[id] {
					return errors.New("curation_request sources require 1-32 unique published observation Fact IDs, never user inputs or draft aliases; draft unchanged")
				}
				seen[id] = true
			}
		}
	}
	if !valid {
		return errors.New(op + " payload is missing required action fields; see the tool contract; draft unchanged")
	}
	return nil
}

func draftReply(item board.DecisionAction) string {
	reply := map[string]any{"draft": true, "op": item.Op}
	if item.Ref != "" {
		reply["id"] = "$" + item.Ref
	}
	raw, _ := json.Marshal(reply)
	return string(raw)
}
