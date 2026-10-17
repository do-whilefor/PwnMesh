package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board/model"
)

type curationProjectionQueries struct {
	transactionQueries
	executionReads, assetReads int
}

func (q *curationProjectionQueries) Query(query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "FROM intents i JOIN pwnmesh_executions") {
		q.executionReads++
	}
	if strings.Contains(query, "FROM pwnmesh_asset_anchors n JOIN pwnmesh_assets") {
		q.assetReads++
	}
	return q.transactionQueries.Query(query, args...)
}

func TestCurationProspectiveCorrectionsLoadProjectionInputsOnce(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("projection-inputs", "")
	old, middle, fresh := f.fact(producer, "old"), f.fact(producer, "middle"), f.fact(producer, "fresh")
	curator, input := f.curator("projection-curator")
	raw, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{
		{Kind: "refutes", Source: fresh, Target: old, Reason: "A fresh independent observation corrects the old one"},
		{Kind: "refutes", Source: fresh, Target: middle, Reason: "The same observation also corrects the intermediate result"},
	}})
	f.do(func(tx *Tx) error {
		data, _, _, err := tx.stateData("p")
		if err != nil {
			return err
		}
		queries := &curationProjectionQueries{transactionQueries: tx.transactionQueries}
		tx.transactionQueries = queries
		_, _, err = tx.curate(input, &data, curator.Fence(), raw)
		if err != nil {
			return err
		}
		if queries.executionReads != 1 || queries.assetReads != 1 || len(data.FactRelations) != 2 {
			t.Fatalf("prospective corrections reloaded stable inputs: executions=%d assets=%d relations=%d", queries.executionReads, queries.assetReads, len(data.FactRelations))
		}
		return nil
	})
}

func TestPartialCurationWithRelationRetainsCoveredGroupProgress(t *testing.T) {
	for _, withRelation := range []bool{false, true} {
		name := "groups_only"
		if withRelation {
			name = "groups_and_relation"
		}
		t.Run(name, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("fresh-notes", "")
			old, correction := f.fact(producer, "old"), f.fact(producer, "correction")
			seedCurationGroups(f, 2, 2, "")
			groups := []CurateGroup{
				{GroupKey: model.FindingIdentity("Capacity claim 0", "fixture"), Status: "candidate", Reason: "Retain the first complete group"},
				{GroupKey: model.FindingIdentity("Capacity claim 1", "fixture"), Status: "candidate", Reason: "Retain the second complete group"},
			}
			curator, input := f.curator("initial-findings")
			f.curate(curator, input, groups...)
			for _, claim := range []string{"Capacity claim 0", "Capacity claim 1"} {
				f.action(producer.Fence(), "candidate", "fresh:"+claim, map[string]any{
					"claim": claim, "scope": "fixture", "status": "candidate", "reason": "A fresh producer note needs reconciliation",
				}, "")
			}
			curator, input = f.curator("partial-with-fresh-notes")
			if len(input.Findings) != 2 || len(input.PendingCurationCandidateIDs()) != 6 {
				t.Fatal("fixture did not establish two existing findings with pending notes")
			}
			payload := CuratePayload{ThroughRevision: input.Revision, Groups: groups[:1]}
			if withRelation {
				// This correction is unrelated to either group's support. Its
				// intermediate projection must not erase progress on the first group.
				payload.Relations = []CurateRelation{{Kind: "refutes", Source: correction, Target: old, Reason: "Correct an unrelated observation"}}
			}
			receipt := f.action(curator.Fence(), "curate", "partial", payload, DecisionStateVersion(input))
			partial := f.state()
			pending := partial.PendingCurationCandidateIDs()
			if len(pending) != 3 || partial.Curation.ThroughRevision != input.Curation.ThroughRevision {
				t.Fatalf("partial batch lost its remaining group or advanced the global cursor: pending=%d cursor=%d", len(pending), partial.Curation.ThroughRevision)
			}
			for _, candidate := range partial.Candidates {
				if pending[candidate.ID] && candidate.Claim != "Capacity claim 1" {
					t.Fatal("the successfully covered first group remained pending")
				}
			}
			var result CurateResult
			if err := json.Unmarshal(receipt.Result, &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Findings) != 1 || result.Findings[0].ID != groups[0].GroupKey || len(result.FactRelations) != len(payload.Relations) {
				t.Fatal("partial receipt lost the covered group or included an unsubmitted group")
			}
			if replay := f.action(curator.Fence(), "curate", "partial", payload, DecisionStateVersion(input)); !reflect.DeepEqual(replay, receipt) {
				t.Fatal("partial curation receipt changed on retry")
			}
			curator, input = f.curator("remaining-fresh-notes")
			f.curate(curator, input, groups[1])
			if final := f.state(); len(final.PendingCurationCandidateIDs()) != 0 || final.Curation.ThroughRevision != input.Revision {
				t.Fatal("the final group did not drain the retained curation backlog")
			}
		})
	}
}

func TestCurationRelationsCommitWithFindingsAndCursor(t *testing.T) {
	for _, kind := range []string{"supersedes", "refutes", "narrows"} {
		t.Run(kind, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			old, correction := f.fact(producer, "old"), f.fact(producer, "correction")
			candidate := f.candidate(producer, old, "verified")
			child := f.action(f.planner, "step", "dependent", map[string]any{"action": "add", "from": []string{old}, "description": "Use the original observation"}, "").ID
			curator, input := f.curator("curator")
			var originalFacts []FactRecord
			f.do(func(tx *Tx) error {
				data, _, _, err := tx.stateData("p")
				originalFacts = data.Facts
				return err
			})
			payload := CuratePayload{ThroughRevision: input.Revision,
				Relations: []CurateRelation{{Kind: kind, Source: correction, Target: old, Reason: "A fresh observation corrects the original scope"}},
				Groups:    []CurateGroup{{CandidateIDs: []string{candidate}, Status: "candidate", Reason: "The original supporting observation is no longer current"}}}
			receipt := f.action(curator.Fence(), "curate", "atomic-curation", payload, DecisionStateVersion(input))
			var result CurateResult
			if err := json.Unmarshal(receipt.Result, &result); err != nil {
				t.Fatal(err)
			}
			state := f.state()
			if !receipt.Committed || receipt.Revision != input.Revision+1 || state.Curation.ThroughRevision != input.Revision || len(state.FactRelations) != 1 || len(result.FactRelations) != 1 || state.FactRelations[0].RunID != curator.Lease {
				t.Fatalf("relations and curation did not share one receipt and boundary: %+v", state)
			}
			if len(state.Findings) != 1 || state.Findings[0].Status != "candidate" || state.Findings[0].SupportValid {
				t.Fatal("curation ignored its own correction when checking finding support")
			}
			if err := state.ValidateFactSources([]string{old}, true); err == nil {
				t.Fatal("corrected observation remained effective evidence")
			}
			for _, step := range state.Steps {
				if step.ID == child && (step.Status != "needs_review" || len(step.InvalidSources) != 1 || step.InvalidSources[0] != old) {
					t.Fatalf("dependent task did not see the atomic correction: %+v", step)
				}
			}
			f.do(func(tx *Tx) error {
				data, _, _, err := tx.stateData("p")
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(data.Facts, originalFacts) || !reflect.DeepEqual(state.Candidates, input.Candidates) {
					t.Fatal("curation rewrote a producer's raw observation or judgment")
				}
				events, err := tx.StateEvents("p", input.Revision)
				if err == nil && (len(events) != 1 || events[0].Op != "curate") {
					t.Fatalf("atomic correction has inconsistent event history: %+v", events)
				}
				return err
			})
			replay := f.action(curator.Fence(), "curate", "atomic-curation", payload, DecisionStateVersion(input))
			if !reflect.DeepEqual(replay, receipt) || f.state().Revision != state.Revision || len(f.state().FactRelations) != 1 {
				t.Fatal("replaying the curation duplicated its relation or changed the receipt")
			}
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			f.store, err = Open(f.path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f.state(), state) {
				t.Fatal("atomic relation projection changed after reopening storage")
			}
			f.do(func(tx *Tx) error {
				saved, err := tx.CurationReceipt("p", curator.Lease)
				if err == nil && !reflect.DeepEqual(saved, receipt) {
					t.Fatal("atomic relation receipt was not durable")
				}
				return err
			})
		})
	}
}

func TestCurationRelationOnlyReceiptDoesNotRepeatProjectedFindings(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	old, correction := f.fact(producer, "old"), f.fact(producer, "correction")
	candidate := f.candidate(producer, old, "verified")
	unrelated := f.action(producer.Fence(), "candidate", "unrelated", map[string]any{"claim": "An unrelated observation", "scope": "fixture", "status": "verified", "sources": []string{correction}, "reason": "Independently supported"}, "").ID
	curator, input := f.curator("initial-findings")
	f.curate(curator, input,
		CurateGroup{CandidateIDs: []string{candidate}, Status: "verified", Reason: "Initial supported judgment"},
		CurateGroup{CandidateIDs: []string{unrelated}, Status: "verified", Reason: "Retain the independent judgment"})
	curator, input = f.curator("correct-support")
	if len(input.Findings) != 2 || !input.Findings[0].SupportValid || !input.Findings[1].SupportValid {
		t.Fatal("fixture did not establish two supported findings")
	}
	payload := CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{{Kind: "refutes", Source: correction, Target: old, Reason: "Correct the original premise"}}}
	receipt := f.action(curator.Fence(), "curate", "correct-support", payload, DecisionStateVersion(input))
	var result CurateResult
	if err := json.Unmarshal(receipt.Result, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Findings)+len(result.Disputes) != 0 || len(result.FactRelations) != 1 || result.FactRelations[0].Target != old {
		t.Fatalf("relation-only receipt repeated unsubmitted groups or lost its relation: %+v", result)
	}
	f.do(func(tx *Tx) error {
		data, _, _, err := tx.stateData("p")
		if err != nil {
			return err
		}
		if len(data.Findings) != 2 || data.Findings[0].SupportValid || !data.Findings[1].SupportValid {
			t.Fatal("relation-only curation did not persist the finding support change")
		}
		events, err := tx.StateEvents("p", input.Revision)
		if err == nil && (len(events) != 1 || string(events[0].Result) != string(receipt.Result)) {
			t.Fatal("event changed the bounded relation-only receipt")
		}
		return err
	})
	if replay := f.action(curator.Fence(), "curate", "correct-support", payload, DecisionStateVersion(input)); !reflect.DeepEqual(replay, receipt) {
		t.Fatal("support invalidation changed its durable receipt during replay")
	}
}

func TestCurationRelationsRejectInvalidBatchWithoutAnyCommit(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
	}{
		{"omitted_candidate", 409}, {"unsupported_finding", 409}, {"invalidated_source", 409},
		{"unknown_source", 404}, {"protected_origin", 403}, {"protected_goal", 400},
		{"self_relation", 422}, {"unknown_kind", 422}, {"relation_limit", 422},
		{"stale_input", 409}, {"forged_current_version", 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			old, correction, third := f.fact(producer, "old"), f.fact(producer, "correction"), f.fact(producer, "third")
			candidate := f.candidate(producer, old, "verified")
			if test.name == "omitted_candidate" {
				other := f.worker("second-producer", "")
				f.candidate(other, f.fact(other, "parallel-observation"), "verified")
			}
			curator, input := f.curator("curator")
			payload := CuratePayload{ThroughRevision: input.Revision,
				Relations: []CurateRelation{{Kind: "refutes", Source: correction, Target: old, Reason: "Correct the original observation"}},
				Groups:    []CurateGroup{{CandidateIDs: []string{candidate}, Status: "candidate", Reason: "The supporting observation was corrected"}}}
			version := DecisionStateVersion(input)
			switch test.name {
			case "omitted_candidate":
				payload.Groups = []CurateGroup{}
			case "unsupported_finding":
				payload.Groups[0].Status = "verified"
			case "invalidated_source":
				payload.Relations = append(payload.Relations, CurateRelation{Kind: "narrows", Source: old, Target: third, Reason: "Cannot use a just-invalidated observation"})
			case "unknown_source":
				payload.Relations[0].Source = "missing"
			case "protected_origin":
				payload.Relations[0].Target = "origin"
			case "protected_goal":
				payload.Relations[0].Target = "goal"
			case "self_relation":
				payload.Relations[0].Target = correction
			case "unknown_kind":
				payload.Relations[0].Kind = "replace"
			case "relation_limit":
				for len(payload.Relations) <= MaxCurationRelations {
					payload.Relations = append(payload.Relations, payload.Relations[0])
				}
			case "stale_input", "forged_current_version":
				f.fact(producer, "later-observation")
				if test.name == "forged_current_version" {
					version = DecisionStateVersion(f.state())
				}
			}
			before, _ := json.Marshal(f.state())
			raw, _ := json.Marshal(payload)
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "rejected", Payload: raw, ExpectedVersion: version})
				return err
			})
			requireAPIStatus(t, err, test.status)
			after, _ := json.Marshal(f.state())
			if string(before) != string(after) {
				t.Fatal("rejected batch changed relations, finding, original evidence or cursor")
			}
			f.do(func(tx *Tx) error {
				_, err := tx.CurationReceipt("p", curator.Lease)
				requireAPIStatus(t, err, 404)
				return nil
			})
		})
	}
}

func TestCurationRelationCannotUseTransitivelyInvalidatedResult(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	upstream := f.fact(producer, "upstream")
	f.finish(producer, upstream)
	child := dependentStep(f, "consumer", producer.Intent)
	consumer := dependencyRun(f, "consumer", child)
	f.do(func(tx *Tx) error { return tx.RegisterExecution(consumer) })
	downstream := f.fact(consumer, "downstream")
	f.finish(consumer, downstream)
	corrector := f.worker("corrector", "")
	correction := f.fact(corrector, "correction")
	f.finish(corrector, correction)
	curator, input := f.curator("curator")
	raw, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{
		{Kind: "refutes", Source: correction, Target: upstream, Reason: "Invalidate the accepted upstream result"},
		{Kind: "refutes", Source: downstream, Target: correction, Reason: "The downstream result has just lost its prerequisite"},
	}})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "invalid-transitive-source", Payload: raw, ExpectedVersion: DecisionStateVersion(input)})
		return err
	})
	requireAPIStatus(t, err, 409)
	if !reflect.DeepEqual(f.state(), input) {
		t.Fatal("failed transitive support validation persisted part of the batch")
	}
}

func TestCurationRelationsCannotResolveDisputeWithoutReview(t *testing.T) {
	f := newOrchestrationFixture(t)
	dispute, candidates, first := f.conflict()
	before := f.state()
	second := before.Candidates[1].Sources[0]
	curator, input := f.curator("relations-on-dispute")
	f.action(curator.Fence(), "curate", "cannot-vote-away-dispute", CuratePayload{ThroughRevision: input.Revision,
		Relations: []CurateRelation{{Kind: "refutes", Source: second, Target: first, Reason: "One conflicting observation challenges the other"}},
		Groups:    []CurateGroup{{CandidateIDs: candidates, Status: "verified", Reason: "The dispute still needs independent evidence", DisputeID: dispute.ID}},
	}, DecisionStateVersion(input))
	f.finish(curator, "")
	after := f.state()
	if len(after.Disputes) != 1 || after.Disputes[0].Status != "open" || after.Findings[0].Status != "candidate" || !reflect.DeepEqual(before.Candidates, after.Candidates) || !reflect.DeepEqual(before.Disputes[0].CandidateIDs, after.Disputes[0].CandidateIDs) {
		t.Fatal("a relation replaced independent review or erased a disputed producer")
	}
	f.do(func(tx *Tx) error {
		requireAPIStatus(t, tx.ValidateStateCompletion("p", []string{second}), 409)
		return nil
	})
}

func TestCurationResolutionChecksRelationsBeforeReviewEvidence(t *testing.T) {
	f := newOrchestrationFixture(t)
	dispute, candidates, original := f.conflict()
	review := f.worker("review", f.step("independent review", dispute.ID))
	result := f.fact(review, "review-evidence")
	f.finish(review, result)
	curator, input := f.curator("invalidated-review")
	raw, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision,
		Relations: []CurateRelation{{Kind: "refutes", Source: original, Target: result, Reason: "The review observation cannot support this resolution"}},
		Groups:    []CurateGroup{{CandidateIDs: candidates, Status: "verified", Reason: "Attempt to resolve using evidence invalidated in this batch", DisputeID: dispute.ID, ReviewFactIDs: []string{result}, Resolution: "resolved"}},
	})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "invalid-review-support", Payload: raw, ExpectedVersion: DecisionStateVersion(input)})
		return err
	})
	requireAPIStatus(t, err, 409)
	if !reflect.DeepEqual(f.state(), input) {
		t.Fatal("rejected resolution changed the dispute, its evidence or curation boundary")
	}
}

func TestCurationRelationsReopenResolutionWithInvalidatedReviewSupport(t *testing.T) {
	f := newOrchestrationFixture(t)
	dispute, candidates, _ := f.conflict()
	failedStep := f.step("failed independent review", dispute.ID)
	failedReview := f.worker("failed-review", failedStep)
	f.do(func(tx *Tx) error {
		return tx.ExecutionStatus(failedReview, "failed", json.RawMessage(`{"status":"failed"}`))
	})
	firstStep := f.step("first review", dispute.ID)
	firstReview := f.worker("first-review", firstStep)
	firstFact := f.fact(firstReview, "first-review-evidence")
	f.finish(firstReview, firstFact)
	curator, input := f.curator("first-resolution")
	firstReceipt := f.curate(curator, input, CurateGroup{CandidateIDs: candidates, Status: "verified", Reason: "First independent result", DisputeID: dispute.ID, ReviewFactIDs: []string{firstFact}, Resolution: "resolved"})
	f.finish(curator, "")
	corrector := f.worker("corrector", "")
	correction := f.fact(corrector, "correction-of-review")
	f.finish(corrector, correction)
	curator, input = f.curator("invalidate-review-support")
	receipt := f.action(curator.Fence(), "curate", "reopen-reviewed-dispute", CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{{Kind: "refutes", Source: correction, Target: firstFact, Reason: "The independent review evidence no longer supports its conclusion"}}}, DecisionStateVersion(input))
	var changed CurateResult
	if json.Unmarshal(receipt.Result, &changed) != nil || len(changed.Findings) != 1 || changed.Findings[0].Status != "candidate" || len(changed.Disputes) != 1 || changed.Disputes[0].Status != "open" {
		t.Fatal("relation-only receipt omitted the reopened finding or dispute")
	}
	f.finish(curator, "")
	reopened := f.state()
	if reopened.Disputes[0].Status != "open" || len(reopened.Disputes[0].ReviewFactIDs) != 0 || !reflect.DeepEqual(reopened.Disputes[0].ReviewStepIDs, []string{failedStep, firstStep}) || reopened.Disputes[0].ReviewStepOffset != 2 || reopened.Findings[0].Status != "candidate" || reopened.Findings[0].SupportValid {
		t.Fatalf("invalidated review did not reopen its disputed conclusion: %+v", reopened.Disputes)
	}
	f.do(func(tx *Tx) error {
		requireAPIStatus(t, tx.ValidateStateCompletion("p", []string{correction}), 409)
		historical, err := tx.CurationReceipt("p", "curator@first-resolution")
		if err == nil && !reflect.DeepEqual(historical, firstReceipt) {
			t.Fatal("reopening changed the original independent review receipt")
		}
		return err
	})
	curator, input = f.curator("reject-old-review")
	raw, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{{CandidateIDs: candidates, Status: "verified", Reason: "Cannot reuse the invalidated review", DisputeID: dispute.ID, ReviewFactIDs: []string{firstFact}, Resolution: "resolved"}}})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "old-review-reuse", Payload: raw, ExpectedVersion: DecisionStateVersion(input)})
		return err
	})
	requireAPIStatus(t, err, 409)
	if !reflect.DeepEqual(f.state(), input) {
		t.Fatal("old review silently closed the reopened dispute")
	}
	f.do(func(tx *Tx) error { return tx.ReleaseCurator("p", curator.Lease) })
	secondStep := f.step("second independent review", dispute.ID)
	secondReview := f.worker("second-review", secondStep)
	secondFact := f.fact(secondReview, "fresh-review-evidence")
	f.finish(secondReview, secondFact)
	curator, input = f.curator("fresh-resolution")
	f.curate(curator, input, CurateGroup{CandidateIDs: candidates, Status: "refuted", Reason: "Fresh independent evidence supersedes the unsupported resolution", DisputeID: dispute.ID, ReviewFactIDs: []string{secondFact}, Resolution: "resolved"})
	f.finish(curator, "")
	final := f.state()
	if final.Disputes[0].Status != "resolved" || !reflect.DeepEqual(final.Disputes[0].ReviewStepIDs, []string{failedStep, firstStep, secondStep}) || !reflect.DeepEqual(final.Disputes[0].ReviewFactIDs, []string{secondFact}) || final.Findings[0].Status != "refuted" || !final.Findings[0].SupportValid {
		t.Fatalf("fresh review did not restore a supported resolution and its history: %+v", final.Disputes)
	}
}
