package contract

import (
	"encoding/json"
	"testing"
)

func TestFinalEvidenceCarriesOptionalAssetsWithoutWeakeningEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, assets string
		valid        bool
	}{
		{"endpoint", `[{"kind":"endpoint","value":"https://example.test/a","method":"GET"}]`, true},
		{"two objects", `[{"kind":"host","value":"example.test"},{"kind":"service","value":"https://example.test"}]`, true},
		{"empty optional index", `[]`, true},
		{"null", `null`, false},
		{"null item", `[null]`, false},
		{"missing identity", `[{"kind":"host"}]`, false},
		{"false truth", `[{"kind":"host","value":"example.test","verified":true}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fact := `{"description":"Observed response","scope":"anonymous request","observed_at":"2026-10-08T00:00:00Z","evidence":[{"path":"response.txt"}],"assets":` + tc.assets + `}`
			result, err := parseEvidenceResult(json.RawMessage(`{"fact":` + fact + `}`))
			if (err == nil) != tc.valid {
				t.Fatalf("accepted=%v: %v", err == nil, err)
			}
			if tc.valid && string(result.FactPayload) != fact {
				t.Fatal("asset input or evidence was rewritten")
			}
		})
	}
	if _, err := parseEvidenceResult(json.RawMessage(`{"fact":{"description":"No proof","scope":"fixture","observed_at":"2026-10-08T00:00:00Z","assets":[{"kind":"host","value":"example.test"}]}}`)); err == nil {
		t.Fatal("asset substituted for evidence")
	}
}
