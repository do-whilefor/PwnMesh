package board

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestCurationPrioritizesExactCandidateBodiesWithin32KiB(t *testing.T) {
	state := overviewState()
	state.Graph.Project.Generation = 0
	state.Graph.Project.OrchestrationVersion = 1
	state.Revision, state.Curation.ThroughRevision = 10, 8
	for i := 0; i < 40; i++ {
		state.Candidates = append(state.Candidates, Candidate{ID: fmt.Sprintf("old_%d", i), Claim: fmt.Sprintf("Unrelated history %d", i), Scope: "old", Revision: 1, Reason: strings.Repeat("old ", 200)})
	}
	for i := 0; i < 2; i++ {
		state.Candidates = append(state.Candidates, Candidate{
			ID: fmt.Sprintf("relevant_%d", i), Claim: "A precise candidate claim", Scope: "same exact scope", Status: "verified", Sources: []string{"first"},
			Reason: strings.Repeat(fmt.Sprintf("reason-%d ", i), 800), Revision: int64(7 + i*2), RunID: fmt.Sprintf("run_%d", i), SourceStepID: fmt.Sprintf("step_%d", i),
			Evidence: []EvidenceRef{{Path: fmt.Sprintf("/evidence/%d", i), Excerpt: strings.Repeat("original ", 900)}},
		})
	}
	state.Disputes = []Dispute{{ID: "old_dispute", Status: "resolved", Question: strings.Repeat("old question ", 1600)}}
	before, _ := json.Marshal(state)
	version := DecisionStateVersion(state)
	raw, err := CurationContextView(state, DefaultContextViewBytes)
	if err != nil || len(raw) > DefaultContextViewBytes {
		t.Fatalf("curation context exceeded 32 KiB: %d, %v", len(raw), err)
	}
	var view struct {
		Candidates []contextCandidate    `json:"candidates"`
		Coverage   curationInputCoverage `json:"curation_input"`
		Omitted    map[string]int        `json:"omitted"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Candidates) != 2 || view.Coverage.RelevantCandidates != 2 || view.Coverage.ProvidedCandidates != 2 || view.Coverage.OmittedCandidates != 0 || view.Coverage.EvidenceOmitted != 2 || view.Omitted["candidates"] != 40 {
		t.Fatalf("related work was hidden by global history or called complete: %+v, omitted=%v", view.Coverage, view.Omitted)
	}
	for i, candidate := range view.Candidates {
		want := state.Candidates[40+i]
		want.Evidence = nil
		if !reflect.DeepEqual(candidate.Candidate, want) || !candidate.EvidenceOmitted || candidate.EvidenceCount != 1 || candidate.GroupKey == "" || candidate.SupportValid == nil {
			t.Fatalf("candidate judgment fields changed or missing support was hidden: %+v", candidate)
		}
	}
	after, _ := json.Marshal(state)
	if string(before) != string(after) || version != DecisionStateVersion(state) {
		t.Fatal("context projection changed immutable input or its version")
	}
}

func TestCurationCoverageCountsRemainExactAtBudgetEdges(t *testing.T) {
	state := overviewState()
	state.Graph.Project.Generation = 0
	state.Graph.Project.OrchestrationVersion = 1
	state.Revision = 1
	for i := 0; i < 120; i++ {
		state.Candidates = append(state.Candidates, Candidate{ID: fmt.Sprintf("c%03d", i), RunID: fmt.Sprintf("run%03d", i), Claim: "Exact claim", Scope: "scope", Reason: strings.Repeat("reason ", 30), Revision: 1, Evidence: []EvidenceRef{{Path: "/evidence", Excerpt: "exact original bytes"}}})
	}
	for budget := 12000; budget <= DefaultContextViewBytes; budget += 137 {
		raw, err := CurationContextView(state, budget)
		if err != nil || len(raw) > budget {
			t.Fatalf("budget %d: %d bytes, %v", budget, len(raw), err)
		}
		var view struct {
			Candidates []contextCandidate    `json:"candidates"`
			Coverage   curationInputCoverage `json:"curation_input"`
		}
		if err := json.Unmarshal(raw, &view); err != nil {
			t.Fatal(err)
		}
		omittedEvidence := 0
		for _, candidate := range view.Candidates {
			if candidate.EvidenceOmitted {
				omittedEvidence++
			} else if len(candidate.Evidence) != 1 || candidate.Evidence[0].Excerpt != "exact original bytes" {
				t.Fatal("restored candidate evidence changed or was falsely complete")
			}
		}
		if view.Coverage.ProvidedCandidates != len(view.Candidates) || view.Coverage.OmittedCandidates != 120-len(view.Candidates) || view.Coverage.EvidenceOmitted != omittedEvidence {
			t.Fatalf("budget %d lost exact coverage: %+v", budget, view.Coverage)
		}
	}
}

func TestCurationDiscoveryPrioritizesMissingRelevantCandidate(t *testing.T) {
	state := overviewState()
	state.Graph.Project.Generation = 0
	state.Graph.Project.OrchestrationVersion = 1
	state.Revision, state.Curation.ThroughRevision = 9, 8
	for i := 0; i < 80; i++ {
		state.Candidates = append(state.Candidates, Candidate{ID: fmt.Sprintf("old_%d", i), Claim: "Unrelated historical claim", Scope: "old", Revision: 1})
	}
	state.Candidates = append(state.Candidates, Candidate{ID: "missing_relevant", Claim: strings.Repeat("claim ", 2500), Scope: "new", Reason: strings.Repeat("reason ", 1100), Revision: 9})
	state.Findings = []Finding{{ID: "shared", Claim: strings.Repeat("claim ", 2500), Scope: "new", Status: "candidate"}}
	raw, err := CurationContextView(state, DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Coverage curationInputCoverage `json:"curation_input"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if view.Coverage.OmittedCandidates != 1 || view.Coverage.ProvidedCandidates != 0 {
		t.Fatalf("oversized candidate was presented as supplied: %+v", view.Coverage)
	}
	section := overviewSection(t, decodeOverview(t, raw), "candidates")
	found := false
	for _, item := range section.Items {
		if item.ID == "missing_relevant" {
			found = item.TextTruncated && item.GroupKey != ""
		}
	}
	if !found {
		t.Fatal("unrelated history hid the exact ID needed to read missing candidate details")
	}
}

func TestCurationPrioritizesCandidateSourcesOverUnrelatedActiveSteps(t *testing.T) {
	state := overviewState()
	state.Graph.Project.Generation = 0
	state.Graph.Project.OrchestrationVersion = 1
	state.Revision = 1
	active := Step{ID: "unrelated_work", Status: "running", GoalID: "goal"}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("old_fact_%d", i)
		active.From = append(active.From, id)
		state.FactRecords = append(state.FactRecords, FactRecord{ID: id, Status: "valid", Evidence: []EvidenceRef{{Path: "/old", Excerpt: strings.Repeat("old observation ", 130)}}})
	}
	state.Steps = append(state.Steps, active)
	state.Candidates = []Candidate{{ID: "new", Claim: "Current judgment", Scope: "current", Revision: 1, Sources: []string{"current_source"}}}
	state.Findings = []Finding{{ID: "shared", Claim: "Current judgment", Scope: "current", Status: "candidate"}}
	state.FactRecords = append(state.FactRecords, FactRecord{ID: "current_source", Status: "valid", Description: "Current exact observation", Evidence: []EvidenceRef{{Path: "/current", Excerpt: "current original bytes"}}})
	raw, err := CurationContextView(state, DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Facts []contextFact `json:"fact_records"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	for _, fact := range view.Facts {
		if fact.ID == "current_source" && !fact.DetailsOmitted && len(fact.Evidence) == 1 && fact.Evidence[0].Excerpt == "current original bytes" {
			return
		}
	}
	t.Fatal("unrelated active Step inputs displaced the curator's current original source")
}
