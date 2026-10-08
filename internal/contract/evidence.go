package contract

import (
	"encoding/json"
	"errors"
)

// Version two binds a completed exploration to one evidence-backed result.
// The runtime freezes file selections; the board verifies their provenance.
func parseEvidenceResult(raw json.RawMessage) (Result, error) {
	data, err := object(raw)
	if err != nil {
		return Result{}, errors.New("completed data must be an object")
	}
	r := Result{Kind: "fact", Outcome: "completed"}
	_, byID := data["fact_id"]
	_, byFact := data["fact"]
	if byID == byFact {
		return Result{}, errors.New("completed requires exactly one of fact_id or fact")
	}
	if len(data) != 1 {
		return Result{}, errors.New("unexpected completed result field")
	}
	if byID {
		r.FactID, err = text(data["fact_id"])
		if err != nil || len(r.FactID) > 256 {
			return Result{}, errors.New("fact_id must be a nonempty fact identifier")
		}
		return r, nil
	}
	fact, err := object(data["fact"])
	if err != nil {
		return Result{}, errors.New("fact requires an object")
	}
	for key := range fact {
		switch key {
		case "description", "scope", "observed_at", "evidence", "assets":
		default:
			return Result{}, errors.New("unexpected fact field")
		}
	}
	if assets, exists := fact["assets"]; exists {
		var items []map[string]json.RawMessage
		if string(assets) == "null" || json.Unmarshal(assets, &items) != nil || len(items) > 32 {
			return Result{}, errors.New("fact assets requires at most 32 asset objects")
		}
		for _, item := range items {
			for _, key := range []string{"kind", "value"} {
				if _, err := text(item[key]); err != nil {
					return Result{}, errors.New("asset requires nonempty kind and value")
				}
			}
			for key, value := range item {
				if key != "kind" && key != "value" && key != "method" {
					return Result{}, errors.New("unexpected asset field")
				}
				if _, err := text(value); err != nil {
					return Result{}, errors.New("asset fields must be nonempty strings")
				}
			}
		}
	}
	for _, key := range []string{"description", "scope", "observed_at"} {
		if _, err := text(fact[key]); err != nil {
			return Result{}, errors.New("fact requires nonempty " + key)
		}
	}
	var refs []map[string]json.RawMessage
	if json.Unmarshal(fact["evidence"], &refs) != nil || len(refs) == 0 || len(refs) > 32 {
		return Result{}, errors.New("fact evidence requires 1-32 file selections")
	}
	for _, ref := range refs {
		if ref == nil {
			return Result{}, errors.New("evidence selection must be an object")
		}
		if _, err := text(ref["path"]); err != nil {
			return Result{}, errors.New("evidence selection requires a path")
		}
		for key, value := range ref {
			if string(value) == "null" {
				return Result{}, errors.New("evidence selection fields cannot be null")
			}
			switch key {
			case "path", "run_id", "excerpt":
				var valueText string
				if json.Unmarshal(value, &valueText) != nil {
					return Result{}, errors.New("evidence text fields must be strings")
				}
			case "start_line", "end_line":
				var line int
				if json.Unmarshal(value, &line) != nil || line < 0 {
					return Result{}, errors.New("evidence line bounds must be nonnegative integers")
				}
			default:
				return Result{}, errors.New("unexpected evidence selection field")
			}
		}
	}
	r.FactPayload = append(json.RawMessage(nil), data["fact"]...)
	return r, nil
}
