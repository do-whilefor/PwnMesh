package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func (d *decisionDraft) clearRootAssessment() {
	d.rootAssessment, d.rootReady = nil, false
}

func (d *decisionDraft) observeRootAssessment(loop *agent.Loop) {
	if d.rootAssessment != nil {
		raw, _ := json.Marshal(d.rootAssessment)
		loop.ContextData = append(loop.ContextData, "<root_assessment>\n"+string(raw)+"\n</root_assessment>")
		d.rootReady = true
	}
}

// Assessment is a separate model turn before planning. It is private and is
// discarded along with the draft on conflicts, resets and process recovery.
func (d *decisionDraft) assessRoot(raw json.RawMessage, version string) (string, error) {
	if !d.closureProtocol || d.committed || d.uncertain || d.reread {
		return "", errors.New("root assessment requires a current, uncommitted decision view; finish rereading changed evidence first")
	}
	if len(d.actions) != 0 || d.rootAssessment != nil {
		return "", errors.New("reset before replacing the root assessment and its plan")
	}
	var assessment board.RootAssessment
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&assessment); err != nil || !json.Valid(raw) {
		return "", errors.New("invalid root assessment")
	}
	// Input references and current evidence are authoritatively checked by the
	// Board against the live version at preview/commit, including refreshed input.
	if err := board.ValidateRootAssessmentShape(&assessment); err != nil {
		return "", err
	}
	d.rootAssessment, d.rootReady, d.version = &assessment, false, version
	result, _ := json.Marshal(map[string]any{"assessment": assessment, "next": "Read this assessment in the next model request before staging actions. Every new goal, step, retry or curation request requires a gap_id; satisfied permits only explicit closure and complete."})
	return string(result), nil
}

func (d *decisionDraft) checkRootAction(action board.DecisionAction, payload map[string]any) error {
	if !d.closureProtocol {
		if action.GapID != "" {
			return errors.New("gap_id requires the root assessment protocol")
		}
		return nil
	}
	a := d.rootAssessment
	if a == nil || !d.rootReady {
		return errors.New("assess_root must precede planning")
	}
	expands := action.Op == "curation_request" || (action.Op == "goal" && payload["action"] == "add") || (action.Op == "step" && (payload["action"] == "add" || payload["action"] == "retry"))
	if expands {
		if a.Status != "missing" || !slices.ContainsFunc(a.Gaps, func(g board.RequirementGap) bool { return g.ID == action.GapID }) {
			return errors.New("new work requires gap_id from a missing original requirement; satisfied assessments cannot expand work")
		}
	} else if action.GapID != "" {
		return errors.New("gap_id is only for new goals, steps, retries or curation requests")
	}
	if action.Op == "complete" && a.Status != "satisfied" {
		return errors.New("missing original requirements cannot be completed or withdrawn away; reset and reassess after obtaining evidence")
	}
	return nil
}

func rootAssessmentTool(d *decisionDraft, version *string) agent.Tool {
	return agent.Tool{Definition: agent.Definition{
		Name:        "assess_root",
		Description: "Before planning, compare original user_inputs and hints against observed evidence, even with active Steps. Return satisfied with proof, or missing with concrete unmet requirements anchored by input_ids. Preserve uncertainty; existing Step descriptions are plans, not additional user requirements. This private assessment must be read in a subsequent model request before any graph_action; conflicts/reset/recovery discard it. The server still checks evidence and completion.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","enum":["satisfied","missing"]},"from":{"type":"array","items":{"type":"string"},"uniqueItems":true},"description":{"type":"string","minLength":1},"gaps":{"type":"array","maxItems":32,"items":{"type":"object","properties":{"id":{"type":"string"},"input_ids":{"type":"array","items":{"type":"string"},"minItems":1,"uniqueItems":true},"description":{"type":"string","minLength":1}},"required":["id","input_ids","description"],"additionalProperties":false}}},"required":["status","from","description"],"additionalProperties":false}`),
	}, Execute: func(_ context.Context, raw json.RawMessage) (string, error) {
		return d.assessRoot(raw, *version)
	}}
}
