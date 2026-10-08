package worker

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"pwnmesh/internal/board"
)

func validateGraphAssetFilter(r GraphRequest) error {
	if len(r.AssetIDs) == 0 {
		return nil
	}
	if (r.Op != "" && r.Op != "read_graph" && r.Op != "read_snapshot") || !slices.Contains([]string{"assets", "anchors", "facts", "steps", "findings", "candidates", "history"}, r.Section) {
		return errors.New("asset_ids requires an assets, anchors, facts, steps, findings, candidates or history read")
	}
	if len(r.AssetIDs) > 32 {
		return errors.New("at most 32 asset IDs are allowed")
	}
	seen := map[string]bool{}
	for _, id := range r.AssetIDs {
		if strings.TrimSpace(id) == "" || len(id) > 256 || seen[id] {
			return errors.New("asset IDs must be unique, nonempty and at most 256 bytes")
		}
		seen[id] = true
	}
	return nil
}

// Filter identities, never evidence validity. Both the catalogue and anchors
// come from the same State, including when that State is a frozen read view.
func filterGraphAssets(s board.State, r GraphRequest, items []json.RawMessage) ([]json.RawMessage, error) {
	if len(r.AssetIDs) == 0 {
		return items, nil
	}
	known := map[string]bool{}
	for _, asset := range s.Assets {
		known[asset.ID] = true
	}
	selected := map[string]bool{}
	for _, id := range r.AssetIDs {
		if !known[id] {
			return nil, errors.New("asset is absent from this graph input")
		}
		selected[id] = true
	}
	attached := map[string]bool{}
	for _, anchor := range s.AssetAnchors {
		if selected[anchor.AssetID] {
			attached[anchor.NodeKind+":"+anchor.NodeID] = true
		}
	}
	kind := map[string]string{"facts": "fact", "steps": "step", "findings": "finding", "candidates": "candidate"}[r.Section]
	out := []json.RawMessage{}
	for _, raw := range items {
		var node struct {
			ID       string   `json:"id"`
			AssetID  string   `json:"asset_id"`
			AssetIDs []string `json:"asset_ids"`
		}
		if err := json.Unmarshal(raw, &node); err != nil {
			return nil, err
		}
		include := attached[kind+":"+node.ID]
		switch r.Section {
		case "assets":
			include = selected[node.ID]
		case "anchors":
			include = selected[node.AssetID]
		case "history":
			for _, id := range node.AssetIDs {
				include = include || selected[id]
			}
		}
		if include {
			out = append(out, raw)
		}
	}
	return out, nil
}

func assetInputSchema() map[string]any {
	return map[string]any{"type": "array", "maxItems": 32, "description": "Optional related assets for fact/candidate publication or step add. Runtime normalizes identity; associations do not establish evidence, coverage or permission.", "items": map[string]any{
		"type": "object", "properties": map[string]any{
			"kind":   map[string]any{"type": "string", "enum": []string{"host", "service", "endpoint"}},
			"value":  map[string]any{"type": "string", "minLength": 1, "maxLength": 4096},
			"method": map[string]any{"type": "string", "description": "Required for endpoint only; preserves HTTP method identity."},
		}, "required": []string{"kind", "value"}, "additionalProperties": false,
	}}
}

func assetGraphReadSchema(raw json.RawMessage) json.RawMessage {
	// The base schema is a static program constant, not model input.
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		panic(err)
	}
	properties := schema["properties"].(map[string]any)
	section := properties["section"].(map[string]any)
	section["enum"] = append(section["enum"].([]any), "assets", "anchors", "history")
	properties["asset_ids"] = map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}, "maxItems": 32, "uniqueItems": true, "description": "Match any of these assets in assets/anchors/facts/steps/findings/candidates/history. Combine with ids to narrow records; preserve filters while paging."}
	result, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	return result
}

func assetFinishSchema(raw json.RawMessage) json.RawMessage {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		panic(err)
	}
	fact := schema["properties"].(map[string]any)["fact"].(map[string]any)
	fact["properties"].(map[string]any)["assets"] = assetInputSchema()
	result, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	return result
}

func validateTraceRunRequest(j Job, r GraphRequest) error {
	if !j.GraphRPC || j.Kind != "explore" || j.Intent == nil || j.RunID == "" || j.Graph.Project.ID == "" {
		return errors.New("trace reads require a registered Execute bridge")
	}
	if r.Section != "" || r.ByteOffset != nil || r.ExpectedVersion != "" || r.RecordVersion != "" || r.Updates != nil || r.Batch != nil || r.Action.Op != "" || len(r.IDs) > 1 || r.Offset < 0 || r.Limit < 0 || r.Limit > 20 {
		return errors.New("trace reads accept only one run ID or an offset/limit page")
	}
	if len(r.IDs) == 1 && (strings.TrimSpace(r.IDs[0]) == "" || len(r.IDs[0]) > 256 || r.IDs[0] == j.RunID || r.Offset != 0) {
		return errors.New("trace run must identify another execution in the current project")
	}
	return nil
}
