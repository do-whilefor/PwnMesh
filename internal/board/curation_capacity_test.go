package board

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"pwnmesh/internal/board/model"
)

// Seed individually valid notes together so capacity tests do not spend their
// time repeatedly serializing every intermediate fixture state.
func seedCurationGroups(f *orchestrationFixture, groups, notes int, source string) {
	f.t.Helper()
	f.do(func(tx *Tx) error {
		d, revision, decision, err := tx.stateData("p")
		if err != nil {
			return err
		}
		for group := range groups {
			for note := range notes {
				candidate := Candidate{ID: fmt.Sprintf("capacity_%d_%d", group, note), Claim: fmt.Sprintf("Capacity claim %d", group), Scope: "fixture", Status: "candidate", Sources: []string{}, Evidence: []EvidenceRef{}, Reason: "Retain the original producer judgment", RunID: fmt.Sprintf("worker@producer%d", note%2), Generation: 0, Revision: revision + 1, CreatedAt: tx.Now}
				if source != "" {
					candidate.Sources, candidate.Status = []string{source}, "verified"
					if note == notes-1 {
						candidate.Status = "refuted"
					}
				}
				d.Candidates = append(d.Candidates, candidate)
			}
		}
		raw, err := json.Marshal(d)
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO pwnmesh_state(project_id,data,revision,decision_revision) VALUES(?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET data=excluded.data,revision=excluded.revision,decision_revision=excluded.decision_revision", "p", raw, revision+1, decision+1)
		return err
	})
}

func capacityCurator(f *orchestrationFixture, label string) (Execution, State) {
	f.t.Helper()
	e := Execution{ProjectID: "p", ID: label, Namespace: "test", Backend: "curator", Kind: "curate", Lease: "curator@" + label}
	var state State
	f.do(func(tx *Tx) error {
		if _, err := tx.ClaimCurator("p", e.Lease, "capacity"); err != nil {
			return err
		}
		var err error
		state, err = tx.State("p")
		if err != nil {
			return err
		}
		ref, err := tx.FreezeInput(state)
		if err != nil {
			return err
		}
		view, err := CurationContextView(state, DefaultContextViewBytes)
		if err != nil {
			return err
		}
		e.Job, err = json.Marshal(map[string]any{"kind": "curate", "run_id": e.ID, "graph": Graph{Project: state.Graph.Project}, "input_snapshot": ref, "input_view": view, "graph_rpc": true, "result_contract_version": 2})
		if err != nil {
			return err
		}
		e.RetryKey = CurationRetryKey(state)
		return tx.RegisterExecution(e)
	})
	return e, state
}

func TestCurationCapacityDrainsWholeGroupsAcrossExecutions(t *testing.T) {
	f := newOrchestrationFixture(t)
	seedCurationGroups(f, 129, 2, "")
	e, input := capacityCurator(f, "first-page")
	groups := []CurateGroup{}
	for group := range 128 {
		groups = append(groups, CurateGroup{CandidateIDs: []string{fmt.Sprintf("capacity_%d_0", group), fmt.Sprintf("capacity_%d_1", group)}, Status: "candidate", Reason: "Retain tentative observations"})
	}
	f.curate(e, input, groups...)
	partial := f.state()
	if len(partial.Findings) != 128 || len(partial.PendingCurationCandidateIDs()) != 2 || partial.Curation.ThroughRevision != input.Curation.ThroughRevision {
		t.Fatalf("partial batch lost its remaining work: findings=%d pending=%d cursor=%d", len(partial.Findings), len(partial.PendingCurationCandidateIDs()), partial.Curation.ThroughRevision)
	}
	assertCurationNeeded(t, f, true)
	e, input = capacityCurator(f, "second-page")
	f.curate(e, input, CurateGroup{CandidateIDs: []string{"capacity_128_0", "capacity_128_1"}, Status: "candidate", Reason: "Finish the remaining identity group"})
	final := f.state()
	if len(final.Findings) != 129 || len(final.PendingCurationCandidateIDs()) != 0 || final.Curation.ThroughRevision != input.Revision {
		t.Fatal("incremental curation did not fully drain the backlog")
	}
	assertCurationNeeded(t, f, false)
}

func TestCurationReceiptsRetainOnlyTheSubmittedGroups(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("source", "")
	source, old, correction := f.fact(producer, "support"), f.fact(producer, "old"), f.fact(producer, "correction")
	previous, prior := capacityCurator(f, "previous-relation")
	f.action(previous.Fence(), "curate", "previous-relation", CuratePayload{ThroughRevision: prior.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{{Kind: "refutes", Source: correction, Target: old, Reason: "An unrelated earlier correction"}}}, DecisionStateVersion(prior))
	seedCurationGroups(f, 8, 2, source)
	for group := range 8 {
		e, input := capacityCurator(f, fmt.Sprintf("batch-%d", group))
		key := model.FindingIdentity(fmt.Sprintf("Capacity claim %d", group), "fixture")
		payload := CurateGroup{GroupKey: key, Status: "candidate", Reason: "Retain this complete group", Question: "Which producer observation is supported?"}
		receipt := f.curate(e, input, payload)
		var result CurateResult
		if err := json.Unmarshal(receipt.Result, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Findings) != 1 || result.Findings[0].ID != key || len(result.Disputes) != 1 || result.Disputes[0].FindingID != key || len(result.FactRelations) != 0 {
			t.Fatalf("batch %d retained unrelated historical records: %+v", group, result)
		}
		// A lost response must replay this batch even after its lease settles.
		replay := f.curate(e, input, payload)
		if string(replay.Result) != string(receipt.Result) || replay.Revision != receipt.Revision {
			t.Fatal("bounded receipt changed during replay")
		}
	}
	f.do(func(tx *Tx) error {
		events, err := tx.StateEvents("p", 0)
		if err != nil {
			return err
		}
		count := 0
		for _, event := range events {
			if event.Op != "curate" {
				continue
			}
			var result CurateResult
			if err := json.Unmarshal(event.Result, &result); err != nil {
				return err
			}
			count += len(result.Findings)
		}
		if count != 8 {
			t.Fatalf("events retained %d findings for eight submitted groups", count)
		}
		return nil
	})
	if state := f.state(); len(state.Findings) != 8 || len(state.Disputes) != 8 || len(state.FactRelations) != 1 {
		t.Fatal("bounded receipts removed canonical findings, disputes or relations")
	}
}

func TestCurationGroupKeyIncludesConflictsBeyondExplicitIDLimit(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("support", "")
	fact := f.fact(producer, "support")
	seedCurationGroups(f, 1, 257, fact)
	e, input := capacityCurator(f, "large-group")
	key := input.CandidateView(input.Candidates[0]).GroupKey
	group := map[string]any{"group_key": key, "status": "verified", "reason": "Assess the complete identity group"}
	try := func(key string, group map[string]any) error {
		raw, _ := json.Marshal(map[string]any{"through_revision": input.Revision, "groups": []any{group}})
		return f.store.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.StateAction("p", e.Fence(), StateAction{Op: "curate", IdempotencyKey: key, Payload: raw, ExpectedVersion: DecisionStateVersion(input)})
			return err
		})
	}
	requireAPIStatus(t, try("cannot-hide-last-note", group), 422)
	group["question"] = "Which original observation holds for this scope?"
	if err := try("include-all-notes", group); err != nil {
		t.Fatal(err)
	}
	state := f.state()
	if len(state.Disputes) != 1 || len(state.Disputes[0].CandidateIDs) != 257 || state.Disputes[0].Status != "open" || state.Findings[0].Status != "candidate" || len(state.PendingCurationCandidateIDs()) != 0 {
		t.Fatal("compact group selection omitted an opposing note or skipped independent review")
	}
}

func TestPartialCurationRetainsRequestAndRejectsNonprogressingBatch(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("request-source", "")
	fact := f.fact(producer, "request-source")
	seedCurationGroups(f, 2, 2, "")
	f.action(f.planner, "curation_request", "request", map[string]any{"sources": []string{fact}, "reason": "Review both pending groups before accepting the observations"}, "")
	e, input := capacityCurator(f, "partial-request")
	group := CurateGroup{GroupKey: model.FindingIdentity("Capacity claim 0", "fixture"), Status: "candidate", Reason: "Retain the complete first group"}
	f.curate(e, input, group)
	partial := f.state()
	if partial.PendingCurationRequest() == nil || partial.Curation.ThroughRevision != input.Curation.ThroughRevision {
		t.Fatal("partial curation acknowledged the complete request")
	}
	e, input = capacityCurator(f, "remaining-request")
	for _, groups := range [][]CurateGroup{{}, {group}, {{GroupKey: group.GroupKey, CandidateIDs: []string{"capacity_0_0"}, Status: "candidate", Reason: "Ambiguous selector"}}} {
		raw, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: groups})
		err := f.store.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.StateAction("p", e.Fence(), StateAction{Op: "curate", IdempotencyKey: "no-progress", Payload: raw, ExpectedVersion: DecisionStateVersion(input)})
			return err
		})
		if err == nil {
			t.Fatal("accepted an ambiguous or nonprogressing partial batch")
		}
	}
	f.curate(e, input, CurateGroup{GroupKey: model.FindingIdentity("Capacity claim 1", "fixture"), Status: "candidate", Reason: "Finish the remaining group"})
	if final := f.state(); final.PendingCurationRequest() != nil || len(final.PendingCurationCandidateIDs()) != 0 {
		t.Fatal("the final batch did not acknowledge the complete request")
	}
}

func TestPartialCurationBoundaryDoesNotAcknowledgeUnlistedCandidates(t *testing.T) {
	s := State{Revision: 10, Graph: Graph{Project: Project{Generation: 0}}, Curation: CurationProgress{ThroughRevision: 2},
		Candidates: []Candidate{
			{ID: "old", Claim: "same claim", Scope: "scope", RunID: "first", Revision: 1},
			{ID: "new", Claim: "same claim", Scope: "scope", RunID: "second", Revision: 9},
		},
		Findings: []Finding{{Claim: "same claim", Scope: "scope", CandidateIDs: []string{"old"}, CuratedRevision: 10}},
	}
	// Correcting a finding's support can advance its revision without scanning
	// newly published candidates. Only explicit group coverage acknowledges them.
	if len(s.PendingCurationCandidateIDs()) != 2 {
		t.Fatal("a finding-only update hid an unprocessed candidate")
	}
	s.Findings[0].CandidateIDs = append(s.Findings[0].CandidateIDs, "new")
	if len(s.PendingCurationCandidateIDs()) != 0 {
		t.Fatal("a fully processed partial group remained queued")
	}
}
