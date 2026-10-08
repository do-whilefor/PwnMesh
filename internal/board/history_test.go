package board

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func historyTestState() State {
	result := "observation"
	return State{
		Graph: Graph{Project: Project{ID: "project", Status: "active", Generation: 1, OrchestrationVersion: 1}, Facts: []Fact{{ID: "origin", Description: "Only inspect the agreed scope."}, {ID: "goal", Description: "Explain observations."}}},
		Goals: []Goal{{ID: "goal", Status: "open", Condition: "Explain observations."}},
		Steps: []Step{{ID: "old", GoalID: "goal", Status: "completed", Description: strings.Repeat("检查历史。", 100), From: []string{"origin"}, Result: &result, SupportValid: true}},
		FactRecords: []FactRecord{
			{ID: "origin", Description: "Only inspect the agreed scope.", Status: "input"},
			{ID: "goal", Description: "Explain observations.", Status: "input"},
			{ID: result, Description: strings.Repeat("匿名身份返回403。", 40), Scope: strings.Repeat("https://example.test/path/", 10), ObservedAt: "2026-10-08T08:00:00Z", Status: "valid", SourceStepID: "old"},
			{ID: "side-observation", Description: "Logged-in user received 200.", Status: "valid", SourceStepID: "old"},
		},
		Assets: []Asset{{ID: "asset-b", Kind: "host", Value: "b.example.test"}, {ID: "asset-a", Kind: "host", Value: "a.example.test"}},
		AssetAnchors: []AssetAnchor{
			{NodeKind: "step", NodeID: "old", AssetID: "asset-b"},
			{NodeKind: "fact", NodeID: result, AssetID: "asset-a"},
			{NodeKind: "fact", NodeID: "side-observation", AssetID: "asset-b"},
		},
		Revision: 1,
	}
}

func TestHistoryIsLiteralExpandableAndDoesNotMutateSnapshot(t *testing.T) {
	state := historyTestState()
	before, _ := json.Marshal(state)
	history := state.History()
	if len(history) != 1 {
		t.Fatalf("cold groups=%+v", history)
	}
	entry := history[0]
	if entry.ID != "old" || entry.Status != "completed" || !entry.TextTruncated || !utf8.ValidString(entry.Summary) || len(entry.Summary) > 240 || !strings.HasPrefix(state.Steps[0].Description, entry.Summary) {
		t.Fatalf("summary was not a bounded literal prefix: %+v", entry)
	}
	if !slices.Equal(entry.AssetIDs, []string{"asset-a", "asset-b"}) || len(entry.Members) != 3 {
		t.Fatalf("membership or asset union=%+v", entry)
	}
	for i, id := range []string{"old", "observation", "side-observation"} {
		if entry.Members[i].ID != id {
			t.Fatalf("member %d=%+v", i, entry.Members[i])
		}
	}
	observation := entry.Members[1]
	if observation.Kind != "fact" || observation.Status != "valid" || !observation.TextTruncated || !observation.ScopeTruncated || observation.ObservedAt != state.FactRecords[2].ObservedAt || !utf8.ValidString(observation.Excerpt) || !strings.HasPrefix(state.FactRecords[2].Description, observation.Excerpt) || !strings.HasPrefix(state.FactRecords[2].Scope, observation.Scope) {
		t.Fatalf("observation qualifiers were lost or invented: %+v", observation)
	}
	if !reflect.DeepEqual(history, state.History()) {
		t.Fatal("history is not deterministic")
	}
	history[0].Members[0].ID = "changed"
	history[0].AssetIDs[0] = "changed"
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Fatal("reading or editing history changed the frozen state")
	}
}

func TestHistoryProtectsLiveSupportAndUnresolvedConditions(t *testing.T) {
	cases := map[string]func(*State){
		"goal source":        func(s *State) { s.Goals[0].Sources = []string{"observation"} },
		"candidate source":   func(s *State) { s.Candidates = []Candidate{{ID: "note", Sources: []string{"observation"}}} },
		"candidate producer": func(s *State) { s.Candidates = []Candidate{{ID: "note", SourceStepID: "old"}} },
		"finding source":     func(s *State) { s.Findings = []Finding{{ID: "finding", Sources: []string{"observation"}}} },
		"dispute source": func(s *State) {
			s.Disputes = []Dispute{{ID: "dispute", Status: "open", ReviewFactIDs: []string{"observation"}}}
		},
		"dispute step": func(s *State) {
			s.Disputes = []Dispute{{ID: "dispute", Status: "open", ReviewStepIDs: []string{"old"}}}
		},
		"dispute reference": func(s *State) {
			s.Disputes = []Dispute{{ID: "dispute", Status: "open"}}
			s.Steps[0].DisputeID = "dispute"
		},
		"correction chain": func(s *State) {
			s.FactRelations = []FactRelation{{Kind: "narrows", Source: "new", Target: "observation"}}
		},
		"unsupported result":  func(s *State) { s.FactRecords[2].SupportInvalid = true },
		"refuted observation": func(s *State) { s.FactRecords[2].Status = "refuted" },
		"unsupported step":    func(s *State) { s.Steps[0].SupportValid = false },
		"invalid premise":     func(s *State) { s.Steps[0].InvalidSources = []string{"source"} },
		"blocked dependency":  func(s *State) { s.Steps[0].BlockedBy = []string{"upstream"} },
		"running consumer": func(s *State) {
			s.Steps = append(s.Steps, Step{ID: "current", Status: "running", From: []string{"observation"}})
		},
		"failed consumer": func(s *State) {
			s.Steps = append(s.Steps, Step{ID: "failed", Status: "failed", From: []string{"observation"}})
		},
		"transitive execution": func(s *State) {
			s.Steps = append(s.Steps, Step{ID: "middle", Status: "completed", SupportValid: true, DependsOn: []string{"old"}}, Step{ID: "current", Status: "running", DependsOn: []string{"middle"}})
		},
		"curation request": func(s *State) {
			s.Curation = CurationProgress{Generation: 1, Request: &CurationRequest{Revision: 1, Sources: []string{"observation"}}}
		},
		"root completion": func(s *State) { goal := "goal"; s.Steps[0].Result = &goal },
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) {
			state := historyTestState()
			modify(&state)
			for _, entry := range state.History() {
				if entry.ID == "old" {
					t.Fatalf("folded required historical support: %+v", entry)
				}
			}
		})
	}
}

func TestHistoryPreservesFeedbackAndAmbiguousLegacyOwnership(t *testing.T) {
	state := historyTestState()
	state.Graph.Project.OrchestrationVersion = 0
	state.FactRecords[2].SourceStepID = ""
	state.Steps = append(state.Steps, Step{ID: "other", Status: "completed", Result: state.Steps[0].Result})
	for _, entry := range state.History() {
		for _, member := range entry.Members {
			if member.ID == "observation" {
				t.Fatal("ambiguous legacy provenance was inferred")
			}
		}
	}
	state.Goals[0].Sources = []string{"observation"}
	if len(state.History()) != 0 {
		t.Fatal("possible producers of active evidence were hidden")
	}
	state = historyTestState()
	state.Steps[0].Description = "external_feedback"
	state.FactRecords[2].Legacy, state.FactRecords[2].SourceStepID = true, ""
	if len(state.History()) != 0 {
		t.Fatal("user feedback was folded as old work")
	}
}

func TestHistoryFoldsAbandonedWorkAndKeepsMissingProducerFactVisible(t *testing.T) {
	state := historyTestState()
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
	state := historyTestState()
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
	state := historyTestState()
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
