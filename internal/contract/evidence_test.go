package contract

import (
	"encoding/json"
	"testing"
)

func TestEvidenceResultRequiresOneSupportedAnchor(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"published fact", `{"fact_id":"f001"}`, true},
		{"selected evidence", `{"fact":{"description":"No authentication challenge observed","scope":"GET /fixture","observed_at":"2026-09-22T10:00:00Z","evidence":[{"path":"response.txt"}]}}`, true},
		{"bare legacy description", `{"description":"Looks done"}`, false},
		{"ambiguous anchor", `{"fact_id":"f001","fact":{}}`, false},
		{"null anchor", `{"fact_id":null}`, false},
		{"no evidence", `{"fact":{"description":"Observation","scope":"fixture","observed_at":"2026-09-22T10:00:00Z","evidence":[]}}`, false},
		{"source identity injection", `{"fact":{"description":"Observation","scope":"fixture","observed_at":"2026-09-22T10:00:00Z","evidence":[{"path":"response.txt"}],"source_step_id":"other"}}`, false},
		{"unknown evidence field", `{"fact":{"description":"Observation","scope":"fixture","observed_at":"2026-09-22T10:00:00Z","evidence":[{"path":"response.txt","claim":"made up"}]}}`, false},
		{"null selection field", `{"fact":{"description":"Observation","scope":"fixture","observed_at":"2026-09-22T10:00:00Z","evidence":[{"path":"response.txt","start_line":null}]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := `{"accepted":true,"outcome":"completed","data":` + tc.data + `}`
			for _, conclude := range []bool{false, true} {
				got, err := ParseWithPolicy(output, "explore", conclude, Policy{Version: 2, GraphRPC: true})
				if (err == nil) != tc.valid {
					t.Fatalf("result=%+v error=%v", got, err)
				}
				if err == nil && (got.Kind != "fact" || got.Outcome != "completed" || (got.FactID == "") == (len(got.FactPayload) == 0)) {
					t.Fatalf("invalid anchor: %+v", got)
				}
			}
		})
	}
}

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
