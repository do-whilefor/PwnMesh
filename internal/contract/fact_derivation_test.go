package contract

import (
	"encoding/json"
	"testing"
)

func TestFinalFactDerivationPreservedAndValidated(t *testing.T) {
	for _, sources := range []string{`["first","second"]`, `null`, `["first","first"]`, `[""]`} {
		fact := `{"description":"Calculated result","scope":"fixture","observed_at":"2026-10-16T00:00:00Z","evidence":[{"path":"calculation.txt"}],"derived_from":` + sources + `}`
		result, err := parseEvidenceResult(json.RawMessage(`{"fact":` + fact + `}`))
		if sources == `["first","second"]` {
			if err != nil || string(result.FactPayload) != fact {
				t.Fatalf("final handoff lost exact derivation: %v", err)
			}
		} else if err == nil {
			t.Fatalf("malformed final derivation accepted: %s", sources)
		}
	}
}
