package protocol_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"pwnmesh/internal/board/model"
	"pwnmesh/internal/protocol"
)

func assetNeighborhoodState(t *testing.T) model.State {
	t.Helper()
	s := model.State{Graph: model.Graph{Project: model.Project{ID: "project", Generation: 3}}}
	for _, spec := range []model.AssetSpec{
		{Kind: "host", Value: "EXAMPLE.test."},
		{Kind: "service", Value: "https://example.test:443/"},
		{Kind: "service", Value: "http://example.test"},
		{Kind: "endpoint", Value: "https://example.test/path?a=1&b=2", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.test/path?a=1&b=2", Method: "POST"},
		{Kind: "service", Value: "tcp://example.test:22"},
		{Kind: "host", Value: "different.test"},
	} {
		asset, err := model.NormalizeAsset(spec)
		if err != nil {
			t.Fatal(err)
		}
		s.Assets = append(s.Assets, asset)
	}
	return s
}

func neighborhoodPage(t *testing.T, s model.State, request protocol.GraphRequest) protocol.GraphItemsPage {
	t.Helper()
	value, err := protocol.GraphPage(s, request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var page protocol.GraphItemsPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestAssetNeighborhoodIsOptInAndDoesNotExpandThroughSiblings(t *testing.T) {
	s := assetNeighborhoodState(t)
	before, _ := json.Marshal(s)
	for _, tc := range []struct {
		name         string
		selected     int
		neighborhood bool
		want         []int
	}{
		{"exact-host", 0, false, []int{0}},
		{"exact-endpoint", 3, false, []int{3}},
		{"host-descendants", 0, true, []int{0, 1, 2, 3, 4, 5}},
		{"service-parent-and-descendants", 1, true, []int{0, 1, 3, 4}},
		{"endpoint-ancestors", 3, true, []int{0, 1, 3}},
		{"tcp-parent", 5, true, []int{0, 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := protocol.GraphRequest{Section: "assets", AssetIDs: []string{s.Assets[tc.selected].ID}, AssetNeighborhood: tc.neighborhood, Limit: 1}
			var got []string
			for {
				page := neighborhoodPage(t, s, request)
				for _, raw := range page.Items {
					var asset model.Asset
					if err := json.Unmarshal(raw, &asset); err != nil {
						t.Fatal(err)
					}
					got = append(got, asset.ID)
				}
				if page.NextOffset == nil {
					break
				}
				request.Offset, request.ExpectedVersion = *page.NextOffset, page.StateVersion
			}
			var want []string
			for _, i := range tc.want {
				want = append(want, s.Assets[i].ID)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("neighborhood lookup changed exact identities")
	}
	endpointOnly := model.State{Graph: s.Graph, Assets: []model.Asset{s.Assets[3]}}
	page := neighborhoodPage(t, endpointOnly, protocol.GraphRequest{Section: "assets", AssetIDs: []string{s.Assets[3].ID}, AssetNeighborhood: true})
	if page.Total != 1 {
		t.Fatal("neighborhood lookup invented unobserved host or service catalog entries")
	}
}

func TestAssetNeighborhoodUsesBoundSnapshotAndRetainsDiscoveryProvenance(t *testing.T) {
	s := assetNeighborhoodState(t)
	s.FactRecords = []model.FactRecord{{ID: "result", Status: "refuted", SupportInvalid: true}}
	s.AssetAnchors = []model.AssetAnchor{{AssetID: s.Assets[3].ID, NodeKind: "fact", NodeID: "result", DiscoveryStepID: "producer"}}
	raw, _ := json.Marshal(s)
	var frozen model.State
	if err := json.Unmarshal(raw, &frozen); err != nil {
		t.Fatal(err)
	}
	r := protocol.GraphRequest{Section: "anchors", AssetIDs: []string{s.Assets[0].ID}, AssetNeighborhood: true, ExpectedVersion: model.DecisionStateVersion(frozen)}
	page := neighborhoodPage(t, frozen, r)
	var anchor model.AssetAnchor
	if len(page.Items) != 1 || json.Unmarshal(page.Items[0], &anchor) != nil || anchor != frozen.AssetAnchors[0] {
		t.Fatalf("lost discovery provenance: %+v", page)
	}
	r.Section = "facts"
	page = neighborhoodPage(t, frozen, r)
	var fact model.FactRecord
	if len(page.Items) != 1 || json.Unmarshal(page.Items[0], &fact) != nil || !reflect.DeepEqual(fact, s.FactRecords[0]) {
		t.Fatal("asset lookup changed evidence validity")
	}
	s.AssetAnchors = nil
	if _, err := protocol.GraphPage(s, r); err == nil {
		t.Fatal("live asset drift accepted an old state version")
	}
	if neighborhoodPage(t, frozen, r).Total != 1 {
		t.Fatal("live changes altered frozen discovery")
	}
	for _, invalid := range []protocol.GraphRequest{
		{Section: "assets", AssetNeighborhood: true},
		{Section: "relations", AssetIDs: []string{s.Assets[0].ID}, AssetNeighborhood: true},
		{Section: "assets", AssetIDs: []string{"absent"}, AssetNeighborhood: true},
	} {
		if _, err := protocol.GraphPage(frozen, invalid); err == nil {
			t.Fatalf("invalid neighborhood accepted: %+v", invalid)
		}
	}
}

func TestOptionalAssetDiscoveryFieldsPreserveExistingWire(t *testing.T) {
	anchor, _ := json.Marshal(model.AssetAnchor{AssetID: "a", NodeKind: "fact", NodeID: "f"})
	if string(anchor) != `{"asset_id":"a","node_kind":"fact","node_id":"f"}` {
		t.Fatalf("explicit anchor wire changed: %s", anchor)
	}
	request, _ := json.Marshal(protocol.GraphRequest{RequestID: "id", Op: "read_graph", Section: "assets", AssetIDs: []string{"a"}})
	if string(request) != `{"request_id":"id","op":"read_graph","section":"assets","asset_ids":["a"],"action":{"op":"","idempotency_key":"","payload":null}}` {
		t.Fatalf("exact query wire changed: %s", request)
	}
}
