package board

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestCurationRequestDispositionPersistsAndRetainsUnresolvedWork(t *testing.T) {
	for _, disposition := range []string{"resolved", "no_change", "needs_review"} {
		t.Run(disposition, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			run := f.worker("producer", "")
			fact := f.fact(run, "response")
			f.finish(run, fact)
			f.action(f.planner, "curation_request", "request", map[string]any{"sources": []string{fact}, "reason": "Inspect original response"}, "")
			curator, input := f.curator("review")
			review := &model.CurationRequestReview{Disposition: disposition, CheckedSources: []string{fact}, Reason: "Compared the retained original response"}
			payload := CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, RequestReview: review}
			receipt := f.action(curator.Fence(), "curate", "review", payload, DecisionStateVersion(input))
			state := f.state()
			if state.Curation.RequestReview == nil || state.Curation.RequestReview.Revision != input.Curation.Request.Revision || state.Curation.RequestReview.Disposition != disposition || !receipt.Committed {
				t.Fatal("request disposition was not bound and persisted with the receipt")
			}
			if (state.PendingCurationRequest() != nil) != (disposition == "needs_review") {
				t.Fatal("request blocker disagrees with its disposition")
			}
			assertCurationNeeded(t, f, false)
			f.do(func(tx *Tx) error {
				page, err := tx.ScheduleInput("p", 0, "")
				if err == nil && page.CurationRequested {
					t.Fatal("reviewed request kept forcing an urgent curator")
				}
				return err
			})
			if err := state.ValidateCompletionDisputes([]string{fact}); (err != nil) != (disposition == "needs_review") {
				t.Fatalf("wrong completion gate: %v", err)
			}
			replayed := f.action(curator.Fence(), "curate", "review", payload, DecisionStateVersion(input))
			if !reflect.DeepEqual(replayed, receipt) {
				t.Fatal("replaying an acknowledged request changed its receipt")
			}
			if disposition == "needs_review" {
				f.action(f.planner, "curation_request", "recheck", map[string]any{"sources": []string{fact}, "reason": "Recheck after inspecting the omitted context"}, "")
				assertCurationNeeded(t, f, true)
			}
		})
	}
}

func TestCurationRequestReviewRequiresExactRequestedSources(t *testing.T) {
	for _, mode := range []string{"missing", "extra", "duplicate", "stale", "reason"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			run := f.worker("producer", "")
			fact := f.fact(run, "response")
			f.action(f.planner, "curation_request", "request", map[string]any{"sources": []string{fact}, "reason": "Review"}, "")
			curator, input := f.curator("review")
			review := &model.CurationRequestReview{Disposition: "no_change", CheckedSources: []string{fact}, Reason: "Reviewed originals"}
			status := 422
			switch mode {
			case "missing":
				review.CheckedSources = nil
			case "extra":
				review.CheckedSources = append(review.CheckedSources, "absent")
			case "duplicate":
				review.CheckedSources = append(review.CheckedSources, fact)
			case "stale":
				review.Revision, status = input.Revision+1, 409
			case "reason":
				review.Reason = ""
			}
			before := f.state()
			raw, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, RequestReview: review})
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "bad-review", Payload: raw, ExpectedVersion: DecisionStateVersion(input)})
				return err
			})
			requireAPIStatus(t, err, status)
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("invalid review mutated the request")
			}
		})
	}
}

func unresolvedRequestFixture(t *testing.T, invalidate bool) (*orchestrationFixture, string, string, string) {
	t.Helper()
	f := newOrchestrationFixture(t)
	producer := f.worker("original-producer", "")
	old, fresh, other := f.fact(producer, "requested-old"), f.fact(producer, "effective-new"), f.fact(producer, "unrequested-old")
	f.finish(producer, fresh)
	f.action(f.planner, "curation_request", "original-request", map[string]any{"sources": []string{old}, "reason": "Review this observation"}, "")
	curator, input := f.curator("unresolved-review")
	payload := CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, RequestReview: &CurationRequestReview{Disposition: "needs_review", CheckedSources: []string{old}, Reason: "Additional interpretation is required"}}
	if invalidate {
		payload.Relations = []CurateRelation{
			{Kind: "refutes", Source: fresh, Target: old, Reason: "The old observation is contradicted"},
			{Kind: "refutes", Source: fresh, Target: other, Reason: "Another old observation is contradicted"},
		}
	}
	f.action(curator.Fence(), "curate", "unresolved-review", payload, DecisionStateVersion(input))
	assertCurationNeeded(t, f, false)
	return f, old, fresh, other
}

func TestUnresolvedCurationRequestCannotBeReplacedByUnrelatedSources(t *testing.T) {
	f, old, fresh, _ := unresolvedRequestFixture(t, false)
	before := f.state()
	raw, _ := json.Marshal(map[string]any{"sources": []string{fresh}, "reason": "Review unrelated evidence instead"})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", f.planner, StateAction{Op: "curation_request", IdempotencyKey: "unrelated-replacement", Payload: raw})
		return err
	})
	requireAPIStatus(t, err, 409)
	if !reflect.DeepEqual(before, f.state()) || f.state().ValidateCompletionDisputes([]string{old}) == nil {
		t.Fatal("unrelated request removed the unresolved completion blocker")
	}
}

func TestUnresolvedCurationRequestExplicitRetryRearmsIdenticalRequest(t *testing.T) {
	f, old, _, _ := unresolvedRequestFixture(t, false)
	before := f.state()
	request := before.PendingCurationRequest()
	payload := map[string]any{"sources": []string{old}, "reason": request.Reason}
	receipt := f.action(f.planner, "curation_request", "explicit-identical-retry", payload, "")
	after := f.state()
	if receipt.Unchanged || after.Curation.RequestReview != nil || after.Curation.Request.Revision <= request.Revision {
		t.Fatal("an explicit identical retry did not rearm the unresolved request")
	}
	assertCurationNeeded(t, f, true)
	if replay := f.action(f.planner, "curation_request", "explicit-identical-retry", payload, ""); !reflect.DeepEqual(replay, receipt) || !reflect.DeepEqual(after, f.state()) {
		t.Fatal("replaying the explicit retry changed its receipt or rearmed it again")
	}
	if duplicate := f.action(f.planner, "curation_request", "pending-identical-request", payload, ""); !duplicate.Unchanged || !reflect.DeepEqual(after, f.state()) {
		t.Fatal("a duplicate ordinary pending request changed the state")
	}
}

func TestUnrelatedCurationPreservesUnresolvedRequestDisposition(t *testing.T) {
	for _, disposition := range []string{"omitted", "resolved", "no_change", "needs_review"} {
		t.Run(disposition, func(t *testing.T) {
			f, old, _, _ := unresolvedRequestFixture(t, false)
			var candidates []string
			for _, label := range []string{"left", "right"} {
				run := f.worker(label, "")
				fact := f.fact(run, label)
				candidates = append(candidates, f.candidate(run, fact, "verified"))
				f.finish(run, fact)
			}
			assertCurationNeeded(t, f, true)
			curator, input := f.curator("unrelated-groups")
			before := f.state()
			payload := CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{{CandidateIDs: candidates, Status: "verified", Reason: "Independent matching observations"}}}
			if disposition != "omitted" {
				payload.RequestReview = &CurationRequestReview{Disposition: disposition, CheckedSources: []string{old}, Reason: "An unrelated curator cannot replace the original disposition"}
			}
			raw, _ := json.Marshal(payload)
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "unrelated-groups", Payload: raw, ExpectedVersion: DecisionStateVersion(input)})
				return err
			})
			if disposition == "resolved" || disposition == "no_change" {
				requireAPIStatus(t, err, 409)
				if !reflect.DeepEqual(before, f.state()) {
					t.Fatal("unrelated curator changed the request before an explicit re-request")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			after := f.state()
			if !reflect.DeepEqual(after.Curation.RequestReview, before.Curation.RequestReview) || !reflect.DeepEqual(after.Curation.Request, before.Curation.Request) || after.ValidateCompletionDisputes([]string{old}) == nil || len(after.Findings) != 1 {
				t.Fatal("unrelated reconciliation replaced the original unresolved request or failed to process its group")
			}
			assertCurationNeeded(t, f, false)
		})
	}
}

func TestUnresolvedCurationRequestReopensWithHistoricalInvalidSource(t *testing.T) {
	f, old, fresh, other := unresolvedRequestFixture(t, true)
	before := f.state()
	if before.ValidateFactSources([]string{old}, true) == nil {
		t.Fatal("fixture did not invalidate the original requested source")
	}
	for _, sources := range [][]string{{old, other}, {old, old}, {old, "missing"}} {
		raw, _ := json.Marshal(map[string]any{"sources": sources, "reason": "A new source must be effective"})
		err := f.store.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.StateAction("p", f.planner, StateAction{Op: "curation_request", IdempotencyKey: "invalid-extra", Payload: raw})
			return err
		})
		if err == nil || !reflect.DeepEqual(before, f.state()) {
			t.Fatal("invalid added source changed the request")
		}
	}
	request := before.PendingCurationRequest()
	// Retain the original observation identity while adding current evidence.
	receipt := f.action(f.planner, "curation_request", "explicit-retry", map[string]any{"sources": []string{old, fresh}, "reason": request.Reason}, "")
	after := f.state()
	if receipt.Unchanged || after.Curation.RequestReview != nil || after.PendingCurationRequest() == nil || after.Curation.Request.Revision <= request.Revision {
		t.Fatal("historical requested source prevented explicit review retry")
	}
	assertCurationNeeded(t, f, true)
	curator, input := f.curator("explicit-second-review")
	f.action(curator.Fence(), "curate", "finish-second-review", CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, RequestReview: &CurationRequestReview{Disposition: "no_change", CheckedSources: []string{old, fresh}, Reason: "Compared the historical source and current correction"}}, DecisionStateVersion(input))
	if f.state().PendingCurationRequest() != nil {
		t.Fatal("explicitly requested second review could not settle the retained request")
	}
}
