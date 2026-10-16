//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"testing"

	"pwnmesh/internal/board/model"
	"pwnmesh/internal/protocol"
)

func TestGraphReadArgumentsPreserveAssetNeighborhood(t *testing.T) {
	request, err := graphReadArguments(json.RawMessage(`{"section":"facts","asset_ids":["asset"],"asset_neighborhood":true,"limit":1}`), "read_graph")
	if err != nil || !request.AssetNeighborhood || len(request.AssetIDs) != 1 || request.AssetIDs[0] != "asset" {
		t.Fatalf("neighborhood query lost before bridge: %+v %v", request, err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(graphReadSchema(), &schema) != nil || len(schema.Properties["asset_neighborhood"]) == 0 {
		t.Fatal("neighborhood option is absent from read tool schema")
	}
}

func TestGraphReadExposesUsableUnstoredAssetParentSelectors(t *testing.T) {
	asset, err := model.NormalizeAsset(model.AssetSpec{Kind: "endpoint", Value: "https://example.test/path", Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	state := model.State{Assets: []model.Asset{asset}, FactRecords: []model.FactRecord{{ID: "fact", Status: "valid"}},
		AssetAnchors: []model.AssetAnchor{{AssetID: asset.ID, NodeKind: "fact", NodeID: "fact"}}}
	request := func(_ context.Context, r GraphRequest) (string, error) {
		page, err := protocol.GraphPage(state, r)
		if err != nil {
			return "", err
		}
		raw, err := json.Marshal(page)
		return string(raw), err
	}
	raw, err := modelGraphRead(context.Background(), GraphRequest{Section: "assets"}, request)
	var assets struct {
		Items []struct {
			AncestorIDs map[string]string `json:"ancestor_ids"`
		} `json:"items"`
	}
	if err != nil || json.Unmarshal([]byte(raw), &assets) != nil || len(assets.Items) != 1 {
		t.Fatalf("asset discovery failed: %s %v", raw, err)
	}
	for _, kind := range []string{"host", "service"} {
		id := assets.Items[0].AncestorIDs[kind]
		if id == "" {
			t.Fatalf("model read lost the %s selector", kind)
		}
		raw, err = modelGraphRead(context.Background(), GraphRequest{Section: "facts", AssetIDs: []string{id}, AssetNeighborhood: true}, request)
		var facts protocol.GraphItemsPage
		if err != nil || json.Unmarshal([]byte(raw), &facts) != nil || facts.Total != 1 || facts.StateVersion != model.DecisionStateVersion(state) {
			t.Fatalf("model could not use its %s selector: %s %v", kind, raw, err)
		}
	}
}
