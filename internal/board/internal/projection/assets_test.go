package projection

import (
	"encoding/json"
	"reflect"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestStepResultAssetDiscoveryPreservesObservedAssociations(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted", true: "different-explicit"}[explicit], func(t *testing.T) {
			result := "result"
			s := model.State{
				Steps:       []model.Step{{ID: "step", Result: &result, Status: "completed", From: []string{"origin"}, SupportValid: true}},
				FactRecords: []model.FactRecord{{ID: result, SourceStepID: "step", Status: "valid"}, {ID: "intermediate", SourceStepID: "step", Status: "valid"}},
				Findings:    []model.Finding{{ID: "finding", Sources: []string{result}, Status: "verified", SupportValid: true}},
			}
			assets := []model.Asset{{ID: "target", Kind: "host", Value: "target.test"}, {ID: "observed", Kind: "host", Value: "observed.test"}}
			anchors := []model.AssetAnchor{{AssetID: "target", NodeKind: "step", NodeID: "step"}}
			if explicit {
				anchors = append(anchors, model.AssetAnchor{AssetID: "observed", NodeKind: "fact", NodeID: result})
			}
			before, _ := json.Marshal(s)
			stored, _ := json.Marshal(anchors)
			got := WithAssets(s, assets, anchors)
			wantID := "target"
			if explicit {
				wantID = "observed"
			}
			if !reflect.DeepEqual(got.AssetIDs("fact", result), []string{wantID}) || len(got.AssetIDs("fact", "intermediate")) != 0 {
				t.Fatalf("result discovery changed explicit associations or attached intermediate observations: %+v", got.AssetAnchors)
			}
			for _, anchor := range got.AssetAnchors {
				wantStep := "step"
				if explicit {
					wantStep = ""
				}
				if anchor.NodeKind == "fact" && anchor.NodeID == result && anchor.DiscoveryStepID != wantStep {
					t.Fatalf("missing or invented discovery provenance: %+v", anchor)
				}
			}
			if explicit && !reflect.DeepEqual(got.AssetIDs("finding", "finding"), []string{"observed"}) || !explicit && len(got.AssetIDs("finding", "finding")) != 0 {
				t.Fatal("discovery association was promoted into a finding's observed support")
			}
			if !reflect.DeepEqual(got.Steps, s.Steps) || !reflect.DeepEqual(got.FactRecords, s.FactRecords) || !reflect.DeepEqual(got.Findings, s.Findings) {
				t.Fatal("discovery changed dependencies or evidence validity")
			}
			after, _ := json.Marshal(s)
			storedAfter, _ := json.Marshal(anchors)
			if string(before) != string(after) || string(stored) != string(storedAfter) {
				t.Fatal("projection mutated its source state or stored anchors")
			}
		})
	}
}

func TestStepResultAssetDiscoveryRequiresProducingStep(t *testing.T) {
	result := "result"
	s := model.State{Steps: []model.Step{{ID: "step", Result: &result}}, FactRecords: []model.FactRecord{{ID: result, SourceStepID: "another-step"}}}
	got := WithAssets(s, []model.Asset{{ID: "asset"}}, []model.AssetAnchor{{AssetID: "asset", NodeKind: "step", NodeID: "step"}})
	if len(got.AssetIDs("fact", result)) != 0 {
		t.Fatal("a different Step's observation acquired discovery provenance")
	}
}
