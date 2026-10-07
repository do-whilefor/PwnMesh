package board

import "encoding/json"

// RootAssessment records the model's semantic judgment separately from its
// plan. The runtime validates provenance and control flow, not natural language.
// Its authority is the enclosing DecisionBatch's checked state version.
type RootAssessment struct {
	Status      string           `json:"status"`
	From        []string         `json:"from"`
	Description string           `json:"description"`
	Gaps        []RequirementGap `json:"gaps,omitempty"`
}

type RequirementGap struct {
	ID          string   `json:"id"`
	InputIDs    []string `json:"input_ids"`
	Description string   `json:"description"`
}

// ValidateRootAssessment is shared with the Worker for early draft feedback.
// The Board additionally checks From against live effective evidence under CAS.
func ValidateRootAssessment(a *RootAssessment, inputs []Fact, hints []Hint) error {
	if err := ValidateRootAssessmentShape(a); err != nil {
		return err
	}
	original := map[string]bool{}
	for _, input := range inputs {
		original[input.ID] = true
	}
	for _, hint := range hints {
		original[hint.ID] = true
	}
	for _, gap := range a.Gaps {
		for _, id := range gap.InputIDs {
			if !original[id] {
				return Err(422, "requirement gap input_ids must cite original user inputs or hints, not planner goals or observations")
			}
		}
	}
	return nil
}

// ValidateRootAssessmentShape does not need a graph snapshot. This lets a
// Worker validate refreshed drafts while authoritative input checks stay live.
func ValidateRootAssessmentShape(a *RootAssessment) error {
	if a == nil {
		return Err(422, "root assessment is required before planning or completing")
	}
	if (a.Status != "satisfied" && a.Status != "missing") || !required(a.Description, 8192) || len(a.From) > 256 || len(a.Gaps) > 32 {
		return Err(422, "root assessment requires satisfied/missing status, description and bounded evidence/gaps")
	}
	seen := map[string]bool{}
	for _, id := range a.From {
		if !required(id, 256) || seen[id] {
			return Err(422, "root assessment evidence IDs must be nonempty and unique")
		}
		seen[id] = true
	}
	if a.Status == "satisfied" {
		if len(a.From) == 0 || len(a.Gaps) != 0 {
			return Err(422, "satisfied root assessment requires evidence and no missing gaps")
		}
		return nil
	}
	if len(a.Gaps) == 0 {
		return Err(422, "missing root assessment requires a concrete original-requirement gap")
	}
	seen = map[string]bool{}
	for _, gap := range a.Gaps {
		if !decisionAlias.MatchString(gap.ID) || seen[gap.ID] || !required(gap.Description, 8192) || len(gap.InputIDs) == 0 || len(gap.InputIDs) > 32 {
			return Err(422, "requirement gaps need unique IDs, descriptions and original user input IDs")
		}
		seen[gap.ID] = true
		refs := map[string]bool{}
		for _, id := range gap.InputIDs {
			if !required(id, 256) || refs[id] {
				return Err(422, "requirement gap input_ids must be nonempty and unique")
			}
			refs[id] = true
		}
	}
	return nil
}

// ValidateClosureActions enforces the root judgment at both preview and commit.
// It does not decide whether a natural-language gap correctly interprets a user.
func ValidateClosureActions(a *RootAssessment, actions []DecisionAction) error {
	if err := ValidateRootAssessmentShape(a); err != nil {
		return err
	}
	gaps := map[string]bool{}
	for _, gap := range a.Gaps {
		gaps[gap.ID] = true
	}
	complete := false
	for _, action := range actions {
		var payload struct {
			Action string   `json:"action"`
			From   []string `json:"from"`
		}
		if json.Unmarshal(action.Payload, &payload) != nil {
			return Err(422, "invalid assessed decision payload")
		}
		expands := action.Op == "curation_request" || action.Op == "goal" && payload.Action == "add" || action.Op == "step" && (payload.Action == "add" || payload.Action == "retry")
		if expands && (a.Status != "missing" || !gaps[action.GapID]) {
			return Err(422, "new work requires a gap_id from a missing root assessment")
		}
		if action.GapID != "" && (!expands || !gaps[action.GapID]) {
			return Err(422, "gap_id must bind new work to an assessed requirement gap")
		}
		if action.Op == "complete" {
			if a.Status != "satisfied" {
				return Err(422, "cannot complete while the root assessment reports missing requirements")
			}
			proof := map[string]bool{}
			for _, id := range a.From {
				proof[id] = true
			}
			if len(payload.From) != len(proof) {
				return Err(422, "completion must cite exactly the root assessment evidence")
			}
			for _, id := range payload.From {
				if !proof[id] {
					return Err(422, "completion must cite exactly the root assessment evidence")
				}
				delete(proof, id)
			}
			complete = true
		}
		if a.Status == "satisfied" && action.Op != "complete" && !(action.Op == "step" && payload.Action == "abandon") && !(action.Op == "goal" && (payload.Action == "achieve" || payload.Action == "withdraw")) {
			return Err(422, "satisfied root assessment permits only explicit work closure and complete")
		}
	}
	if a.Status == "satisfied" && !complete {
		return Err(422, "satisfied root assessment must explicitly complete the project")
	}
	return nil
}
