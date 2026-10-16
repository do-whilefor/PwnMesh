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

func TestAssetNeighborhoodSupportsUnstoredParentSelectors(t *testing.T) {
	catalog := assetNeighborhoodState(t)
	s := model.State{Graph: catalog.Graph, Assets: []model.Asset{catalog.Assets[3], catalog.Assets[4]}}
	s.FactRecords = []model.FactRecord{{ID: "observed", Status: "valid"}, {ID: "discovered", Status: "refuted", SupportInvalid: true}}
	s.AssetAnchors = []model.AssetAnchor{
		{AssetID: s.Assets[0].ID, NodeKind: "fact", NodeID: "observed"},
		{AssetID: s.Assets[1].ID, NodeKind: "fact", NodeID: "discovered", DiscoveryStepID: "producer"},
	}
	before, _ := json.Marshal(s)
	version := model.DecisionStateVersion(s)
	discovery := neighborhoodPage(t, s, protocol.GraphRequest{Section: "assets", IDs: []string{s.Assets[0].ID}})
	var endpoint struct {
		model.Asset
		AncestorIDs map[string]string `json:"ancestor_ids"`
	}
	if len(discovery.Items) != 1 || json.Unmarshal(discovery.Items[0], &endpoint) != nil || endpoint.Asset != s.Assets[0] {
		t.Fatal("asset read lost the original endpoint identity")
	}
	for _, parent := range catalog.Assets[:2] {
		t.Run(parent.Kind, func(t *testing.T) {
			selector := endpoint.AncestorIDs[parent.Kind]
			if selector != parent.ID {
				t.Fatal("asset read omitted its normalized parent selector")
			}
			request := protocol.GraphRequest{Section: "assets", AssetIDs: []string{selector}, ExpectedVersion: version}
			if _, err := protocol.GraphPage(s, request); err == nil {
				t.Fatal("an exact lookup accepted an unstored parent")
			}
			request.AssetNeighborhood, request.Limit = true, 1
			first := neighborhoodPage(t, s, request)
			if first.Total != 2 || len(first.Items) != 1 || first.NextOffset == nil {
				t.Fatalf("parent selector did not find bound descendants: %+v", first)
			}
			request.Offset = *first.NextOffset
			second := neighborhoodPage(t, s, request)
			if second.Total != 2 || len(second.Items) != 1 || second.NextOffset != nil {
				t.Fatal("parent selector pagination lost descendants")
			}
			for i, raw := range append(first.Items, second.Items...) {
				var asset model.Asset
				if json.Unmarshal(raw, &asset) != nil || asset != s.Assets[i] {
					t.Fatal("parent selector fabricated or changed a catalog asset")
				}
			}
			request.Section, request.Offset, request.Limit = "anchors", 0, 20
			anchors := neighborhoodPage(t, s, request)
			if anchors.Total != 2 || len(anchors.Items) != 2 {
				t.Fatal("parent selector lost bound anchors")
			}
			for i, raw := range anchors.Items {
				var anchor model.AssetAnchor
				if json.Unmarshal(raw, &anchor) != nil || anchor != s.AssetAnchors[i] {
					t.Fatal("parent selector changed observed/discovery provenance")
				}
			}
			request.Section = "facts"
			facts := neighborhoodPage(t, s, request)
			if facts.Total != 2 {
				t.Fatal("parent selector lost related facts")
			}
			var fact model.FactRecord
			if json.Unmarshal(facts.Items[1], &fact) != nil || !reflect.DeepEqual(fact, s.FactRecords[1]) {
				t.Fatal("parent selector changed evidence validity")
			}
			unrelated := model.State{Graph: s.Graph, Assets: []model.Asset{catalog.Assets[6]}}
			if _, err := protocol.GraphPage(unrelated, request); err == nil {
				t.Fatal("parent selector accepted another snapshot's version")
			}
			request.ExpectedVersion = ""
			if _, err := protocol.GraphPage(unrelated, request); err == nil {
				t.Fatal("parent selector escaped its supporting catalog")
			}
		})
	}
	// Starting from an endpoint still does not expand through virtual parents
	// to its sibling, and a missing service with another scheme is not inferred.
	page := neighborhoodPage(t, s, protocol.GraphRequest{Section: "assets", AssetIDs: []string{s.Assets[0].ID}, AssetNeighborhood: true})
	if page.Total != 1 {
		t.Fatal("endpoint lookup expanded to a sibling through its virtual parent")
	}
	if _, err := protocol.GraphPage(s, protocol.GraphRequest{Section: "assets", AssetIDs: []string{catalog.Assets[2].ID}, AssetNeighborhood: true}); err == nil {
		t.Fatal("parent lookup inferred an unrelated service")
	}
	mixed := model.State{Graph: s.Graph, Assets: []model.Asset{catalog.Assets[0], catalog.Assets[2], s.Assets[0]}}
	page = neighborhoodPage(t, mixed, protocol.GraphRequest{Section: "assets", AssetIDs: []string{endpoint.AncestorIDs["service"]}, AssetNeighborhood: true})
	if page.Total != 2 {
		t.Fatal("virtual service lookup lost its existing host or included a sibling service")
	}
	for i, want := range []model.Asset{mixed.Assets[0], mixed.Assets[2]} {
		var got model.Asset
		if json.Unmarshal(page.Items[i], &got) != nil || got != want {
			t.Fatal("virtual service lookup returned an unrelated identity")
		}
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) || model.DecisionStateVersion(s) != version {
		t.Fatal("parent selector mutated the input state")
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
