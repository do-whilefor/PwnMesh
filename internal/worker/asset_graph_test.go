package worker

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

func assetGraphFixture(t *testing.T) board.State {
	t.Helper()
	a, err := board.NormalizeAsset(board.AssetSpec{Kind: "endpoint", Value: "https://EXAMPLE.test:443/a", Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := board.NormalizeAsset(board.AssetSpec{Kind: "endpoint", Value: "https://example.test/a", Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	return board.State{
		Graph:  board.Graph{Project: board.Project{ID: "p", Generation: 3}},
		Assets: []board.Asset{a, b},
		AssetAnchors: []board.AssetAnchor{
			{AssetID: a.ID, NodeKind: "fact", NodeID: "old"},
			{AssetID: a.ID, NodeKind: "fact", NodeID: "new"},
			{AssetID: b.ID, NodeKind: "fact", NodeID: "new"},
			{AssetID: b.ID, NodeKind: "step", NodeID: "work"},
		},
		FactRecords: []board.FactRecord{
			{ID: "old", Description: "An earlier session returned 200", Status: "refuted", SupportInvalid: true},
			{ID: "new", Description: "A fresh anonymous session returned 403", Status: "valid"},
			{ID: "other", Description: "Unrelated observation", Status: "valid"},
		},
		Steps: []board.Step{{ID: "work", Description: "Check the POST method", Status: "open"}},
	}
}

func TestAssetGraphFilterPreservesContradictoryObservationsAndPagination(t *testing.T) {
	s := assetGraphFixture(t)
	before, _ := json.Marshal(s)
	r := GraphRequest{Section: "facts", AssetIDs: []string{s.Assets[0].ID}, Limit: 1}
	first := checkedGraphPage(t, s, r)
	if first.Total != 2 || len(first.Items) != 1 || first.NextOffset == nil {
		t.Fatalf("lost related history: %+v", first)
	}
	var fact board.FactRecord
	if err := json.Unmarshal(first.Items[0], &fact); err != nil {
		t.Fatal(err)
	}
	if fact.ID != "old" || fact.Status != "refuted" || !fact.SupportInvalid {
		t.Fatalf("asset promoted observation: %+v", fact)
	}
	r.Offset, r.ExpectedVersion = *first.NextOffset, first.StateVersion
	second := checkedGraphPage(t, s, r)
	if len(second.Items) != 1 || second.NextOffset != nil {
		t.Fatalf("bad continuation: %+v", second)
	}
	r.AssetIDs = []string{s.Assets[0].ID, s.Assets[1].ID}
	r.Offset, r.Limit = 0, 20
	union := checkedGraphPage(t, s, r)
	if union.Total != 2 {
		t.Fatalf("multi-asset observation duplicated: %+v", union)
	}
	r.IDs = []string{"other"}
	intersection := checkedGraphPage(t, s, r)
	if intersection.Total != 0 || !reflect.DeepEqual(intersection.MissingIDs, []string{"other"}) {
		t.Fatalf("IDs bypassed asset filter: %+v", intersection)
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("read mutated the original State")
	}
}

func TestAssetGraphFrozenViewsBindCatalogueAndAnchors(t *testing.T) {
	s := assetGraphFixture(t)
	raw, _ := json.Marshal(s)
	var frozen board.State
	if err := json.Unmarshal(raw, &frozen); err != nil {
		t.Fatal(err)
	}
	r := GraphRequest{Section: "anchors", IDs: []string{"new"}, AssetIDs: []string{s.Assets[0].ID}, ExpectedVersion: board.DecisionStateVersion(frozen)}
	page := checkedGraphPage(t, frozen, r)
	if page.Total != 1 {
		t.Fatalf("anchor filters not combined: %+v", page)
	}
	s.AssetAnchors = append(s.AssetAnchors, board.AssetAnchor{AssetID: s.Assets[0].ID, NodeKind: "fact", NodeID: "other"})
	if _, err := GraphPage(s, r); err == nil || !strings.HasPrefix(err.Error(), "state_changed:") {
		t.Fatalf("changed anchors accepted under old version: %v", err)
	}
	if page := checkedGraphPage(t, frozen, r); page.Total != 1 {
		t.Fatal("frozen catalogue drifted")
	}
	r.AssetIDs = []string{"unknown"}
	if _, err := GraphPage(frozen, r); err == nil {
		t.Fatal("unknown asset silently appeared to have no observations")
	}
}

func TestAssetGraphOverviewCountsMatchVersionedSections(t *testing.T) {
	live := assetGraphFixture(t)
	live.Steps = append(live.Steps, board.Step{ID: "retired", Status: "abandoned"})
	live.AssetAnchors = append(live.AssetAnchors, board.AssetAnchor{AssetID: live.Assets[0].ID, NodeKind: "step", NodeID: "retired"})
	raw, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	var frozen board.State
	if err := json.Unmarshal(raw, &frozen); err != nil {
		t.Fatal(err)
	}
	asset, err := board.NormalizeAsset(board.AssetSpec{Kind: "host", Value: "later.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	live.Assets = append(live.Assets, asset)
	live.Steps = append(live.Steps, board.Step{ID: "later-retired", Status: "abandoned"})
	live.AssetAnchors = append(live.AssetAnchors, board.AssetAnchor{AssetID: asset.ID, NodeKind: "step", NodeID: "later-retired"})
	for _, tc := range []struct {
		name   string
		op     string
		state  board.State
		counts map[string]int
	}{
		{"frozen", "read_snapshot", frozen, map[string]int{"assets": 2, "anchors": 5, "history": 1}},
		{"live", "read_graph", live, map[string]int{"assets": 3, "anchors": 6, "history": 2}},
		{"legacy-empty", "read_graph", board.State{Graph: board.Graph{Project: board.Project{ID: "legacy"}}}, map[string]int{"assets": 0, "anchors": 0, "history": 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version := board.DecisionStateVersion(tc.state)
			value, err := GraphPage(tc.state, GraphRequest{Op: tc.op, Section: "overview", ExpectedVersion: version})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var overview struct {
				StateVersion string         `json:"state_version"`
				Counts       map[string]int `json:"counts"`
			}
			if err := json.Unmarshal(raw, &overview); err != nil || overview.StateVersion != version {
				t.Fatalf("overview lost its version: %s, %v", raw, err)
			}
			for section, want := range tc.counts {
				count, exists := overview.Counts[section]
				if !exists || count != want {
					t.Fatalf("overview %s count=%d present=%v, want %d", section, count, exists, want)
				}
				request := GraphRequest{Op: tc.op, Section: section, ExpectedVersion: version, Limit: 1}
				collected := 0
				for {
					page := checkedGraphPage(t, tc.state, request)
					if page.Total != count {
						t.Fatalf("%s total=%d differs from overview count=%d", section, page.Total, count)
					}
					collected += len(page.Items)
					if page.NextOffset == nil {
						break
					}
					request.Offset = *page.NextOffset
				}
				if collected != count {
					t.Fatalf("%s yielded %d records, want %d", section, collected, count)
				}
			}
		})
	}
	if _, err := GraphPage(live, GraphRequest{Section: "overview", ExpectedVersion: board.DecisionStateVersion(frozen)}); err == nil || !strings.HasPrefix(err.Error(), "state_changed:") {
		t.Fatalf("overview accepted the previous asset/history version: %v", err)
	}
}

func TestAssetGraphRejectsFiltersOnUnrelatedOperations(t *testing.T) {
	for _, r := range []GraphRequest{
		{Op: "read_graph", Section: "overview", AssetIDs: []string{"a"}},
		{Op: "read_graph", Section: "evidence", IDs: []string{"f"}, AssetIDs: []string{"a"}},
		{Op: "read_updates", AssetIDs: []string{"a"}},
		{Op: "graph_action", AssetIDs: []string{"a"}},
		{Op: "read_graph", Section: "assets", AssetIDs: []string{"a", "a"}},
		{Op: "read_graph", Section: "assets", AssetIDs: []string{" "}},
	} {
		r.RequestID = strings.Repeat("a", 32)
		if err := ValidateGraphRequest(Job{}, r); err == nil {
			t.Fatalf("accepted ignored asset filter: %+v", r)
		}
	}
}

func TestCompactGraphReceiptRetainsAssetLookup(t *testing.T) {
	result := board.StateActionResult{Op: "fact", ID: "f001", Revision: 3, StateVersion: "version", Committed: true, AssetIDs: []string{"a001", "a002"}}
	result.Result, _ = json.Marshal(map[string]string{"description": strings.Repeat("observation", maxGraphPageBytes)})
	raw, _ := json.Marshal(result)
	compact, err := CompactGraphActionResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	var receipt board.StateActionResult
	if err := json.Unmarshal(compact, &receipt); err != nil || len(compact) >= maxGraphPageBytes || receipt.ID != result.ID || !receipt.Committed || !reflect.DeepEqual(receipt.AssetIDs, result.AssetIDs) {
		t.Fatalf("large successful write lost its asset lookup: %s, %v", compact, err)
	}
}

func TestWorkerTraceRunProtocolRequiresRegisteredExecuteScope(t *testing.T) {
	j := Job{Kind: "explore", RunID: "self", GraphRPC: true, Intent: &board.Intent{ID: "work"}, Graph: board.Graph{Project: board.Project{ID: "p"}}}
	r := GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_trace_runs", IDs: []string{"other"}, Limit: 10}
	if err := ValidateGraphRequest(j, r); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Job, *GraphRequest){
		"own run":         func(_ *Job, r *GraphRequest) { r.IDs = []string{"self"} },
		"curator":         func(j *Job, _ *GraphRequest) { j.Kind = "curate" },
		"offline":         func(j *Job, _ *GraphRequest) { j.GraphRPC = false },
		"multiple":        func(_ *Job, r *GraphRequest) { r.IDs = []string{"one", "two"} },
		"negative offset": func(_ *Job, r *GraphRequest) { r.Offset = -1 },
		"cursor":          func(_ *Job, r *GraphRequest) { r.ExpectedVersion = "unrelated" },
		"mutation":        func(_ *Job, r *GraphRequest) { r.Action.Op = "fact" },
		"asset filter":    func(_ *Job, r *GraphRequest) { r.AssetIDs = []string{"a"} },
	} {
		t.Run(name, func(t *testing.T) {
			job, req := j, r
			change(&job, &req)
			if err := ValidateGraphRequest(job, req); err == nil {
				t.Fatal("accepted an unauthorized trace request")
			}
		})
	}
}
