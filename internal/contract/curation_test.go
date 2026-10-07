package contract

import "testing"

func TestCuratorFinalContractRequiresReceiptShapeAndLiveMode(t *testing.T) {
	policy := Policy{Version: 2, GraphRPC: true}
	for _, tc := range []struct{ output, want string }{
		{`{"accepted":true,"data":{"curated":true}}`, "curated"},
		{`{"accepted":false,"reason":"Missing original sources"}`, "rejected"},
	} {
		r, err := ParseWithPolicy(tc.output, "curate", false, policy)
		if err != nil || r.Kind != tc.want {
			t.Fatalf("valid curation response rejected: %+v %v", r, err)
		}
	}
	for _, raw := range []string{
		`{"accepted":true,"data":{"curated":false}}`, `{"accepted":true,"data":{"curated":true,"fact":{}}}`,
		`{"accepted":true,"outcome":"completed","data":{"curated":true}}`, `{"data":{"curated":true}}`,
		`{"accepted":false}`, `{"accepted":false,"reason":""}`, `{"accepted":true,"data":{"decided":true}}`,
	} {
		if _, err := ParseWithPolicy(raw, "curate", false, policy); err == nil {
			t.Fatalf("accepted invalid curation contract %s", raw)
		}
	}
	for _, tc := range []struct{ conclude, bridge bool }{{true, true}, {false, false}} {
		if _, err := ParseWithPolicy(`{"accepted":true,"data":{"curated":true}}`, "curate", tc.conclude, Policy{Version: 2, GraphRPC: tc.bridge}); err == nil {
			t.Fatal("accepted an unavailable curator mode")
		}
	}
}
