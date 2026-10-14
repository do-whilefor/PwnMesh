package projection

import (
	"bytes"
	"encoding/json"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestMetadataPreservesEncodingAndLegacyStatusBoundary(t *testing.T) {
	absent, err := DecodeMetadata(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(absent)
	want := `{"goals":[],"steps":[],"facts":[],"findings":[],"fact_relations":[],"curation":{"generation":0,"through_revision":0}}`
	if string(raw) != want {
		t.Fatalf("absent metadata changed field order or empty collections: %s", raw)
	}
	if _, err := DecodeMetadata([]byte{}); err == nil {
		t.Fatal("present but empty metadata was accepted as an absent row")
	}
	nulls, err := DecodeMetadata([]byte(`{"goals":null,"steps":null,"facts":null,"findings":null,"fact_relations":null}`))
	if err != nil || nulls.Goals != nil || nulls.Steps != nil || nulls.Facts != nil || nulls.Findings != nil || nulls.FactRelations != nil {
		t.Fatalf("explicit null collections changed: %+v, %v", nulls, err)
	}
	legacy, err := DecodeMetadata([]byte(`{"steps":[{"id":"old","status":"completed","unknown":"retained-format"},{"id":"retired","goal_id":"goal","priority":2,"status":"abandoned","depends_on":["upstream"]}]}`))
	if err != nil || legacy.Steps[0].Status != "" || legacy.Steps[1].Status != "abandoned" {
		t.Fatalf("historical runtime status escaped the loading adapter: %+v, %v", legacy, err)
	}
	raw, _ = json.Marshal(legacy.Steps[1])
	if string(raw) != `{"id":"retired","goal_id":"goal","priority":2,"status":"abandoned","depends_on":["upstream"]}` {
		t.Fatalf("step metadata encoding changed: %s", raw)
	}
}

func TestBuildDoesNotMutatePendingFindingSupport(t *testing.T) {
	g := model.Graph{Project: model.Project{OrchestrationVersion: 1}, Facts: []model.Fact{{ID: "observation"}}}
	d := Metadata{Facts: []model.FactRecord{{ID: "observation", Status: "valid", Evidence: []model.EvidenceRef{{RunID: "run", Path: "evidence.txt"}}}},
		Findings: []model.Finding{{ID: "finding", Sources: []string{"observation"}}}}
	before, _ := json.Marshal(d)
	supported := Build(g, d, 4, 2, nil)
	after, _ := json.Marshal(d)
	if !supported.Findings[0].SupportValid || !bytes.Equal(before, after) {
		t.Fatal("projecting support mutated pending metadata")
	}
	d.FactRelations = []model.FactRelation{{Kind: "refutes", Target: "observation"}}
	before, _ = json.Marshal(d)
	invalidated := Build(g, d, 5, 3, nil)
	after, _ = json.Marshal(d)
	if invalidated.Findings[0].SupportValid || !supported.Findings[0].SupportValid || !bytes.Equal(before, after) {
		t.Fatal("later relation projection changed an earlier view or pending metadata")
	}
	for _, findings := range [][]model.Finding{nil, {}} {
		state := Build(model.Graph{}, Metadata{Findings: findings}, 0, 0, nil)
		if (state.Findings == nil) != (findings == nil) || state.Goals == nil || state.Steps == nil || state.FactRecords == nil {
			t.Fatal("projection changed nil/empty collection semantics")
		}
	}
}

func TestBuildBindsSupportToGenerationWithoutHidingLatestAttempt(t *testing.T) {
	g := model.Graph{Project: model.Project{OrchestrationVersion: 1, Generation: 4}, Facts: []model.Fact{{ID: "origin"}, {ID: "accepted"}},
		Intents: []model.Intent{{ID: "done", From: []string{"origin"}, To: model.Ptr("accepted")}, {ID: "waiting", From: []string{"origin"}}, {ID: "failed", From: []string{"origin"}}}}
	d := Metadata{Steps: []StepMetadata{{ID: "waiting", DependsOn: []string{"done"}}},
		Facts: []model.FactRecord{{ID: "accepted", Status: "valid", SourceStepID: "done", RunID: "accepted-run", Evidence: []model.EvidenceRef{{Path: "accepted.txt"}}}}}
	for _, generation := range []int64{3, 4} {
		runs := []model.ExecutionSummary{
			{ID: "accepted-run", Intent: "done", Lease: "worker@accepted-run", Status: "succeeded", Generation: generation},
			{ID: "old-failure", Intent: "failed", Lease: "worker@old-failure", Status: "failed", Generation: 3},
		}
		state := Build(g, d, 5, 2, runs)
		valid := generation == 4
		if state.Steps[0].Status != "completed" || state.Steps[0].SupportValid != valid || state.FactRecords[1].SupportInvalid == valid {
			t.Fatalf("generation %d changed historical success or authorized stale support: %+v", generation, state.Steps[0])
		}
		if (state.Steps[1].Status == "blocked") == valid {
			t.Fatalf("generation %d did not govern dependency readiness", generation)
		}
		if state.Steps[2].Status != "failed" || state.Steps[2].LatestRunID != "old-failure" || state.Steps[2].Reason != "execution failed" {
			t.Fatal("generation filtering hid the latest diagnostic attempt")
		}
	}
}

func TestWithAssetsDoesNotAppendIntoCallerAnchors(t *testing.T) {
	backing := []model.AssetAnchor{{AssetID: "asset", NodeKind: "fact", NodeID: "source"}, {NodeKind: "sentinel", NodeID: "untouched"}}
	sentinel := backing[1]
	state := model.State{Findings: []model.Finding{{ID: "finding", Sources: []string{"source"}}}}
	projected := WithAssets(state, []model.Asset{{ID: "asset"}}, backing[:1])
	if backing[1] != sentinel || state.Assets != nil || state.AssetAnchors != nil {
		t.Fatal("asset projection mutated caller-owned state or spare anchor capacity")
	}
	if len(projected.AssetAnchors) != 2 || projected.AssetAnchors[1] != (model.AssetAnchor{AssetID: "asset", NodeKind: "finding", NodeID: "finding"}) {
		t.Fatalf("finding association lost or reordered its source anchor: %+v", projected.AssetAnchors)
	}
}
