package worker

import (
	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/board"
)

const evidenceSelectionDescription = "For new evidence, select a UTF-8 file at most 32 MiB and a nonempty excerpt at most 8192 bytes. Omit both line bounds for the whole file, or supply start_line and end_line together (1-based, inclusive)."
const factScopeDescription = "For a fact, preserve any task-specified scope value exactly; put additional explanation in description."

// Describe every input field available in this mode, including the transition
// discriminator. Conditional requirements and operation semantics remain with
// the draft/server validators; this is not a second action validator.
func orchestrationPayloadSchema(kind string) map[string]any {
	text := map[string]any{"type": "string"}
	if kind == "reason" {
		properties := map[string]any{"description": text}
		for _, name := range []string{"id", "condition", "parent_id"} {
			properties[name] = text
		}
		properties["from"] = map[string]any{"type": "array", "items": text, "description": "Step inputs: published, effective Fact IDs or origin. Complete: published, effective Fact IDs only, never origin. goal is a user constraint, never a source; assign a Goal with goal_id instead."}
		properties["goal_id"] = map[string]any{"type": "string", "description": "The Goal this Step advances; use goal for the root requirement. Never put this ID in from."}
		properties["priority"] = map[string]any{"type": "integer", "minimum": 0, "maximum": 1000000}
		properties["action"] = map[string]any{
			"type": "string", "enum": []string{"add", "achieve", "withdraw", "abandon", "priority", "retry"},
			"description": "Only goal (add/achieve/withdraw) and step (add/abandon/priority/retry) use action. Complete uses from and description without action; the root goal is completed only by complete. step retry requires only id, latest_run_id and reason and authorizes one new execution after failure.",
		}
		properties["latest_run_id"] = map[string]any{"type": "string", "description": "Required for step retry; copy the target Step's latest_run_id to bind the observed failed attempt."}
		properties["dispute_id"] = map[string]any{"type": "string", "description": "Assign an independent review Step with a new execution. Include both sides' raw sources and a specific question in description."}
		properties["repair"] = artifactcheck.Schema()
		properties["assets"] = assetInputSchema()
		properties["depends_on"] = map[string]any{
			"type": "array", "items": text, "uniqueItems": true,
			"description": "Optional step add prerequisites: existing Step IDs or earlier Step $aliases, never Fact IDs. Use from for evidence inputs.",
		}
		properties["write_paths"] = map[string]any{
			"type": "array", "maxItems": 16,
			"items":       map[string]any{"type": "string", "minLength": 1, "maxLength": 4096},
			"description": "Optional immutable step add output files or directories under /workspace, outside /workspace/.pwnmesh. Declare shared writes; overlapping scopes run sequentially. repair.path is included automatically.",
		}
		properties["sources"] = map[string]any{"type": "array", "items": text, "description": "Goal achievement support. For curation_request, provide 1-32 unique published, effective observation Fact IDs; never origin, goal or draft aliases. The server validates current support."}
		properties["reason"] = map[string]any{"type": "string", "description": "Explain the action. For curation_request, state the concrete evidence conflict or merge requiring Curate; its payload uses only sources and reason."}
		return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	}
	if kind != "curate" {
		return map[string]any{"type": "object", "properties": map[string]any{
			"assets":      assetInputSchema(),
			"description": text,
			"reason":      text,
			"sources":     map[string]any{"type": "array", "items": text},
			"scope":       map[string]any{"type": "string", "description": factScopeDescription},
			"observed_at": map[string]any{"type": "string", "format": "date-time"},
			"claim":       text,
			"status":      map[string]any{"type": "string", "enum": []string{"candidate", "verified", "refuted"}, "description": "Optional producer judgment; omitted means a tentative candidate, never a shared conclusion."},
			"supersedes":  map[string]any{"type": "string", "description": "Optional active Candidate ID from this run with the same claim and scope. Supply current support; earlier evidence, reasoning and judgment remain in history."},
			"evidence": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "description": evidenceSelectionDescription, "properties": map[string]any{
					"path": text,
					// Historical references remain valid for Findings backed by sources.
					"run_id":     text,
					"excerpt":    text,
					"start_line": map[string]any{"type": "integer"},
					"end_line":   map[string]any{"type": "integer"},
				},
			}},
		}}
	}
	return map[string]any{"type": "object", "properties": map[string]any{
		"relations": map[string]any{"type": "array", "maxItems": board.MaxCurationRelations, "items": map[string]any{
			"type": "object", "properties": map[string]any{
				"kind":   map[string]any{"type": "string", "enum": []string{"supersedes", "refutes", "narrows"}},
				"source": text, "target": text, "reason": text,
			}, "required": []string{"kind", "source", "target", "reason"}, "additionalProperties": false,
		}},
		"groups": map[string]any{"type": "array", "maxItems": 128, "description": "One group per equal candidate group_key requiring reconciliation (read-only grouping hint, not a graph ID). Identify each group with active candidate_ids; omit group_key. Combine original and review candidate IDs; unrelated singleton notes need no group.", "items": map[string]any{
			"type": "object", "properties": map[string]any{
				"candidate_ids": map[string]any{"type": "array", "items": text, "uniqueItems": true}, "status": map[string]any{"type": "string", "enum": []string{"candidate", "verified", "refuted"}},
				"reason": text, "question": text, "dispute_id": map[string]any{"type": []string{"string", "null"}},
				"review_fact_ids": map[string]any{"type": []string{"array", "null"}, "items": text, "uniqueItems": true, "description": "Independent review evidence for an existing dispute only; omit for ordinary corroboration."},
				"resolution":      map[string]any{"type": []string{"string", "null"}, "enum": []any{"resolved", "uncertain", nil}, "description": "Only for an existing dispute; include its dispute_id."},
			}, "required": []string{"candidate_ids", "status", "reason"}, "additionalProperties": false,
		}},
	}, "required": []string{"groups"}, "additionalProperties": false}
}
