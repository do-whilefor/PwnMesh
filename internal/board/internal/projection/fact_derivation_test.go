package projection

import (
	"encoding/json"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestFactDerivationMalformedHistoryFailsClosed(t *testing.T) {
	for _, sources := range []string{"missing", "cycle"} {
		t.Run(sources, func(t *testing.T) {
			g := model.Graph{Project: model.Project{OrchestrationVersion: 1}, Facts: []model.Fact{{ID: "first"}, {ID: "second"}, {ID: "raw"}}}
			d := Metadata{Facts: []model.FactRecord{
				{ID: "first", Status: "valid", Evidence: []model.EvidenceRef{{Path: "first.txt"}}, DerivedFrom: []string{"second"}},
				{ID: "second", Status: "valid", Evidence: []model.EvidenceRef{{Path: "second.txt"}}, DerivedFrom: []string{sources}},
				{ID: "raw", Status: "valid", Evidence: []model.EvidenceRef{{Path: "raw.txt"}}},
			}}
			if sources == "cycle" {
				d.Facts[1].DerivedFrom = []string{"first"}
			}
			before, _ := json.Marshal(d)
			state := Build(g, d, 0, 0, nil)
			after, _ := json.Marshal(d)
			if string(before) != string(after) || !state.FactRecords[0].SupportInvalid || !state.FactRecords[1].SupportInvalid || state.FactRecords[2].SupportInvalid {
				t.Fatalf("malformed derivation changed history or retained authority: %+v", state.FactRecords)
			}
		})
	}
}
