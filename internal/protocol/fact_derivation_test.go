package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestFactDerivationBoundedSourcesAndPublication(t *testing.T) {
	fact := model.FactRecord{ID: "derived", Status: "valid", DerivedFrom: []string{"first", "second"}, Evidence: []model.EvidenceRef{{Path: "large.txt", Excerpt: strings.Repeat("evidence ", MaxGraphPageBytes)}}}
	state := model.State{FactRecords: []model.FactRecord{fact}}
	var got []string
	for offset := 0; offset < 2; offset++ {
		response, err := GraphPage(state, GraphRequest{Section: "sources", IDs: []string{fact.ID}, Offset: offset, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(response)
		var page struct {
			Items []string `json:"items"`
			Total int      `json:"total"`
		}
		if json.Unmarshal(raw, &page) != nil || page.Total != 2 || len(page.Items) != 1 {
			t.Fatalf("bounded Fact sources lost ancestry: %s", raw)
		}
		got = append(got, page.Items...)
	}
	if !reflect.DeepEqual(got, fact.DerivedFrom) {
		t.Fatalf("Fact sources changed: %v", got)
	}
	raw, _ := json.Marshal(fact)
	compact, err := compactGraphRecord("facts", raw)
	if err != nil {
		t.Fatal(err)
	}
	var restored model.FactRecord
	if json.Unmarshal(compact, &restored) != nil || !reflect.DeepEqual(restored.DerivedFrom, fact.DerivedFrom) {
		t.Fatal("bounded record omitted derivation identities")
	}
}
