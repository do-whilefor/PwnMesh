package board

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCompletionReviewKeepsTentativeNotesAndUnrelatedUncertainty(t *testing.T) {
	state := decisionNotesFixture()
	state.Disputes = append(state.Disputes, Dispute{ID: "resolved", Status: "resolved"})
	payload := json.RawMessage(`{"from":["first"],"description":"Proposed completion"}`)
	review, err := buildCompletionReview(state, "bound", payload, MaxCompletionReviewBytes)
	if err != nil {
		t.Fatal(err)
	}
	if review.Acceptance != "not_checked" || len(review.Candidates) != 1 || review.Candidates[0].ID != "revised" || len(review.Disputes) != 1 || review.Disputes[0].ID != "uncertain" || review.ReadMore == "" {
		t.Fatalf("review silently dismissed tentative or unresolved context: %+v", review)
	}
	if !reflect.DeepEqual(review.UserInputs, state.Graph.Facts) || !reflect.DeepEqual(review.FactRecords, state.FactRecords[:1]) {
		t.Fatal("uncertainty replaced root requirements or raw observations")
	}
	d := &DecisionContext{StateVersion: "bound", View: json.RawMessage(`{}`), Mode: "full"}
	if err := UseCompletionAssessment(state, d, review); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d)
	if d.Mode != "completion" || len(raw) > DefaultContextViewBytes || strings.Count(string(raw), state.Disputes[0].Question) != 1 || strings.Count(string(raw), state.Candidates[1].Reason) != 1 {
		t.Fatalf("completion packet lost or duplicated uncertainty: %s", raw)
	}
}

func TestCompletionReviewRetainsOmittedUncertaintyIDsWithinBudget(t *testing.T) {
	state := decisionNotesFixture()
	state.Candidates[1].Reason = strings.Repeat("large note ", 1000)
	state.Disputes[0].Question = strings.Repeat("large question ", 1000)
	review, err := buildCompletionReview(state, "bound", json.RawMessage(`{"from":["first"],"description":"Proposed completion"}`), 3000)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(review)
	if len(raw) > 3000 || len(review.Candidates)+len(review.Disputes) != 0 || !reflect.DeepEqual(review.OmittedCandidateIDs, []string{"revised"}) || !reflect.DeepEqual(review.OmittedDisputeIDs, []string{"uncertain"}) || review.ReadMore == "" || len(review.FactRecords) != 1 {
		t.Fatalf("large uncertainty vanished instead of remaining readable by ID: %s", raw)
	}
}

func TestCompletionReviewPreservesPendingReconciliationRequest(t *testing.T) {
	state := decisionNotesFixture()
	state.Curation = CurationProgress{Generation: 1, Request: &CurationRequest{Sources: []string{"second"}, Reason: "Reinterpret the original response before trusting it", Revision: 4}}
	review, err := buildCompletionReview(state, "bound", json.RawMessage(`{"from":["first"],"description":"Proposed completion"}`), MaxCompletionReviewBytes)
	if err != nil || !reflect.DeepEqual(review.CurationRequest, state.Curation.Request) || review.ReadMore == "" {
		t.Fatalf("review hid a pending request outside its cited proof: %+v %v", review, err)
	}
	d := &DecisionContext{StateVersion: "bound", View: json.RawMessage(`{}`), Mode: "full"}
	if err := UseCompletionAssessment(state, d, review); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d)
	if d.Mode != "completion" || strings.Count(string(raw), state.Curation.Request.Reason) != 1 {
		t.Fatalf("completion packet lost or duplicated request context: %s", raw)
	}
}
