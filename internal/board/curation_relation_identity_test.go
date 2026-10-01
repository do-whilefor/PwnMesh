package board

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCurationRelationIdentityPreservesProvenanceAndEverySubmission(t *testing.T) {
	for _, kind := range []string{"supersedes", "refutes", "narrows"} {
		t.Run(kind, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			old, correction := f.fact(producer, "old"), f.fact(producer, "correction")
			first, input := f.curator("first")
			edge := CurateRelation{Kind: kind, Source: correction, Target: old, Reason: "The controlled observation corrects the original"}
			rephrased := edge
			rephrased.Reason = "A second wording of this same correction in one batch"
			payload := CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{edge, rephrased}}
			firstReceipt := f.action(first.Fence(), "curate", "first-batch", payload, DecisionStateVersion(input))
			firstState := f.state()
			if len(firstState.FactRelations) != 1 || firstState.FactRelations[0].Reason != edge.Reason || firstState.FactRelations[0].RunID != first.Lease {
				t.Fatalf("same-batch duplicate changed the canonical edge: %+v", firstState.FactRelations)
			}
			original := firstState.FactRelations[0]
			f.finish(first, "")
			later := f.store.Now().Add(time.Minute)
			f.store.Now = func() time.Time { return later }
			f.fact(producer, "later-observation")
			second, nextInput := f.curator("second")
			rephrased.Reason = "A later curator independently proposes the same correction"
			secondPayload := CuratePayload{ThroughRevision: nextInput.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{rephrased}}
			secondReceipt := f.action(second.Fence(), "curate", "second-batch", secondPayload, DecisionStateVersion(nextInput))
			state := f.state()
			if !firstReceipt.Committed || !secondReceipt.Committed || secondReceipt.Revision != nextInput.Revision+1 || state.Curation.ThroughRevision != nextInput.Revision || state.Curation.RunID != second.Lease {
				t.Fatal("deduplication lost an accepted curation or its input acknowledgement")
			}
			if len(state.FactRelations) != 1 || !reflect.DeepEqual(state.FactRelations[0], original) || !reflect.DeepEqual(state.Graph.Facts, nextInput.Graph.Facts) || !reflect.DeepEqual(state.FactRecords, nextInput.FactRecords) {
				t.Fatal("cross-run rewording duplicated an edge or rewrote original evidence/provenance")
			}
			if replay := f.action(second.Fence(), "curate", "second-batch", secondPayload, DecisionStateVersion(nextInput)); !reflect.DeepEqual(replay, secondReceipt) || !reflect.DeepEqual(f.state(), state) {
				t.Fatal("replaying a deduplicated batch changed its receipt or state")
			}
			// A stable idempotency key still binds exact submitted values. Semantic
			// edge deduplication does not permit rewriting a previous request.
			changedPayload := secondPayload
			changedPayload.Relations = []CurateRelation{{Kind: kind, Source: correction, Target: old, Reason: "An incompatible replay under the same key"}}
			raw, _ := json.Marshal(changedPayload)
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", second.Fence(), StateAction{Op: "curate", IdempotencyKey: "second-batch", Payload: raw, ExpectedVersion: DecisionStateVersion(nextInput)})
				return err
			})
			requireAPIStatus(t, err, 409)
			if !reflect.DeepEqual(f.state(), state) {
				t.Fatal("conflicting replay changed persisted state")
			}
			// Both proposals remain auditable even though the projection has one
			// edge. Verify the durable requests, event payloads and both receipts.
			verifyHistory := func() {
				f.do(func(tx *Tx) error {
					events, err := tx.StateEvents("p", input.Revision)
					if err != nil {
						return err
					}
					curations := []StateEvent{}
					for _, event := range events {
						if event.Op == "curate" {
							curations = append(curations, event)
						}
					}
					if len(curations) != 2 || curations[0].RunID != first.Lease || curations[1].RunID != second.Lease {
						t.Fatal("semantic deduplication erased a submitting execution's event")
					}
					for n, entry := range []struct {
						run, key string
						payload  CuratePayload
						receipt  StateActionResult
					}{{first.Lease, "first-batch", payload, firstReceipt}, {second.Lease, "second-batch", secondPayload, secondReceipt}} {
						var proposal CuratePayload
						if json.Unmarshal(curations[n].Payload, &proposal) != nil || !reflect.DeepEqual(proposal, entry.payload) {
							t.Fatal("event history lost a curator's original reasoning")
						}
						var request string
						if err := tx.QueryRow("SELECT request FROM xloom_state_actions WHERE project_id='p' AND idempotency_key=?", entry.key).Scan(&request); err != nil {
							return err
						}
						var savedRequest struct {
							Run     string
							Payload CuratePayload
						}
						if json.Unmarshal([]byte(request), &savedRequest) != nil || savedRequest.Run != entry.run || !reflect.DeepEqual(savedRequest.Payload, entry.payload) {
							t.Fatal("durable request lost its original submitter or payload")
						}
						saved, err := tx.CurationReceipt("p", entry.run)
						if err != nil {
							return err
						}
						if !reflect.DeepEqual(saved, entry.receipt) {
							t.Fatal("a later duplicate replaced the original receipt")
						}
						var result CurateResult
						if json.Unmarshal(saved.Result, &result) != nil || !reflect.DeepEqual(result.FactRelations, []FactRelation{original}) {
							t.Fatal("receipt returned a duplicate or rewritten canonical edge")
						}
					}
					return nil
				})
			}
			verifyHistory()
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			f.store, err = Open(f.path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f.state(), state) {
				t.Fatal("reopening storage changed the canonical relation")
			}
			verifyHistory()
		})
	}
}

func TestCurationRelationDuplicateStillValidatesSourcesAndVersion(t *testing.T) {
	for _, mode := range []string{"invalidated_source", "stale_input", "forged_current_version", "empty_reason"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			old, correction, fresh := f.fact(producer, "old"), f.fact(producer, "correction"), f.fact(producer, "fresh")
			first, input := f.curator("first")
			edge := CurateRelation{Kind: "supersedes", Source: correction, Target: old, Reason: "Original correction"}
			f.action(first.Fence(), "curate", "first-batch", CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{edge}}, DecisionStateVersion(input))
			f.finish(first, "")
			second, nextInput := f.curator("second")
			edge.Reason = "Restated correction"
			payload := CuratePayload{ThroughRevision: nextInput.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{edge}}
			version, want := DecisionStateVersion(nextInput), 409
			switch mode {
			case "invalidated_source":
				payload.Relations = append([]CurateRelation{{Kind: "refutes", Source: fresh, Target: correction, Reason: "New evidence invalidates the previously corrective source"}}, payload.Relations...)
			case "stale_input", "forged_current_version":
				f.fact(producer, "concurrent-observation")
				if mode == "forged_current_version" {
					version = DecisionStateVersion(f.state())
				}
			case "empty_reason":
				payload.Relations[0].Reason = ""
				want = 422
			}
			before := f.state()
			raw, _ := json.Marshal(payload)
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", second.Fence(), StateAction{Op: "curate", IdempotencyKey: "rejected-duplicate", Payload: raw, ExpectedVersion: version})
				return err
			})
			requireAPIStatus(t, err, want)
			if mode == "stale_input" && !strings.Contains(err.Error(), "state_changed:") {
				t.Fatal("duplicate bypassed the immutable input version check")
			}
			if !reflect.DeepEqual(f.state(), before) {
				t.Fatal("invalid duplicate partially applied relations or curation progress")
			}
			f.do(func(tx *Tx) error {
				_, err := tx.CurationReceipt("p", second.Lease)
				requireAPIStatus(t, err, 404)
				return nil
			})
		})
	}
}

func TestCurationRelationIdentityIncludesKindSourceAndTarget(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	old, other, correction, fresh := f.fact(producer, "old"), f.fact(producer, "other"), f.fact(producer, "correction"), f.fact(producer, "fresh")
	curator, input := f.curator("curator")
	relations := []CurateRelation{
		{Kind: "supersedes", Source: correction, Target: old, Reason: "Correct the original"},
		{Kind: "refutes", Source: correction, Target: old, Reason: "Also explicitly refute the original"},
		{Kind: "supersedes", Source: fresh, Target: old, Reason: "Separate corrective evidence"},
		{Kind: "supersedes", Source: correction, Target: other, Reason: "A separate affected observation"},
	}
	f.action(curator.Fence(), "curate", "distinct-edges", CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, Relations: relations}, DecisionStateVersion(input))
	if got := len(f.state().FactRelations); got != len(relations) {
		t.Fatalf("different relationship kinds or endpoints were collapsed: got %d want %d", got, len(relations))
	}
}
