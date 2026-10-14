package worker

import "encoding/json"

func assetInputSchema() map[string]any {
	return map[string]any{"type": "array", "maxItems": 32, "description": "Optional related assets for fact/candidate publication or step add. Runtime normalizes identity; associations do not establish evidence, coverage or permission.", "items": map[string]any{
		"type": "object", "properties": map[string]any{
			"kind":   map[string]any{"type": "string", "enum": []string{"host", "service", "endpoint"}},
			"value":  map[string]any{"type": "string", "minLength": 1, "maxLength": 4096},
			"method": map[string]any{"type": "string", "description": "Required for endpoint only; preserves HTTP method identity."},
		}, "required": []string{"kind", "value"}, "additionalProperties": false,
	}}
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
