package board

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestCurationCandidateHintsSurviveBodyOmissionWithoutChangingSnapshot(t *testing.T) {
	state := overviewState()
	state.Graph.Project.Generation = 0
	state.Graph.Project.OrchestrationVersion = 1
	state.Revision = 1
	state.Candidates = []Candidate{{ID: "oversized", Claim: "Exact group", Scope: "fixture", Status: "candidate", Sources: []string{"first"}, Revision: 1, Reason: strings.Repeat("large body ", 4000)}}
	state.Findings = []Finding{{ID: "shared", Claim: "Exact group", Scope: "fixture", Status: "candidate"}}
	before, _ := json.Marshal(state)
	version := DecisionStateVersion(state)
	raw, err := CurationContextView(state, 0)
	if err != nil || len(raw) > DefaultContextViewBytes {
		t.Fatalf("unbounded candidate hints: %v bytes=%d", err, len(raw))
	}
	var view struct {
		Candidates []model.CandidateView `json:"candidates"`
		Omitted    map[string]int        `json:"omitted"`
		Overview   contextOverview       `json:"overview"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Candidates) != 0 || view.Omitted["candidates"] != 1 {
		t.Fatal("large candidate was not explicitly omitted")
	}
	found := false
	for _, section := range view.Overview.Sections {
		for _, item := range section.Items {
			if section.Section == "candidates" && item.ID == "oversized" {
				found = item.GroupKey == model.FindingIdentity("Exact group", "fixture") && item.SupportValid != nil && item.Status == "candidate"
			}
		}
	}
	after, _ := json.Marshal(state)
	if !found || string(before) != string(after) || version != DecisionStateVersion(state) || strings.Contains(string(after), "group_key") {
		t.Fatal("derived hints were lost or entered persisted state/version")
	}
}

func TestCurationHintsDoNotRelaxGroupingOrProducerConfidence(t *testing.T) {
	for _, tc := range []struct{ name, claim, scope, status string }{
		{"different claim", "Another claim", "same condition", "verified"},
		{"different scope", "Same claim", "another condition", "verified"},
		{"tentative promotion", "Same claim", "same condition", "candidate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newOrchestrationFixture(t)
			producer := fixture.worker("producer", "")
			fact := fixture.fact(producer, "observation")
			add := func(key, claim, scope string) string {
				return fixture.action(producer.Fence(), "candidate", key, map[string]any{"claim": claim, "scope": scope, "status": tc.status, "sources": []string{fact}, "reason": "Original producer judgment"}, "").ID
			}
			first, second := add("a", "Same claim", "same condition"), add("b", tc.claim, tc.scope)
			curator, input := fixture.curator("curator")
			if _, err := CurationContextView(input, 0); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(fixture.state())
			payload, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{{CandidateIDs: []string{first, second}, Status: "verified", Reason: "Invalid semantic merge or promotion"}}})
			err := fixture.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "rejected", Payload: payload, ExpectedVersion: DecisionStateVersion(input)})
				return err
			})
			requireAPIStatus(t, err, 422)
			after, _ := json.Marshal(fixture.state())
			if string(before) != string(after) {
				t.Fatal("rejected grouping/confidence changed facts, findings or cursor")
			}
		})
	}
}

func TestCurationContextTracksCursorAndRetainsRelatedHistory(t *testing.T) {
	state := overviewState()
	state.Graph.Project.Generation = 0
	state.Graph.Project.OrchestrationVersion = 1
	state.Revision = 8
	state.Candidates = []Candidate{
		{ID: "old", Claim: "the claim", Scope: "one", Revision: 1, RunID: "producer", Sources: []string{"first"}},
		{ID: "unrelated", Claim: "other", Scope: "one", Revision: 2},
		{ID: "review", Claim: "the  claim", Scope: " one ", Revision: 7, RunID: "review", Sources: []string{"new"}},
	}
	state.Disputes = []Dispute{{ID: "dispute", Status: "resolved", CandidateIDs: []string{"old", "review"}}}
	for _, tc := range []struct {
		cursor int64
		status string
		want   []string
	}{
		{0, "resolved", []string{"old", "review"}},
		{5, "resolved", []string{"old", "review"}},
		{8, "resolved", []string{}},
		{8, "reviewing", []string{"old", "review"}},
		{8, "uncertain", []string{"old", "review"}},
	} {
		state.Curation.ThroughRevision = tc.cursor
		state.Disputes[0].Status = tc.status
		before, _ := json.Marshal(state)
		raw, err := CurationContextView(state, 0)
		if err != nil {
			t.Fatal(err)
		}
		var view struct {
			Candidates []Candidate     `json:"candidates"`
			Omitted    map[string]int  `json:"omitted"`
			Overview   contextOverview `json:"overview"`
		}
		if err := json.Unmarshal(raw, &view); err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, candidate := range view.Candidates {
			ids = append(ids, candidate.ID)
		}
		if !reflect.DeepEqual(ids, tc.want) || view.Omitted["candidates"] != len(state.Candidates)-len(tc.want) {
			t.Fatalf("cursor %d/%s: candidates=%v omitted=%v", tc.cursor, tc.status, ids, view.Omitted)
		}
		for _, section := range view.Overview.Sections {
			if section.Section == "candidates" && (section.Total != 3 || len(section.Items) != 3) {
				t.Fatalf("omitted history disappeared from discovery: %+v", section)
			}
		}
		after, _ := json.Marshal(state)
		if string(before) != string(after) {
			t.Fatal("curation view changed the immutable state")
		}
	}
}

func TestCurationContextKeepsRelationsDependenciesAndBoundsEvidence(t *testing.T) {
	state := overviewState()
	state.Graph.Project.Generation = 0
	state.Graph.Project.OrchestrationVersion = 1
	state.Revision, state.Curation.ThroughRevision = 9, 6
	state.Candidates = []Candidate{{ID: "new_candidate", Claim: "a claim", Scope: "scope", Revision: 8, Sources: []string{"review_fact"}}}
	state.Findings = []Finding{{ID: "shared", Claim: "a claim", Scope: "scope", Status: "candidate"}}
	state.FactRecords = append(state.FactRecords,
		FactRecord{ID: "review_fact", SourceStepID: "review", Status: "valid", Evidence: []EvidenceRef{{Path: "/review", Excerpt: "independent observation"}}},
		FactRecord{ID: "related", Status: "valid", Evidence: []EvidenceRef{{Path: "/related", Excerpt: "older original bytes"}}},
	)
	result := "review_fact"
	state.Steps = append(state.Steps, Step{ID: "review", Status: "completed", From: []string{"first"}, DependsOn: []string{"producer"}, Result: &result})
	state.FactRelations = append(state.FactRelations, FactRelation{Kind: "narrows", Source: "related", Target: "review_fact", Reason: "retained relation"})
	raw, err := CurationContextView(state, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"older original bytes", "independent observation", `"depends_on":["producer"]`, "retained relation"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("view lost related data %q", want)
		}
	}
	state.Candidates[0].Reason = strings.Repeat("large judgment ", 4000)
	raw, err = CurationContextView(state, 0)
	if err != nil || len(raw) > DefaultContextViewBytes {
		t.Fatalf("unbounded curation view: %d bytes, %v", len(raw), err)
	}
	var view struct {
		Omitted map[string]int `json:"omitted"`
	}
	_ = json.Unmarshal(raw, &view)
	if view.Omitted["candidates"] != 1 || !strings.Contains(string(raw), "new_candidate") {
		t.Fatal("oversized candidate must be explicitly omitted and discoverable")
	}
}
