package contract

import "testing"

func TestCurrentResultPolicy(t *testing.T) {
	policy := Policy{Version: 2, GraphRPC: true}
	for _, tc := range []struct {
		kind, output, want string
		conclude           bool
	}{
		{"reason", `{"accepted":true,"data":{"decided":true}}`, "decided", false},
		{"curate", `{"accepted":true,"data":{"curated":true}}`, "curated", false},
		{"explore", `{"accepted":true,"outcome":"completed","data":{"fact_id":"f1"}}`, "fact", false},
		{"explore", `{"accepted":true,"outcome":"continue","reason":"verify remaining inputs"}`, "continue", false},
		{"explore", `{"accepted":true,"outcome":"incomplete","reason":"missing inputs"}`, "incomplete", true},
	} {
		got, err := ParseWithPolicy(tc.output, tc.kind, tc.conclude, 0, 3, policy)
		if err != nil || got.Kind != tc.want {
			t.Fatalf("%s: %+v %v", tc.kind, got, err)
		}
	}
	for _, kind := range []string{"reason", "curate", "explore"} {
		got, err := ParseWithPolicy(`{"accepted":false,"reason":"Missing original evidence"}`, kind, false, 0, 3, policy)
		if err != nil || got.Kind != "rejected" || got.Reason == "" {
			t.Fatalf("%s: %+v %v", kind, got, err)
		}
	}
}

func TestPolicyRejectsLegacyExecutionAndAmbiguousResults(t *testing.T) {
	for _, version := range []int{-1, 0, 1, 3, 999} {
		if _, err := ParseWithPolicy(`{"accepted":false,"reason":"declined"}`, "explore", false, 0, 3, Policy{Version: version, GraphRPC: true}); err == nil {
			t.Fatalf("accepted version %d", version)
		}
	}
	if _, err := ParseWithPolicy(`{"accepted":false,"reason":"declined"}`, "explore", false, 0, 3, Policy{Version: 2}); err == nil {
		t.Fatal("accepted offline execution")
	}
	for _, tc := range []struct {
		kind, output string
		conclude     bool
	}{
		{"bootstrap", `{"accepted":false,"reason":"declined"}`, false},
		{"reason", `{"accepted":true,"data":{"intents":[{"from":["origin"],"description":"work"}]}}`, false},
		{"reason", `{"accepted":true,"data":{"complete":{"from":["f1"],"description":"done"}}}`, false},
		{"reason", `{"accepted":true,"data":{}}`, false},
		{"reason", `{"accepted":true,"data":{"decided":true}}`, true},
		{"explore", `{"accepted":true,"data":{"description":"done"}}`, false},
		{"explore", `{"accepted":true,"outcome":"completed","data":{"description":"done"}}`, false},
		{"explore", `{"accepted":true,"outcome":"continue","reason":"more"}`, true},
		{"explore", `{"accepted":null}`, false},
		{"explore", `{"accepted":false}`, false},
		{"explore", `{"accepted":false,"reason":" "}`, false},
		{"explore", `{"accepted":false,"reason":"declined","data":{}}`, false},
		{"explore", `{"accepted":true,"outcome":"continue","reason":"more","data":{}}`, false},
		{"explore", `{"accepted":true,"outcome":"incomplete","reason":null}`, false},
		{"explore", `{"accepted":true,"outcome":"unknown","reason":"more"}`, false},
	} {
		if _, err := ParseWithPolicy(tc.output, tc.kind, tc.conclude, 1, 3, Policy{Version: 2, GraphRPC: true}); err == nil {
			t.Fatalf("accepted %s %s", tc.kind, tc.output)
		}
	}
}

func TestPolicyUsesFirstExtractedObject(t *testing.T) {
	policy := Policy{Version: 2, GraphRPC: true}
	valid := `{"accepted":true,"data":{"decided":true}}`
	got, err := ParseWithPolicy("model prose\n"+valid+`{"accepted":false}`, "reason", false, 1, 3, policy)
	if err != nil || got.Kind != "decided" {
		t.Fatalf("first object changed: %+v %v", got, err)
	}
	if _, err := ParseWithPolicy(`{"accepted":null}`+valid, "reason", false, 1, 3, policy); err == nil {
		t.Fatal("skipped invalid first object")
	}
}
