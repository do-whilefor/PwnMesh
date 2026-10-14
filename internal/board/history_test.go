package board

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func historyTestState(t *testing.T) State {
	t.Helper()
	raw, err := os.ReadFile("model/testdata/history.json")
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestHistoryFoldsAbandonedWorkAndKeepsMissingProducerFactVisible(t *testing.T) {
	state := historyTestState(t)
	state.Steps[0].Status = "abandoned"
	state.FactRecords[3].SourceStepID = "missing"
	history := state.History()
	if len(history) != 1 || history[0].Status != "abandoned" || len(history[0].Members) != 2 {
		t.Fatalf("unexpected abandoned group: %+v", history)
	}
	view, err := ContextView(state, "", DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	facts := overviewSection(t, decodeOverview(t, view), "facts")
	if !slices.ContainsFunc(facts.Items, func(node contextOverviewNode) bool { return node.ID == "side-observation" }) {
		t.Fatal("observation with missing producer was hidden")
	}
}

func TestHistoryAndAssetsDiscoverySurviveIncrementalDecision(t *testing.T) {
	state := historyTestState(t)
	state.Steps = append(state.Steps, Step{ID: "current", GoalID: "goal", Status: "running", From: []string{"origin"}})
	state.AssetAnchors = append(state.AssetAnchors, AssetAnchor{AssetID: "asset-a", NodeKind: "step", NodeID: "current"})
	baseline, err := ContextView(state, "", DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	overview := decodeOverview(t, baseline)
	steps, facts, history := overviewSection(t, overview, "steps"), overviewSection(t, overview, "facts"), overviewSection(t, overview, "history")
	if steps.Folded != 1 || facts.Folded != 2 || len(history.Items) != 1 || history.Items[0].ID != "old" || len(history.Items[0].Members) != 3 || !history.Items[0].TextTruncated {
		t.Fatalf("folded discovery=%+v / %+v / %+v", steps, facts, history)
	}
	if len(steps.Items) != 1 || steps.Items[0].ID != "current" || !slices.Equal(steps.Items[0].AssetIDs, []string{"asset-a"}) {
		t.Fatalf("current asset anchor not discoverable: %+v", steps)
	}
	assets, anchors := overviewSection(t, overview, "assets"), overviewSection(t, overview, "anchors")
	if assets.Total != 2 || assets.Items[0].ID != "asset-a" || anchors.Total != 4 || !strings.Contains(overview.ReadMore, "asset_ids") || !strings.Contains(overview.ReadMore, "expand members") {
		t.Fatalf("asset lookup or expansion is unavailable: %+v", overview)
	}
	cursor := &DecisionCursor{ProjectID: "project", Generation: 1, Revision: 1}
	decision, err := BuildDecisionContextFromCursor(state, cursor, nil, DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != "changes" || !reflect.DeepEqual(overview, decodeOverview(t, decision.View)) {
		t.Fatalf("incremental discovery differs from frozen baseline: mode=%s", decision.Mode)
	}
}

func TestHistoryDiscoveryIsBoundedAndOriginalRecordsRemainAvailable(t *testing.T) {
	state := historyTestState(t)
	for i := 0; i < 100; i++ {
		id, factID, assetID := fmt.Sprintf("old-%d", i), fmt.Sprintf("fact-%d", i), fmt.Sprintf("asset-%d", i)
		state.Steps = append(state.Steps, Step{ID: id, GoalID: "goal", Status: "completed", Description: strings.Repeat("long history ", 100), Result: &factID, SupportValid: true})
		state.FactRecords = append(state.FactRecords, FactRecord{ID: factID, Description: strings.Repeat("original observation ", 100), SourceStepID: id, Status: "valid"})
		state.Assets = append(state.Assets, Asset{ID: assetID, Kind: "host", Value: fmt.Sprintf("%d.example.test", i)})
		state.AssetAnchors = append(state.AssetAnchors, AssetAnchor{NodeKind: "step", NodeID: id, AssetID: assetID})
	}
	before, _ := json.Marshal(state)
	for _, budget := range []int{4096, 8192, 16384} {
		view, err := ContextView(state, "", budget)
		if err != nil || len(view) > budget {
			t.Fatalf("budget=%d size=%d error=%v", budget, len(view), err)
		}
		overview := decodeOverview(t, view)
		for _, section := range overview.Sections {
			if section.Omitted != section.Total-len(section.Items) {
				t.Fatalf("omission count=%+v", section)
			}
			if section.Section == "assets" && len(section.Items) > 8 {
				t.Fatal("entire asset catalog was injected")
			}
		}
	}
	// Exact small index budgets cannot be exceeded by folded-count metadata.
	for budget := 900; budget < 2400; budget += 17 {
		overview, err := buildContextOverview(state, budget, nil, "", nil)
		if err != nil {
			continue
		}
		raw, _ := json.Marshal(overview)
		if len(raw) > budget {
			t.Fatalf("overview budget=%d size=%d", budget, len(raw))
		}
	}
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Fatal("reading views changed original records")
	}
	if len(state.History()) != 101 || len(state.Steps) != 101 || len(state.FactRecords) != 104 {
		t.Fatal("folding discarded source records")
	}
}
