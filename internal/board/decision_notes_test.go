package board

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func decisionNotesFixture() State {
	state := overviewState()
	state.Graph.Project.OrchestrationVersion = 1
	state.Revision = 4
	state.Findings = nil
	state.FactRecords[0].SourceStepID = "producer"
	state.FactRecords[0].Evidence = []EvidenceRef{{RunID: "run", Path: "retained/raw", Excerpt: "Original ambiguous response"}}
	state.Steps = []Step{{ID: "producer", GoalID: "goal", From: []string{"origin"}, Status: "completed", Result: Ptr("first")}}
	state.Candidates = []Candidate{
		{ID: "old", Claim: "Authentication may be bypassed", Scope: "service", Status: "verified", Sources: []string{"first"}, SourceStepID: "producer", RunID: "run", Generation: 1, Revision: 2},
		{ID: "revised", Claim: "Authentication may be bypassed", Scope: "service", Status: "candidate", Reason: "Response could also be a public login page; test a protected route", Sources: []string{"first"}, SourceStepID: "producer", RunID: "run", Generation: 1, Revision: 3, Supersedes: "old"},
	}
	state.Disputes = []Dispute{{ID: "uncertain", Status: "uncertain", Question: "Was a protected response reached?", CandidateIDs: []string{"revised"}}}
	return state
}

func TestDecisionNotesReachPlannerWithoutCurationAndPreserveSources(t *testing.T) {
	state := decisionNotesFixture()
	before, _ := json.Marshal(state)
	cursor := &DecisionCursor{ProjectID: "project", Generation: 1, Revision: 1}
	events := []StateEvent{{Revision: 2, Op: "candidate", ID: "old"}, {Revision: 3, Op: "candidate", ID: "revised"}, {Revision: 4, Op: "dispute", ID: "uncertain"}}
	decision, err := BuildDecisionContextFromCursor(state, cursor, events, 0)
	if err != nil {
		t.Fatal(err)
	}
	var view decisionView
	if json.Unmarshal(decision.View, &view) != nil || decision.Mode != "changes" || decision.FromRevision != 1 || decision.ToRevision != 4 || decision.StateVersion != DecisionStateVersion(state) {
		t.Fatalf("notes did not use the immutable incremental input: %+v", decision)
	}
	if len(view.Candidates) != 1 || view.Candidates[0].ID != "revised" || view.Candidates[0].Status != "candidate" || view.Candidates[0].Supersedes != "old" || !slices.Equal(view.Removed["candidates"], []string{"old"}) {
		t.Fatalf("revision was lost or retired judgment remained active: %+v", view)
	}
	if len(view.Facts) != 1 || !reflect.DeepEqual(view.Facts[0], state.FactRecords[0]) || len(view.Steps) != 1 || view.Steps[0].ID != "producer" || !reflect.DeepEqual(view.Disputes, state.Disputes) {
		t.Fatalf("note lost raw support, producer or unresolved uncertainty: %+v", view)
	}
	if !reflect.DeepEqual(view.UserInputs, state.Graph.Facts) {
		t.Fatal("notes displaced original requirements")
	}
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Fatal("context projection mutated evidence history")
	}
}

func TestDecisionNotesRemainVisibleWhenAnUnrelatedEventAdvancesCursor(t *testing.T) {
	state := decisionNotesFixture()
	cursor := &DecisionCursor{ProjectID: "project", Generation: 1, Revision: 3}
	decision, err := BuildDecisionContextFromCursor(state, cursor, []StateEvent{{Revision: 4, Op: "hint", ID: "user"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var view decisionView
	if json.Unmarshal(decision.View, &view) != nil || len(view.Candidates) != 1 || len(view.Disputes) != 1 || len(view.Facts) != 1 {
		t.Fatalf("uncurated notes disappeared behind a scheduling cursor: %s", decision.View)
	}
}

func TestDecisionNoteRevisionRetiresAnAlreadyAcknowledgedInterpretation(t *testing.T) {
	state := decisionNotesFixture()
	state.Revision = 3
	state.Disputes = nil
	decision, err := BuildDecisionContextFromCursor(state, &DecisionCursor{ProjectID: "project", Generation: 1, Revision: 2}, []StateEvent{{Revision: 3, Op: "candidate", ID: "revised"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var view decisionView
	if json.Unmarshal(decision.View, &view) != nil || !slices.Equal(view.Removed["candidates"], []string{"old"}) || len(view.Candidates) != 1 || view.Candidates[0].ID != "revised" {
		t.Fatalf("revision retained a previously acknowledged interpretation: %s", decision.View)
	}
}

func TestDecisionNotesSurviveFullViewAndBudgetFallback(t *testing.T) {
	state := decisionNotesFixture()
	state.Candidates[1].Evidence = []EvidenceRef{{RunID: "run", Path: "retained/raw", Excerpt: strings.Repeat("large evidence ", 1000)}}
	decision, err := BuildDecisionContextFromCursor(state, &DecisionCursor{ProjectID: "project", Generation: 1, Revision: 3}, []StateEvent{{Revision: 4, Op: "candidate", ID: "revised"}}, 6000)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Candidates []contextCandidate `json:"candidates"`
		Disputes   []Dispute          `json:"disputes"`
		ReadMore   string             `json:"read_more"`
	}
	if json.Unmarshal(decision.View, &view) != nil || decision.Mode != "full" || decision.Fallback != "related_context_over_budget" || len(decision.View) > 6000 {
		t.Fatalf("large note evidence bypassed bounded fallback: %+v", decision)
	}
	if len(view.Candidates) != 1 || view.Candidates[0].ID != "revised" || !view.Candidates[0].EvidenceOmitted || len(view.Disputes) != 1 || view.ReadMore == "" {
		t.Fatalf("bounded view lost uncertainty or concealed omitted evidence: %s", decision.View)
	}
	section := overviewSection(t, decodeOverview(t, decision.View), "candidates")
	if section.Total != 2 {
		t.Fatal("retired note vanished from historical discovery")
	}
	for _, item := range section.Items {
		if item.ID == "old" && !item.Superseded {
			t.Fatal("historical note was presented as a current interpretation")
		}
	}
}

func TestDecisionCurationEventPreservesChangedConclusionsAndDisputes(t *testing.T) {
	state := decisionNotesFixture()
	state.Findings = []Finding{{ID: "finding", Claim: "Conflicting interpretation", Sources: []string{"first"}, Status: "candidate"}}
	state.Disputes[0].FindingID = "finding"
	state.Curation.ThroughRevision = 4
	decision, err := BuildDecisionContextFromCursor(state, &DecisionCursor{ProjectID: "project", Generation: 1, Revision: 3}, []StateEvent{{Revision: 4, Op: "curate", ID: "curation"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var view decisionView
	if json.Unmarshal(decision.View, &view) != nil || decision.Mode != "changes" || len(view.Findings) != 1 || len(view.Disputes) != 1 || len(view.Candidates) != 1 || len(view.Facts) != 1 {
		t.Fatalf("curation change lost its current support closure: %s", decision.View)
	}
}

func TestRequestedCurationRetainsOriginalSourcesInCuratorAndDecisionContext(t *testing.T) {
	state := overviewState()
	state.Graph.Project.OrchestrationVersion = 1
	state.Revision = 2
	state.Curation = CurationProgress{Generation: 1, Request: &CurationRequest{Sources: []string{"requested"}, Reason: "These differently worded observations may conflict", Revision: 2}}
	for n := range 30 {
		state.FactRecords = append(state.FactRecords, FactRecord{ID: fmt.Sprintf("unrelated-%d", n), Status: "valid", Evidence: []EvidenceRef{{Path: "old", Excerpt: strings.Repeat("unrelated bytes ", 400)}}})
	}
	requested := FactRecord{ID: "requested", Status: "valid", Description: "Exact response needing reinterpretation", Evidence: []EvidenceRef{{Path: "raw", Excerpt: "ambiguous original bytes"}}}
	state.FactRecords = append(state.FactRecords, requested)
	raw, err := CurationContextView(state, DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	var curator struct {
		Facts []contextFact `json:"fact_records"`
	}
	if err := json.Unmarshal(raw, &curator); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, fact := range curator.Facts {
		found = found || fact.ID == requested.ID && !fact.DetailsOmitted && reflect.DeepEqual(fact.FactRecord, requested)
	}
	if !found {
		t.Fatal("explicit reconciliation sources were displaced by unrelated history")
	}
	decision, err := BuildDecisionContextFromCursor(state, &DecisionCursor{ProjectID: "project", Generation: 1, Revision: 1}, []StateEvent{{Revision: 2, Op: "curation_request", ID: "curation_request"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var view decisionView
	if json.Unmarshal(decision.View, &view) != nil || decision.Mode != "changes" || view.Curation == nil || !reflect.DeepEqual(view.Curation.Request, state.Curation.Request) || len(view.Facts) != 1 || !reflect.DeepEqual(view.Facts[0], requested) {
		t.Fatalf("request delta lost its reason or exact source: %s", decision.View)
	}
}
