//go:build linux

package worker

import (
	"encoding/json"
	"testing"
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
