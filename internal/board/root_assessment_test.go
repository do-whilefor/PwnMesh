package board

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func closureFixture(t *testing.T, protocol int) (*orchestrationFixture, string, string, string) {
	t.Helper()
	f := newOrchestrationFixture(t)
	w := f.worker("observation", "")
	fact := f.fact(w, "actual response")
	f.finish(w, fact)
	optional := f.step("Optional extra report", "")
	s := f.state()
	version := DecisionStateVersion(s)
	e := Execution{ProjectID: "p", ID: "plan", Namespace: "test", Backend: "planner", Kind: "reason", Lease: f.planner.Run, RetryKey: "reason:root-assessment"}
	e.Job, _ = json.Marshal(map[string]any{"kind": "reason", "run_id": e.ID, "graph": s.Graph, "state": s, "graph_rpc": true, "result_contract_version": 2,
		"decision": &DecisionContext{Version: 2, StateVersion: version, ClosureProtocol: protocol}, "budget": map[string]int{"max_intents": 8}})
	f.do(func(tx *Tx) error { return tx.RegisterExecution(e) })
	return f, fact, optional, version
}

func missingRootAssessment(fact string) *RootAssessment {
	return &RootAssessment{Status: "missing", From: []string{fact}, Description: "The original response requirement has an uncovered condition", Gaps: []RequirementGap{{ID: "response", InputIDs: []string{"goal"}, Description: "Obtain the required response under the specified condition"}}}
}

func closureAction(op, gap string, payload any) DecisionAction {
	raw, _ := json.Marshal(payload)
	return DecisionAction{Op: op, GapID: gap, Payload: raw}
}

func TestClosureProtocolRejectsUnassessedAndUnanchoredExpansionAtomically(t *testing.T) {
	for _, scenario := range []string{"missing_assessment", "missing_gap", "unknown_gap", "observation_anchor", "missing_evidence", "satisfied_expansion", "missing_complete", "satisfied_without_complete", "changed_completion_proof", "stale"} {
		t.Run(scenario, func(t *testing.T) {
			f, fact, _, version := closureFixture(t, 1)
			batch := DecisionBatch{ExpectedVersion: version, Assessment: missingRootAssessment(fact), Actions: []DecisionAction{closureAction("step", "response", map[string]any{"action": "add", "from": []string{fact}, "description": "Obtain the missing required response"})}}
			want := 422
			switch scenario {
			case "missing_assessment":
				batch.Assessment = nil
			case "missing_gap":
				batch.Actions[0].GapID = ""
			case "unknown_gap":
				batch.Actions[0].GapID = "invented"
			case "observation_anchor":
				batch.Assessment.Gaps[0].InputIDs = []string{fact}
			case "missing_evidence":
				batch.Assessment.From = []string{"missing"}
				want = 404
			case "satisfied_expansion":
				batch.Assessment = &RootAssessment{Status: "satisfied", From: []string{fact}, Description: "User requirement is satisfied"}
			case "missing_complete":
				batch.Actions = []DecisionAction{closureAction("complete", "", map[string]any{"from": []string{fact}, "description": "Premature completion"})}
			case "satisfied_without_complete":
				batch.Assessment = &RootAssessment{Status: "satisfied", From: []string{fact}, Description: "User requirement is satisfied"}
				batch.Actions = nil
			case "changed_completion_proof":
				batch.Assessment = &RootAssessment{Status: "satisfied", From: []string{fact}, Description: "User requirement is satisfied"}
				batch.Actions = []DecisionAction{closureAction("complete", "", map[string]any{"from": []string{"different"}, "description": "Replace the assessed proof"})}
			case "stale":
				batch.ExpectedVersion = strings.Repeat("0", 64)
				want = 409
			}
			before := f.state()
			f.do(func(tx *Tx) error {
				_, err := tx.PreviewDecision("p", f.planner, batch)
				requireAPIStatus(t, err, want)
				_, err = tx.CommitDecision("p", f.planner, batch)
				requireAPIStatus(t, err, want)
				return nil
			})
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("rejected closure decision changed state")
			}
		})
	}
}

func TestClosureProtocolAllowsRealGapAndBindsReceiptToAssessment(t *testing.T) {
	f, fact, _, version := closureFixture(t, 1)
	batch := DecisionBatch{ExpectedVersion: version, Assessment: missingRootAssessment(fact), Actions: []DecisionAction{closureAction("step", "response", map[string]any{"action": "add", "from": []string{fact}, "description": "Obtain the missing required response"})}}
	f.do(func(tx *Tx) error {
		preview, err := tx.PreviewDecision("p", f.planner, batch)
		if err != nil {
			return err
		}
		if preview.Committed {
			t.Fatal("preview wrote an assessment")
		}
		result, err := tx.CommitDecision("p", f.planner, batch)
		if err != nil {
			return err
		}
		if !result.Committed || result.Completed {
			t.Fatal("real gap did not retain active work")
		}
		replay, err := tx.CommitDecision("p", f.planner, batch)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(result, replay) {
			t.Fatal("assessment receipt replay changed")
		}
		batch.Assessment.Description = "A different root judgment"
		_, err = tx.CommitDecision("p", f.planner, batch)
		requireAPIStatus(t, err, 409)
		return nil
	})
}

func TestClosureProtocolCompletesOnlyAfterExplicitlyRetiringOptionalWork(t *testing.T) {
	f, fact, optional, version := closureFixture(t, 1)
	batch := DecisionBatch{ExpectedVersion: version, Assessment: &RootAssessment{Status: "satisfied", From: []string{fact}, Description: "The original requested response is directly evidenced"}, Actions: []DecisionAction{closureAction("complete", "", map[string]any{"from": []string{fact}, "description": "Observed the required response"})}}
	f.do(func(tx *Tx) error {
		_, err := tx.PreviewDecision("p", f.planner, batch)
		requireAPIStatus(t, err, 409)
		batch.Actions = append([]DecisionAction{closureAction("step", "", map[string]any{"action": "abandon", "id": optional, "reason": "Extra report is not an original user requirement"})}, batch.Actions...)
		preview, err := tx.PreviewDecision("p", f.planner, batch)
		if err != nil {
			return err
		}
		if preview.CompletionReview == nil || preview.CompletionReview.Acceptance != "not_checked" {
			t.Fatal("root assessment bypassed evidence review")
		}
		result, err := tx.CommitDecision("p", f.planner, batch)
		if err == nil && !result.Completed {
			t.Fatal("satisfied root did not complete after explicit closure")
		}
		return err
	})
}

func TestClosureProtocolLeavesOldRegisteredJobAndReceiptCanonicalCompatible(t *testing.T) {
	f, fact, _, version := closureFixture(t, 0)
	batch := DecisionBatch{ExpectedVersion: version, Actions: []DecisionAction{closureAction("step", "", map[string]any{"action": "add", "from": []string{fact}, "description": "Legacy plan"})}}
	f.do(func(tx *Tx) error {
		batch.Actions[0].GapID = "orphan"
		_, err := tx.CommitDecision("p", f.planner, batch)
		requireAPIStatus(t, err, 422)
		batch.Actions[0].GapID = ""
		if _, err := tx.CommitDecision("p", f.planner, batch); err != nil {
			return err
		}
		_, canonical, err := tx.savedDecision("p", f.planner.Run)
		if err != nil {
			return err
		}
		actions, _ := normalizeDecisionActions(batch.Actions)
		old, _ := json.Marshal(actions)
		if canonical != string(old) || strings.Contains(canonical, "gap_id") || strings.Contains(canonical, "assessment") {
			t.Fatal("legacy receipt canonical changed")
		}
		_, err = tx.CommitDecision("p", f.planner, batch)
		return err
	})
}

func TestRootAssessmentRejectsInvalidShapesAndFutureProtocols(t *testing.T) {
	for _, scenario := range []string{"nil", "unknown_status", "blank_description", "duplicate_proof", "satisfied_no_proof", "satisfied_with_gap", "missing_no_gap", "duplicate_gap", "invalid_gap_id", "duplicate_input"} {
		t.Run(scenario, func(t *testing.T) {
			a := missingRootAssessment("f001")
			switch scenario {
			case "nil":
				a = nil
			case "unknown_status":
				a.Status = "accepted"
			case "blank_description":
				a.Description = " "
			case "duplicate_proof":
				a.From = []string{"f001", "f001"}
			case "satisfied_no_proof":
				a.Status, a.From, a.Gaps = "satisfied", nil, nil
			case "satisfied_with_gap":
				a.Status = "satisfied"
			case "missing_no_gap":
				a.Gaps = nil
			case "duplicate_gap":
				a.Gaps = append(a.Gaps, a.Gaps[0])
			case "invalid_gap_id":
				a.Gaps[0].ID = " invalid "
			case "duplicate_input":
				a.Gaps[0].InputIDs = []string{"goal", "goal"}
			}
			if ValidateRootAssessmentShape(a) == nil {
				t.Fatal("malformed root assessment accepted")
			}
		})
	}
	for _, protocol := range []int{-1, 2} {
		raw, _ := json.Marshal(map[string]any{"decision": map[string]any{"version": 2, "closure_protocol": protocol}})
		if _, err := DecisionJobVersion(raw); err == nil || !strings.Contains(err.Error(), "unsupported closure") {
			t.Fatalf("unsupported protocol %d was silently accepted: %v", protocol, err)
		}
	}
	// A legacy decision uses direct writes, so it cannot opt into a policy
	// enforced only by the version 2 batch endpoint.
	if _, err := DecisionJobVersion(json.RawMessage(`{"decision":{"version":1,"closure_protocol":1}}`)); err == nil || !strings.Contains(err.Error(), "unsupported closure") {
		t.Fatalf("legacy write protocol advertised an unenforced closure policy: %v", err)
	}
}

func TestRootAssessmentValidatesOriginalInputsAndAllExpansionKinds(t *testing.T) {
	inputs, hints := []Fact{{ID: "origin"}, {ID: "goal"}, {ID: "reopened"}}, []Hint{{ID: "h001"}}
	for _, input := range []string{"goal", "origin", "reopened", "h001"} {
		a := missingRootAssessment("f001")
		a.Gaps[0].InputIDs = []string{input}
		if err := ValidateRootAssessment(a, inputs, hints); err != nil {
			t.Fatal(err)
		}
		for _, action := range []DecisionAction{closureAction("goal", "response", map[string]any{"action": "add"}), closureAction("step", "response", map[string]any{"action": "add"}), closureAction("step", "response", map[string]any{"action": "retry"}), closureAction("curation_request", "response", map[string]any{"sources": []string{"f001"}})} {
			if err := ValidateClosureActions(a, []DecisionAction{action}); err != nil {
				t.Fatal(err)
			}
			action.GapID = ""
			if err := ValidateClosureActions(a, []DecisionAction{action}); err == nil {
				t.Fatal("unbound expansion accepted")
			}
		}
	}
}
