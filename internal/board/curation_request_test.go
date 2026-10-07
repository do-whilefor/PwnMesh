package board

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestExplicitCurationRequestMakesFactCorrectionReachable(t *testing.T) {
	f := newOrchestrationFixture(t)
	first, second := f.worker("first", ""), f.worker("second", "")
	old, correction := f.fact(first, "first-service-response"), f.fact(second, "fresh-protocol-observation")
	f.finish(first, old)
	f.finish(second, correction)
	assertCurationNeeded(t, f, false)
	before := f.state()
	payload := map[string]any{"sources": []string{old, correction}, "reason": "The differently worded observations may describe a corrected service state; inspect the retained originals"}
	f.action(f.planner, "curation_request", "request", payload, DecisionStateVersion(before))
	requested := f.state()
	request := requested.PendingCurationRequest()
	if request == nil || request.Revision != before.Revision+1 || !reflect.DeepEqual(request.Sources, []string{old, correction}) || len(requested.Candidates) != 0 {
		t.Fatal("explicit reconciliation required synthetic candidate labels or lost the request")
	}
	assertCurationNeeded(t, f, true)
	f.do(func(tx *Tx) error {
		page, err := tx.ScheduleInput("p", 0, "")
		if err == nil && (!page.CurationNeeded || !page.CurationRequested) {
			t.Fatal("explicit reconciliation lost scheduler priority")
		}
		return err
	})
	replay := f.action(f.planner, "curation_request", "request-again", map[string]any{"sources": []string{correction, old}, "reason": payload["reason"]}, "")
	if !replay.Unchanged || !reflect.DeepEqual(f.state(), requested) {
		t.Fatal("same pending request changed its boundary or evidence")
	}
	curator, input := f.curator("requested-correction")
	f.action(curator.Fence(), "curate", "correct", CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{{Kind: "supersedes", Source: correction, Target: old, Reason: "Original retained bytes confirm the later response replaces the old service observation"}}}, DecisionStateVersion(input))
	f.finish(curator, "")
	final := f.state()
	if final.PendingCurationRequest() != nil || final.Curation.Request != nil || final.Curation.ThroughRevision != requested.Revision || len(final.FactRelations) != 1 || len(final.Candidates)+len(final.Findings) != 0 || final.ValidateFactSources([]string{old}, true) == nil {
		t.Fatal("requested correction was not acknowledged atomically without manufacturing judgments")
	}
	assertCurationNeeded(t, f, false)
	f.do(func(tx *Tx) error {
		events, err := tx.StateEvents("p", before.Revision)
		if err == nil && (len(events) < 1 || events[0].Op != "curation_request" || string(events[0].Payload) == "") {
			t.Fatal("acknowledgement erased request history")
		}
		return err
	})
}

func TestCurationRequestRejectsInvalidSourcesAndPendingReplacement(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
	}{{"missing", 404}, {"origin", 409}, {"duplicate", 422}, {"invalidated", 409}, {"distinct_pending", 409}, {"producer", 403}, {"stale_version", 409}} {
		t.Run(test.name, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			old, fresh := f.fact(producer, "old"), f.fact(producer, "fresh")
			sources, reason, fence, version := []string{old}, "Inspect contradictory originals", f.planner, ""
			switch test.name {
			case "missing":
				sources = []string{"missing"}
			case "origin":
				sources = []string{"origin"}
			case "duplicate":
				sources = []string{old, old}
			case "invalidated":
				curator, input := f.curator("invalidate")
				f.action(curator.Fence(), "curate", "invalidate", CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{{Kind: "refutes", Source: fresh, Target: old, Reason: "Correct the old observation"}}}, DecisionStateVersion(input))
				f.finish(curator, "")
			case "distinct_pending":
				f.action(f.planner, "curation_request", "first", map[string]any{"sources": []string{fresh}, "reason": "First review must not be overwritten"}, "")
			case "producer":
				fence = producer.Fence()
			case "stale_version":
				version = DecisionStateVersion(f.state())
				f.fact(producer, "newer-input")
			}
			before := f.state()
			raw, _ := json.Marshal(map[string]any{"sources": sources, "reason": reason})
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", fence, StateAction{Op: "curation_request", IdempotencyKey: "rejected-request", Payload: raw, ExpectedVersion: version})
				return err
			})
			requireAPIStatus(t, err, test.status)
			if !reflect.DeepEqual(f.state(), before) {
				t.Fatal("rejected request overwrote pending evidence or changed the state")
			}
		})
	}
}

func TestCurationRequestIsAnAtomicPlannerAction(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	fact := f.fact(producer, "observation")
	f.finish(producer, fact)
	before := f.state()
	version := DecisionStateVersion(before)
	raw, _ := json.Marshal(map[string]any{"sources": []string{fact}, "reason": "Check whether the original response invalidates the recorded interpretation"})
	batch := DecisionBatch{ExpectedVersion: version, Actions: []DecisionAction{{Op: "curation_request", Payload: raw}}}
	f.do(func(tx *Tx) error {
		job, _ := json.Marshal(map[string]any{"kind": "reason", "run_id": "plan", "graph": before.Graph, "state": before, "graph_rpc": true, "result_contract_version": 2, "decision": map[string]any{"version": 2, "state_version": version}})
		return tx.RegisterExecution(Execution{ProjectID: "p", ID: "plan", Namespace: "test", Backend: "planner", Kind: "reason", Lease: f.planner.Run, Job: job, RetryKey: "reason:request"})
	})
	f.do(func(tx *Tx) error {
		preview, err := tx.PreviewDecision("p", f.planner, batch)
		if err == nil && (preview.Committed || preview.ChangedActions != 1) {
			t.Fatal("request preview was not a protocol-only single action")
		}
		return err
	})
	if !reflect.DeepEqual(f.state(), before) {
		t.Fatal("request preview escaped the decision savepoint")
	}
	f.do(func(tx *Tx) error {
		committed, err := tx.CommitDecision("p", f.planner, batch)
		if err == nil && (!committed.Committed || committed.ChangedActions != 1) {
			t.Fatal("planner could not yield to reconciliation without an Explore step")
		}
		return err
	})
	assertCurationNeeded(t, f, true)
}

func TestStaleCuratorCannotAcknowledgeARequest(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	fact := f.fact(producer, "observation")
	curator, input := f.curator("before-request")
	f.action(f.planner, "curation_request", "new-request", map[string]any{"sources": []string{fact}, "reason": "New evidence reconciliation request"}, "")
	before := f.state()
	raw, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "old-ack", Payload: raw, ExpectedVersion: DecisionStateVersion(input)})
		return err
	})
	requireAPIStatus(t, err, 409)
	if !reflect.DeepEqual(f.state(), before) || before.PendingCurationRequest() == nil {
		t.Fatal("old curator consumed a request outside its immutable input")
	}
	before.Graph.Project.Generation++
	if before.PendingCurationRequest() != nil {
		t.Fatal("a prior generation request leaked into new exploration")
	}
}
