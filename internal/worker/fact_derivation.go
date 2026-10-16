package worker

import board "pwnmesh/internal/board/model"

func factDerivationSchema() map[string]any {
	return map[string]any{"type": "array", "maxItems": board.MaxFactDerivationSources, "uniqueItems": true,
		"items":       map[string]any{"type": "string", "minLength": 1, "maxLength": 256},
		"description": "Fact only: immutable effective observation Fact IDs used to compute this observation. Omit for independent raw observations. Corrections invalidate explicit derivations; original evidence is still required."}
}
