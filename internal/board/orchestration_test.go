package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/contract"
	"pwnmesh/internal/protocol"
)

type orchestrationFixture struct {
	t       testing.TB
	store   *Store
	path    string
	planner ExecutionFence
}

func newOrchestrationFixture(t testing.TB) *orchestrationFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orchestration.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f := &orchestrationFixture{t: t, store: store, path: path, planner: ExecutionFence{Run: "planner@plan", Lease: "reason"}}
	store.Now = func() time.Time { return time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { _ = f.store.Close() })
	f.do(func(tx *Tx) error {
		return tx.Save(Graph{Project: Project{ID: "p", Title: "orchestration", Status: "active", CreatedAt: tx.Now, OrchestrationVersion: 1, Reason: &Reason{Worker: f.planner.Run, Trigger: "test", StartedAt: tx.Now, Heartbeat: tx.Now}}, Facts: []Fact{{ID: "origin", Description: "Synthetic fixture"}, {ID: "goal", Description: "Resolve the controlled conflict with independent evidence"}}, Intents: []Intent{}, Hints: []Hint{}})
	})
	return f
}

func (f *orchestrationFixture) do(fn func(*Tx) error) {
	f.t.Helper()
	if err := f.store.Do(context.Background(), fn); err != nil {
		f.t.Fatal(err)
	}
}
func (f *orchestrationFixture) state() State {
	f.t.Helper()
	var s State
	f.do(func(tx *Tx) error { var err error; s, err = tx.State("p"); return err })
	return s
}

func (f *orchestrationFixture) complete(tx *Tx, from []string, description string) error {
	state, err := tx.State("p")
	if err != nil {
		return err
	}
	version := DecisionStateVersion(state)
	ref, err := tx.FreezeInput(state)
	if err != nil {
		return err
	}
	decision, err := BuildDecisionContextFromCursor(state, nil, nil, DefaultContextViewBytes)
	if err != nil {
		return err
	}
	job, err := json.Marshal(map[string]any{
		"kind": "reason", "run_id": "plan", "graph": Graph{Project: state.Graph.Project}, "input_snapshot": ref, "workspace": "/workspace",
		"graph_rpc": true, "result_contract_version": 2,
		"decision": decision, "budget": map[string]int{"max_intents": 1},
	})
	if err != nil {
		return err
	}
	if err := tx.RegisterExecution(Execution{ProjectID: "p", ID: "plan", Namespace: "test", Backend: "planner", Kind: "reason", Lease: f.planner.Run, Job: job, RetryKey: DecisionRetryKey(state.Graph, state.DecisionRevision)}); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"from": from, "description": description})
	if err != nil {
		return err
	}
	_, err = tx.CommitDecision("p", f.planner, DecisionBatch{ExpectedVersion: version, Actions: []DecisionAction{{Op: "complete", Payload: payload}}})
	return err
}

func (f *orchestrationFixture) action(fence ExecutionFence, op, key string, payload any, version string) StateActionResult {
	f.t.Helper()
	raw, _ := json.Marshal(payload)
	var result StateActionResult
	f.do(func(tx *Tx) error {
		var err error
		result, err = tx.StateAction("p", fence, StateAction{Op: op, IdempotencyKey: key, Payload: raw, ExpectedVersion: version})
		return err
	})
	return result
}
func (f *orchestrationFixture) step(label, dispute string) string {
	f.t.Helper()
	return f.action(f.planner, "step", "step:"+label, map[string]any{"action": "add", "description": label, "from": []string{"origin"}, "dispute_id": dispute}, "").ID
}
func (f *orchestrationFixture) worker(label, step string) Execution {
	f.t.Helper()
	return f.workerInput(label, step, true)
}

// Inline input is retained only for explicit persisted-job recovery coverage.
func (f *orchestrationFixture) workerInput(label, step string, snapshot bool) Execution {
	f.t.Helper()
	if step == "" {
		step = f.step(label, "")
	}
	e := Execution{ProjectID: "p", ID: label, Namespace: "test", Backend: "worker", Kind: "explore", Intent: step, Lease: "worker@" + label, RetryKey: "explore:" + step}
	f.do(func(tx *Tx) error {
		intent, err := tx.IntentLease("p", step, LeaseOperation{Op: "heartbeat", Worker: e.Lease})
		if err != nil {
			return err
		}
		s, err := tx.State("p")
		if err != nil {
			return err
		}
		job := map[string]any{"kind": e.Kind, "run_id": e.ID, "graph": s.Graph, "state": s, "intent": intent, "workspace": "/workspace", "graph_rpc": true, "result_contract_version": 2}
		if snapshot {
			ref, err := tx.FreezeInput(s)
			if err != nil {
				return err
			}
			view, err := ContextView(s, step, DefaultContextViewBytes)
			if err != nil {
				return err
			}
			delete(job, "state")
			job["graph"] = Graph{Project: s.Graph.Project}
			job["input_snapshot"] = ref
			job["input_view"] = view
		}
		e.Job, err = json.Marshal(job)
		if err != nil {
			return err
		}
		return tx.RegisterExecution(e)
	})
	return e
}
func (f *orchestrationFixture) fact(e Execution, label string) string {
	f.t.Helper()
	return f.action(e.Fence(), "fact", "fact:"+label, map[string]any{"description": "Observed " + label, "scope": "controlled fixture", "observed_at": f.store.Now().Format(time.RFC3339), "evidence": []EvidenceRef{{RunID: e.Lease, Path: "/workspace/" + e.ID + "/" + label + ".txt", Excerpt: "independently observed " + label}}}, "").ID
}
func (f *orchestrationFixture) candidate(e Execution, fact, status string) string {
	f.t.Helper()
	return f.action(e.Fence(), "candidate", "candidate:"+e.ID, map[string]any{"claim": "The fixture is reachable", "scope": "same controlled condition", "status": status, "sources": []string{fact}, "reason": "Observed result of " + e.ID}, "").ID
}
func (f *orchestrationFixture) finish(e Execution, fact string) {
	f.t.Helper()
	f.do(func(tx *Tx) error {
		if e.Kind == "curate" {
			current, err := tx.Execution(e.ProjectID, e.ID)
			if err == nil && current.Status != "succeeded" {
				return fmt.Errorf("curation commit did not settle its execution: %s", current.Status)
			}
			return err
		}
		if fact != "" {
			if _, err := tx.ConcludeEvidenceStep("p", e.Fence(), fact, nil); err != nil {
				return err
			}
		}
		if fact == "" {
			graph, err := tx.Load(e.ProjectID)
			if err != nil {
				return err
			}
			for _, intent := range graph.Intents {
				if intent.ID == e.Intent && intent.To != nil {
					fact = *intent.To
				}
			}
		}
		if fact == "" {
			return fmt.Errorf("successful fixture execution %s requires accepted evidence", e.ID)
		}
		text, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]string{"fact_id": fact}})
		result, _ := json.Marshal(map[string]string{"status": "success", "text": string(text)})
		if err := tx.ExecutionStatus(e, "result_pending", result); err != nil {
			return err
		}
		if err := tx.ExecutionStatus(e, "succeeded", result); err != nil {
			return err
		}
		return nil
	})
}
func (f *orchestrationFixture) curator(label string) (Execution, State) {
	f.t.Helper()
	e := Execution{ProjectID: "p", ID: label, Namespace: "test", Backend: "curator", Kind: "curate", Lease: "curator@" + label}
	var s State
	f.do(func(tx *Tx) error {
		if _, err := tx.CuratorLease("p", LeaseOperation{Op: "claim", Worker: e.Lease, Trigger: "observations"}); err != nil {
			return err
		}
		var err error
		s, err = tx.State("p")
		if err != nil {
			return err
		}
		ref, err := tx.FreezeInput(s)
		if err != nil {
			return err
		}
		view, err := CurationContextView(s, DefaultContextViewBytes)
		if err != nil {
			return err
		}
		e.Job, err = json.Marshal(map[string]any{"kind": "curate", "run_id": e.ID, "graph": Graph{Project: s.Graph.Project}, "input_snapshot": ref, "input_view": view, "workspace": "/workspace", "graph_rpc": true, "result_contract_version": 2})
		if err != nil {
			return err
		}
		e.RetryKey = CurationRetryKey(s)
		return tx.RegisterExecution(e)
	})
	return e, s
}
func (f *orchestrationFixture) curate(e Execution, s State, groups ...CurateGroup) StateActionResult {
	f.t.Helper()
	if groups == nil {
		groups = []CurateGroup{}
	}
	return f.action(e.Fence(), "curate", e.ID+":curate", CuratePayload{ThroughRevision: s.Revision, Groups: groups}, DecisionStateVersion(s))
}
func requireAPIStatus(t *testing.T, err error, want int) {
	t.Helper()
	var api *APIError
	if !errors.As(err, &api) || api.Status != want {
		t.Fatalf("want HTTP %d, got %v", want, err)
	}
}

func TestOrchestrationFixturesUseRuntimeInputAndResultContracts(t *testing.T) {
	f := newOrchestrationFixture(t)
	worker := f.worker("observe", "")
	fact := f.fact(worker, "response")
	f.finish(worker, fact)
	curator, input := f.curator("curate")
	f.curate(curator, input)
	f.do(func(tx *Tx) error { return f.complete(tx, []string{fact}, "Observed the controlled fixture") })
	f.do(func(tx *Tx) error {
		for _, id := range []string{worker.ID, curator.ID, "plan"} {
			e, err := tx.Execution("p", id)
			if err != nil {
				return err
			}
			var job protocol.Job
			if err := json.Unmarshal(e.Job, &job); err != nil {
				return err
			}
			if err := protocol.ValidateSnapshotInput(job); err != nil {
				t.Fatalf("%s fixture is not a current runtime input: %v", e.Kind, err)
			}
			state, err := tx.ReadInputSnapshot("p", job.InputSnapshot.ID)
			if err != nil {
				return err
			}
			if DecisionStateVersion(state) != job.InputSnapshot.StateVersion {
				t.Fatalf("%s fixture lost its immutable input", e.Kind)
			}
			if e.Kind == "explore" {
				var result protocol.Result
				if err := json.Unmarshal(e.Result, &result); err != nil {
					return err
				}
				parsed, err := contract.ParseWithPolicy(result.Text, e.Kind, result.Conclude, contract.Policy{Version: 2, GraphRPC: true})
				if err != nil || parsed.FactID != fact || parsed.Outcome != "completed" {
					t.Fatalf("fixture result bypassed evidence settlement: %+v, %v", parsed, err)
				}
			}
		}
		return nil
	})
}

func TestOrchestrationConflictOrderAndIndependentReview(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			f := newOrchestrationFixture(t)
			a := f.worker("first", "")
			b := f.worker("second", "")
			fa, fb := f.fact(a, "first"), f.fact(b, "second")
			var ca, cb string
			if reverse {
				cb = f.candidate(b, fb, "refuted")
				ca = f.candidate(a, fa, "verified")
			} else {
				ca = f.candidate(a, fa, "verified")
				cb = f.candidate(b, fb, "refuted")
			}
			if s := f.state(); len(s.Candidates) != 2 || len(s.Findings) != 0 {
				t.Fatal("producer judgment modified the shared view")
			}
			f.finish(a, fa)
			f.finish(b, fb)
			curator, input := f.curator("merge")
			receipt := f.curate(curator, input, CurateGroup{CandidateIDs: []string{ca, cb}, Status: "verified", Reason: "Opposing evidence", Question: "Does the controlled fixture respond to an independent request?"})
			if !receipt.Committed {
				t.Fatal("missing durable curation receipt")
			}
			f.finish(curator, "")
			s := f.state()
			if len(s.Disputes) != 1 || s.Disputes[0].Status != "open" || s.Findings[0].Status != "candidate" || len(s.Findings[0].CandidateIDs) != 2 {
				t.Fatalf("conflict was lost: %+v", s)
			}
			f.do(func(tx *Tx) error {
				requireAPIStatus(t, tx.ValidateStateCompletion("p", []string{fa}), 409)
				return nil
			})
			dispute := s.Disputes[0]
			reviewStep := f.step("independent review", dispute.ID)
			review := f.worker("review", reviewStep)
			rf := f.fact(review, "fresh-review")
			rc := f.candidate(review, rf, "verified")
			f.finish(review, rf)
			curator, input = f.curator("resolve")
			f.curate(curator, input, CurateGroup{CandidateIDs: []string{ca, cb, rc}, Status: "verified", Reason: "Independent execution observed a response", DisputeID: dispute.ID, ReviewFactIDs: []string{rf}, Resolution: "resolved"})
			f.finish(curator, "")
			s = f.state()
			if len(s.Candidates) != 3 || s.Disputes[0].Status != "resolved" || s.Findings[0].Status != "verified" || len(s.Findings[0].Sources) != 1 || s.Findings[0].Sources[0] != rf {
				t.Fatalf("independent resolution missing: %+v", s)
			}
			if s.Graph.Project.Generation != 0 || s.Candidates[2].Generation != 0 {
				t.Fatal("independent review must succeed in the original project generation")
			}
			f.do(func(tx *Tx) error {
				return f.complete(tx, []string{rf}, "Controlled conflict was independently resolved")
			})
			if f.state().Graph.Project.Status != "completed" {
				t.Fatal("reviewed project did not complete")
			}
		})
	}
}

func TestCurationBoundaryReceiptAndCrashRecovery(t *testing.T) {
	f := newOrchestrationFixture(t)
	worker := f.worker("producer", "")
	fact := f.fact(worker, "observation")
	candidate := f.candidate(worker, fact, "verified")
	curator, input := f.curator("curation")
	group := CurateGroup{CandidateIDs: []string{candidate}, Status: "verified", Reason: "Preserve producer and evidence"}
	raw, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{group}})
	request := StateAction{Op: "curate", IdempotencyKey: curator.ID + ":curate", Payload: raw, ExpectedVersion: DecisionStateVersion(input)}
	var first StateActionResult
	f.do(func(tx *Tx) error {
		var err error
		first, err = tx.StateAction("p", curator.Fence(), request)
		return err
	})
	f.do(func(tx *Tx) error {
		again, err := tx.StateAction("p", curator.Fence(), request)
		if err == nil && (again.Revision != first.Revision || !again.Committed) {
			t.Fatal("receipt changed on replay")
		}
		return err
	})
	f.do(func(tx *Tx) error {
		changed := request
		changed.Payload = json.RawMessage(strings.Replace(string(raw), "Preserve producer", "Different judgment", 1))
		_, err := tx.StateAction("p", curator.Fence(), changed)
		requireAPIStatus(t, err, 409)
		return nil
	})
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.store = reopened
	f.do(func(tx *Tx) error {
		receipt, err := tx.CurationReceipt("p", curator.Lease)
		if err == nil && (receipt.Revision != first.Revision || !receipt.Committed) {
			t.Fatal("receipt missing after reopen")
		}
		return err
	})
	f.finish(curator, "")
	f.do(func(tx *Tx) error {
		_, err := tx.CurationReceipt("p", curator.Lease)
		if err != nil {
			return err
		}
		needed, err := tx.CurationNeeded("p")
		if needed {
			t.Fatal("curation self-triggered")
		}
		return err
	})
	// Completing an ordinary producer after a scan is no new reconciliation
	// work. A successful independent dispute review is handled separately.
	f.finish(worker, fact)
	f.do(func(tx *Tx) error {
		needed, err := tx.CurationNeeded("p")
		if needed {
			t.Fatal("ordinary step completion forced another curation turn")
		}
		return err
	})
}

func TestCurationRejectsStaleVersionAndOmittedCandidatesAtomically(t *testing.T) {
	f := newOrchestrationFixture(t)
	worker := f.worker("producer", "")
	fact := f.fact(worker, "first")
	candidate := f.candidate(worker, fact, "verified")
	second := f.worker("second", "")
	f.candidate(second, f.fact(second, "second"), "verified")
	curator, input := f.curator("curation")
	f.do(func(tx *Tx) error {
		payload, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}})
		_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "omitted", Payload: payload, ExpectedVersion: DecisionStateVersion(input)})
		requireAPIStatus(t, err, 409)
		return nil
	})
	if s := f.state(); s.Curation.ThroughRevision != 0 || len(s.Findings) != 0 {
		t.Fatal("failed scan moved the cursor")
	}
	f.fact(worker, "new-input")
	f.do(func(tx *Tx) error {
		payload, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{{CandidateIDs: []string{candidate}, Status: "verified", Reason: "initial evidence"}}})
		_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "stale", Payload: payload, ExpectedVersion: DecisionStateVersion(input)})
		requireAPIStatus(t, err, 409)
		return nil
	})
	if s := f.state(); s.Curation.ThroughRevision != 0 || len(s.Findings) != 0 {
		t.Fatal("stale proposal had an effect")
	}
	// A caller cannot pair the old immutable scan boundary with a freshly
	// observed state hash to authorize its already-outdated semantic proposal.
	current := f.state()
	payload, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{{CandidateIDs: []string{candidate}, Status: "verified", Reason: "initial evidence"}}})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "forged-current-version", Payload: payload, ExpectedVersion: DecisionStateVersion(current)})
		return err
	})
	requireAPIStatus(t, err, 409)
	if !strings.Contains(err.Error(), "registered snapshot") {
		t.Fatalf("wrong registered input rejection: %v", err)
	}
	if s := f.state(); s.Curation.ThroughRevision != 0 || len(s.Findings) != 0 || s.Revision != current.Revision {
		t.Fatal("forged current version advanced the scan or conclusion")
	}
}

func TestOrchestrationRolePermissionsAndModeAreEnforced(t *testing.T) {
	f := newOrchestrationFixture(t)
	worker := f.worker("worker", "")
	f.fact(worker, "fact")
	curator, input := f.curator("curator")
	for _, test := range []struct {
		fence ExecutionFence
		op    string
	}{{worker.Fence(), "finding"}, {worker.Fence(), "fact_relation"}, {worker.Fence(), "step"}, {f.planner, "fact"}, {f.planner, "finding"}, {f.planner, "fact_relation"}, {curator.Fence(), "fact"}, {curator.Fence(), "step"}, {curator.Fence(), "fact_relation"}} {
		f.do(func(tx *Tx) error {
			_, err := tx.StateAction("p", test.fence, StateAction{Op: test.op, IdempotencyKey: test.fence.Run + ":" + test.op, Payload: json.RawMessage(`{}`), ExpectedVersion: DecisionStateVersion(input)})
			requireAPIStatus(t, err, 403)
			return nil
		})
	}
	f.do(func(tx *Tx) error {
		g, err := tx.Load("p")
		if err != nil {
			return err
		}
		g.Project.OrchestrationVersion = 0
		requireAPIStatus(t, tx.Save(g), 409)
		g.Project.OrchestrationVersion = 99
		requireAPIStatus(t, tx.Save(g), 422)
		return nil
	})
	f.do(func(tx *Tx) error {
		g, err := tx.Load("p")
		if err != nil {
			return err
		}
		before := DecisionStateVersion(input)
		if err = tx.HeartbeatCurator("p", curator.Lease); err != nil {
			return err
		}
		current, err := tx.State("p")
		if err != nil {
			return err
		}
		if before != DecisionStateVersion(current) {
			t.Fatal("curator heartbeat changed semantic version")
		}
		if err = tx.SetStatus(&g, "stopped"); err != nil {
			return err
		}
		if err = tx.Save(g); err != nil {
			return err
		}
		_, err = tx.StateAction("p", worker.Fence(), StateAction{Op: "fact", IdempotencyKey: "late", Payload: json.RawMessage(`{}`)})
		requireAPIStatus(t, err, 403)
		return nil
	})
}

func TestCurationLeaseConcurrencyAndGenerationFence(t *testing.T) {
	f := newOrchestrationFixture(t)
	second, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for n, store := range []*Store{f.store, second} {
		wg.Add(1)
		go func(n int, store *Store) {
			defer wg.Done()
			<-start
			results <- store.Do(context.Background(), func(tx *Tx) error { _, err := tx.ClaimCurator("p", fmt.Sprintf("curator@%d", n), "race"); return err })
		}(n, store)
	}
	close(start)
	wg.Wait()
	close(results)
	success, rejected := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else {
			requireAPIStatus(t, err, 409)
			rejected++
		}
	}
	if success != 1 || rejected != 1 {
		t.Fatalf("leases were not exclusive: %d/%d", success, rejected)
	}
	old := f.state().Graph.Project.Curator.Worker
	f.do(func(tx *Tx) error {
		g, err := tx.RestartProject("p", nil)
		if err != nil {
			return err
		}
		if g.Project.OrchestrationVersion != 1 || g.Project.Curator != nil || g.Project.Generation != 1 {
			t.Fatal("restart lost protocol or retained lease")
		}
		_, err = tx.ClaimCurator("p", old, "stale")
		requireAPIStatus(t, err, 409)
		return nil
	})
}

func TestCandidateConcurrentPersistenceAndIdempotency(t *testing.T) {
	f := newOrchestrationFixture(t)
	a, b := f.worker("a", ""), f.worker("b", "")
	fa, fb := f.fact(a, "a"), f.fact(b, "b")
	second, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	start := make(chan struct{})
	results := make(chan error, 2)
	for n, store := range []*Store{f.store, second} {
		go func(n int, store *Store) {
			<-start
			e, fact, status := a, fa, "verified"
			if n == 1 {
				e, fact, status = b, fb, "refuted"
			}
			payload, _ := json.Marshal(map[string]any{"claim": "Same claim", "scope": "same condition", "status": status, "sources": []string{fact}, "reason": "controlled"})
			results <- store.Do(context.Background(), func(tx *Tx) error {
				request := StateAction{Op: "candidate", IdempotencyKey: e.Lease, Payload: payload}
				first, err := tx.StateAction("p", e.Fence(), request)
				if err != nil {
					return err
				}
				again, err := tx.StateAction("p", e.Fence(), request)
				if err == nil && again.ID != first.ID {
					return fmt.Errorf("idempotency changed candidate ID")
				}
				return err
			})
		}(n, store)
	}
	close(start)
	for n := 0; n < 2; n++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	s := f.state()
	if len(s.Candidates) != 2 || len(s.Findings) != 0 {
		t.Fatalf("concurrent candidate loss or unauthorized finding: %+v", s)
	}
}

func (f *orchestrationFixture) conflict() (Dispute, []string, string) {
	f.t.Helper()
	a, b := f.worker("supporter", ""), f.worker("opponent", "")
	fa, fb := f.fact(a, "supporter"), f.fact(b, "opponent")
	ca, cb := f.candidate(a, fa, "verified"), f.candidate(b, fb, "refuted")
	f.finish(a, fa)
	f.finish(b, fb)
	curator, input := f.curator("initial-curation")
	f.curate(curator, input, CurateGroup{CandidateIDs: []string{ca, cb}, Status: "candidate", Reason: "Opposite results", Question: "Verify the fixture independently"})
	f.finish(curator, "")
	return f.state().Disputes[0], []string{ca, cb}, fa
}

func TestReviewResolutionRejectsOriginalOrUnfinishedEvidence(t *testing.T) {
	f := newOrchestrationFixture(t)
	dispute, candidates, originalFact := f.conflict()
	step := f.step("review", dispute.ID)
	review := f.worker("review", step)
	newFact := f.fact(review, "unfinished-review")
	curator, input := f.curator("premature-curation")
	for _, fact := range []string{originalFact, newFact} {
		payload, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{{CandidateIDs: candidates, Status: "verified", Reason: "Attempt premature resolution", DisputeID: dispute.ID, ReviewFactIDs: []string{fact}, Resolution: "resolved"}}})
		err := f.store.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "premature:" + fact, Payload: payload, ExpectedVersion: DecisionStateVersion(input)})
			return err
		})
		requireAPIStatus(t, err, 409)
	}
	if s := f.state(); s.Disputes[0].Status == "resolved" || s.Curation.ThroughRevision >= input.Revision {
		t.Fatal("invalid independent evidence resolved a dispute")
	}
	f.curate(curator, input, CurateGroup{CandidateIDs: candidates, Status: "candidate", Reason: "Review is incomplete", DisputeID: dispute.ID, Resolution: "uncertain"})
	f.finish(curator, "")
	f.do(func(tx *Tx) error {
		requireAPIStatus(t, tx.ValidateStateCompletion("p", []string{originalFact}), 409)
		return nil
	})
	f.finish(review, newFact)
	f.do(func(tx *Tx) error {
		needed, err := tx.CurationNeeded("p")
		if !needed {
			t.Fatal("review success after an uncertain scan did not re-trigger curation")
		}
		return err
	})
}

func TestReviewLimitIncludesRetriesOfTheSameStep(t *testing.T) {
	f := newOrchestrationFixture(t)
	dispute, _, _ := f.conflict()
	step := f.step("independent review", dispute.ID)
	first := f.worker("review-first", step)
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		raw, _ := json.Marshal(map[string]any{"action": "add", "description": "duplicate concurrent review", "from": []string{"origin"}, "dispute_id": dispute.ID})
		_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "duplicate-review", Payload: raw})
		return err
	})
	requireAPIStatus(t, err, 409)
	f.do(func(tx *Tx) error {
		if err := tx.ExecutionStatus(first, "failed", json.RawMessage(`{"status":"failed","error":"no fresh evidence"}`)); err != nil {
			return err
		}
		return tx.ExecutionStatus(first, "retry_requested", nil)
	})
	second := Execution{ProjectID: "p", ID: "review-second", Namespace: "test", Backend: "worker", Kind: "explore", Intent: step, Lease: "worker@review-second", RetryKey: "explore:" + step}
	f.do(func(tx *Tx) error {
		g, err := tx.Load("p")
		if err != nil {
			return err
		}
		for n := range g.Intents {
			if g.Intents[n].ID == step {
				g.Intents[n].Worker = Ptr(second.Lease)
			}
		}
		if err = tx.Save(g); err != nil {
			return err
		}
		s, err := tx.State("p")
		if err != nil {
			return err
		}
		second.Job, _ = json.Marshal(map[string]any{"kind": "explore", "run_id": second.ID, "graph": s.Graph, "state": s, "graph_rpc": true, "result_contract_version": 2, "previous_run_id": first.ID})
		return tx.RegisterExecution(second)
	})
	f.do(func(tx *Tx) error {
		return tx.ExecutionStatus(second, "failed", json.RawMessage(`{"status":"failed","error":"still no fresh evidence"}`))
	})
	err = f.store.Do(context.Background(), func(tx *Tx) error {
		raw, _ := json.Marshal(map[string]any{"action": "add", "description": "third review attempt", "from": []string{"origin"}, "dispute_id": dispute.ID})
		_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "third-review", Payload: raw})
		return err
	})
	requireAPIStatus(t, err, 409)
	if s := f.state(); s.Disputes[0].Status != "uncertain" || len(s.Disputes[0].ReviewStepIDs) != 1 {
		t.Fatalf("retry limit lost uncertainty: %+v", s.Disputes)
	}
}

func TestOrchestrationRejectsLegacyRegistrationAndOversizedCuration(t *testing.T) {
	f := newOrchestrationFixture(t)
	worker := f.worker("worker", "")
	for _, mutate := range []func(map[string]any){func(j map[string]any) { j["graph_rpc"] = false }, func(j map[string]any) { j["result_contract_version"] = 0 }, func(j map[string]any) { j["worker_type"] = "mock" }} {
		var job map[string]any
		_ = json.Unmarshal(worker.Job, &job)
		mutate(job)
		bad := worker
		bad.Job, _ = json.Marshal(job)
		err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.RegisterExecution(bad) })
		requireAPIStatus(t, err, 422)
	}
	f.do(func(tx *Tx) error {
		_, err := tx.ConcludeEvidenceStep("p", worker.Fence(), "", nil)
		requireAPIStatus(t, err, 422)
		_, err = tx.ConcludeEvidenceStep("p", worker.Fence(), "origin", nil)
		requireAPIStatus(t, err, 409)
		return nil
	})
	curator, _ := f.curator("oversized")
	var job map[string]any
	_ = json.Unmarshal(curator.Job, &job)
	job["unused"] = strings.Repeat("x", MaxCurationInputBytes)
	curator.Job, _ = json.Marshal(job)
	err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.RegisterExecution(curator) })
	requireAPIStatus(t, err, 422)
}

func TestCurrentStateJSONPreservesCanonicalVersion(t *testing.T) {
	s := State{Graph: Graph{Project: Project{ID: "fixture", OrchestrationVersion: 1}}}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	// Captured with the existing v1 serializer before removing pre-v1 omission.
	if got := DecisionStateVersion(s); got != "d19724257f8940ef2ca70e269bd006777b9941939eef8a3dc8c3a0ab9bd0debb" {
		t.Fatalf("current decision hash changed: %s", got)
	}
	if !strings.HasSuffix(string(raw), `,"curation":{"generation":0,"through_revision":0}}`) {
		t.Fatalf("current canonical curation field moved: %s", raw)
	}
	s.Graph.Project.OrchestrationVersion = 0
	raw, err = json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"curation"`) {
		t.Fatal("pre-v1 field omission remains active")
	}
}

func TestReopenedDisputeRequiresReviewCoveringNewCandidate(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		t.Run(fmt.Sprintf("snapshot=%t", snapshot), func(t *testing.T) {
			f := newOrchestrationFixture(t)
			dispute, candidates, _ := f.conflict()
			firstStep := f.step("first independent review", dispute.ID)
			first := f.workerInput("first-review", firstStep, snapshot)
			firstFact := f.fact(first, "first-review")
			firstCandidate := f.candidate(first, firstFact, "verified")
			candidates = append(candidates, firstCandidate)
			f.finish(first, firstFact)
			curator, input := f.curator("first-resolution")
			f.curate(curator, input, CurateGroup{CandidateIDs: candidates, Status: "verified", Reason: "First independent result", DisputeID: dispute.ID, ReviewFactIDs: []string{firstFact}, Resolution: "resolved"})
			f.finish(curator, "")

			producer := f.worker("new-producer", "")
			newFact := f.fact(producer, "new-conflicting-evidence")
			newCandidate := f.candidate(producer, newFact, "refuted")
			candidates = append(candidates, newCandidate)
			f.finish(producer, newFact)
			curator, input = f.curator("reopening")
			before, _ := json.Marshal(f.state())
			payload, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{{CandidateIDs: candidates, Status: "verified", Reason: "Incorrectly reuse old review", DisputeID: dispute.ID, ReviewFactIDs: []string{firstFact}, Resolution: "resolved"}}})
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "stale-review-resolution", Payload: payload, ExpectedVersion: DecisionStateVersion(input)})
				return err
			})
			requireAPIStatus(t, err, 409)
			if !strings.Contains(err.Error(), "predates") {
				t.Fatalf("wrong stale review rejection: %v", err)
			}
			after, _ := json.Marshal(f.state())
			if string(before) != string(after) {
				t.Fatal("rejected old review modified the dispute, finding, or scan boundary")
			}
			f.curate(curator, input, CurateGroup{CandidateIDs: candidates, Status: "candidate", Reason: "New conflicting evidence requires another independent review", DisputeID: dispute.ID})
			f.finish(curator, "")
			reopened := f.state().Disputes[0]
			if reopened.Status != "open" || len(reopened.ReviewFactIDs) != 0 || !slices.Contains(reopened.ProducerRunIDs, producer.Lease) {
				t.Fatalf("new producer did not reopen the dispute: %+v", reopened)
			}
			f.do(func(tx *Tx) error {
				requireAPIStatus(t, tx.ValidateStateCompletion("p", []string{firstFact}), 409)
				return nil
			})

			secondStep := f.step("second review including the new candidate", dispute.ID)
			second := f.workerInput("second-review", secondStep, snapshot)
			secondFact := f.fact(second, "second-review")
			secondCandidate := f.candidate(second, secondFact, "refuted")
			candidates = append(candidates, secondCandidate)
			f.finish(second, secondFact)
			curator, input = f.curator("fresh-resolution")
			f.curate(curator, input, CurateGroup{CandidateIDs: candidates, Status: "refuted", Reason: "Fresh review covers the new conflicting candidate", DisputeID: dispute.ID, ReviewFactIDs: []string{secondFact}, Resolution: "resolved"})
			f.finish(curator, "")
			final := f.state()
			if final.Disputes[0].Status != "resolved" || !slices.Equal(final.Disputes[0].ReviewFactIDs, []string{secondFact}) || !slices.Equal(final.Findings[0].Sources, []string{secondFact}) || final.Findings[0].Status != "refuted" {
				t.Fatalf("fresh review failed to resolve reopened dispute: %+v", final.Disputes)
			}
			f.do(func(tx *Tx) error {
				return f.complete(tx, []string{secondFact}, "Reopened conflict independently rechecked")
			})
		})
	}
}

func TestDisputeReviewBudgetRenewsOnlyForNewRetainedEvidence(t *testing.T) {
	for _, freshEvidence := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh-evidence=%t", freshEvidence), func(t *testing.T) {
			f := newOrchestrationFixture(t)
			dispute, candidates, originalFact := f.conflict()
			failedStep := f.step("failed original review", dispute.ID)
			failed := f.worker("failed-original-review", failedStep)
			f.do(func(tx *Tx) error {
				return tx.ExecutionStatus(failed, "failed", json.RawMessage(`{"status":"failed"}`))
			})
			resolvedStep := f.step("successful original review", dispute.ID)
			resolved := f.workerInput("successful-original-review", resolvedStep, true)
			reviewFact := f.fact(resolved, "original-review-result")
			f.finish(resolved, reviewFact)
			curator, input := f.curator("original-resolution")
			f.curate(curator, input, CurateGroup{CandidateIDs: candidates, Status: "verified", Reason: "Independent review", DisputeID: dispute.ID, ReviewFactIDs: []string{reviewFact}, Resolution: "resolved"})
			before := f.state()

			producer := f.worker("later-producer", "")
			source := originalFact
			if freshEvidence {
				source = f.fact(producer, "new-observation")
			}
			candidates = append(candidates, f.candidate(producer, source, "refuted"))
			curator, input = f.curator("later-curation")
			f.curate(curator, input, CurateGroup{CandidateIDs: candidates, Status: "candidate", Reason: "Consider the later producer note", DisputeID: dispute.ID})
			after := f.state()
			if !reflect.DeepEqual(after.Disputes[0].ReviewStepIDs, before.Disputes[0].ReviewStepIDs) {
				t.Fatal("reconsideration erased prior review history")
			}
			if !freshEvidence {
				if after.Disputes[0].Status != "resolved" || !slices.Equal(after.Findings[0].Sources, []string{reviewFact}) {
					t.Fatal("rewording existing support reopened an independently resolved dispute")
				}
				return
			}
			if after.Disputes[0].Status != "open" {
				t.Fatal("new retained support did not reopen the dispute")
			}
			f.do(func(tx *Tx) error {
				requireAPIStatus(t, tx.validateReviewFacts(after, after.Disputes[0], []string{reviewFact}), 409)
				requireAPIStatus(t, tx.validateReviewExecution(failed), 409)
				return tx.reviewExecutionTransition(failed, "failed")
			})
			if !reflect.DeepEqual(after, f.state()) {
				t.Fatal("an old review transition changed the renewed dispute")
			}
			step := f.step("review new evidence", dispute.ID)
			stale := dependencyRun(f, "stale-review", step)
			var staleInput *InputSnapshot
			f.do(func(tx *Tx) (err error) { staleInput, err = tx.FreezeInput(before); return err })
			bound := f.state()
			for _, frozen := range []bool{false, true} {
				job := map[string]any{"kind": "explore", "run_id": stale.ID, "graph": bound.Graph, "graph_rpc": true, "result_contract_version": 2}
				if frozen {
					job["input_snapshot"] = staleInput
				} else {
					job["state"] = before
				}
				stale.Job, _ = json.Marshal(job)
				err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.RegisterExecution(stale) })
				requireAPIStatus(t, err, 409)
				if !strings.Contains(err.Error(), "predates") || !reflect.DeepEqual(bound, f.state()) {
					t.Fatalf("stale review input escaped its renewed evidence boundary: %v", err)
				}
			}
			f.do(func(tx *Tx) error {
				_, err := tx.IntentLease("p", step, LeaseOperation{Op: "release", Worker: stale.Lease})
				return err
			})
			first := f.workerInput("renewed-review-first", step, true)
			f.do(func(tx *Tx) error { return tx.ExecutionStatus(first, "failed", json.RawMessage(`{"status":"failed"}`)) })
			grantMainRetry(f, "renewed-review-retry", first)
			second := retrySuccessor(f, first, "renewed-review-second")
			f.do(func(tx *Tx) error {
				return tx.ExecutionStatus(second, "failed", json.RawMessage(`{"status":"failed"}`))
			})
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "renewed-review-third", Payload: retryStepAction(step, second.ID)})
				return err
			})
			requireAPIStatus(t, err, 409)
			if !strings.Contains(err.Error(), "review execution limit") || f.state().Disputes[0].Status != "uncertain" {
				t.Fatalf("new evidence removed the bounded review limit: %v", err)
			}
		})
	}
}

func TestDisputeReviewRenewalRequiresNewEffectiveEvidenceReferences(t *testing.T) {
	old := EvidenceRef{RunID: "old", Path: "evidence/response.txt", Excerpt: "observed"}
	fresh := EvidenceRef{RunID: "fresh", Path: "evidence/response.txt", Excerpt: "changed"}
	s := State{Graph: Graph{Project: Project{OrchestrationVersion: 1}},
		FactRecords: []FactRecord{
			{ID: "old", Status: "valid", Evidence: []EvidenceRef{old}},
			{ID: "alias", Status: "valid", Evidence: []EvidenceRef{old}},
			{ID: "fresh", Status: "valid", Evidence: []EvidenceRef{fresh}},
			{ID: "invalid", Status: "refuted", Evidence: []EvidenceRef{fresh}},
		}, Candidates: []Candidate{{ID: "prior", Sources: []string{"old"}}}}
	dispute := Dispute{CandidateIDs: []string{"prior"}}
	for _, tc := range []struct {
		name      string
		candidate Candidate
		want      bool
	}{
		{"same-source", Candidate{Sources: []string{"old"}, Reason: "Reworded interpretation"}, false},
		{"new-fact-same-evidence", Candidate{Sources: []string{"alias"}}, false},
		{"invalid-new-evidence", Candidate{Sources: []string{"invalid"}}, false},
		{"fresh-source", Candidate{Sources: []string{"fresh"}}, true},
		{"same-direct-evidence", Candidate{Evidence: []EvidenceRef{old}}, false},
		{"fresh-direct-evidence", Candidate{Evidence: []EvidenceRef{fresh}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := disputeHasNewEvidence(s, dispute, []Candidate{tc.candidate}, nil, nil); got != tc.want {
				t.Fatalf("new evidence=%t, want %t", got, tc.want)
			}
		})
	}
	s.FactRecords = append(s.FactRecords, FactRecord{ID: "prior-review", SourceStepID: "review", Status: "refuted", Evidence: []EvidenceRef{old}})
	dispute.ReviewStepIDs = []string{"review"}
	prior := []FactRelation{{Kind: "refutes", Source: "fresh", Target: "prior-review"}}
	if disputeHasNewEvidence(s, dispute, nil, []string{"fresh"}, prior) {
		t.Fatal("reusing an earlier correction's retained evidence renewed the budget again")
	}
}

func TestDisputeReopeningWithoutNewEvidenceAdvancesReviewBoundary(t *testing.T) {
	f := newOrchestrationFixture(t)
	dispute, candidates, original := f.conflict()
	step := f.step("original independent review", dispute.ID)
	review := f.worker("original-review", step)
	accepted, alternate := f.fact(review, "accepted"), f.fact(review, "alternate")
	f.finish(review, accepted)
	curator, input := f.curator("resolve-original")
	f.curate(curator, input, CurateGroup{CandidateIDs: candidates, Status: "verified", Reason: "Independent result", DisputeID: dispute.ID, ReviewFactIDs: []string{accepted}, Resolution: "resolved"})
	before := f.state()
	curator, input = f.curator("correct-with-known-evidence")
	f.action(curator.Fence(), "curate", "known-correction", CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{{Kind: "refutes", Source: original, Target: accepted, Reason: "The original retained observation contradicts the review"}}}, DecisionStateVersion(input))
	reopened := f.state()
	if reopened.Disputes[0].Status != "open" || reopened.Disputes[0].ReviewStepOffset != 0 || reopened.Disputes[0].ReviewRevision != input.Revision+1 {
		t.Fatalf("known evidence reset the allowance or failed to fence prior inputs: %+v", reopened.Disputes[0])
	}
	if err := reopened.ValidateFactSources([]string{alternate}, true); err != nil {
		t.Fatalf("alternate raw observation should remain valid: %v", err)
	}
	f.do(func(tx *Tx) error {
		err := tx.validateReviewFacts(reopened, reopened.Disputes[0], []string{alternate})
		requireAPIStatus(t, err, 409)
		if !strings.Contains(err.Error(), "predates") {
			t.Fatalf("old alternate evidence escaped the new input boundary: %v", err)
		}
		return nil
	})
	freshStep := f.step("remaining independent attempt", dispute.ID)
	stale := dependencyRun(f, "stale-remaining-review", freshStep)
	bound := f.state()
	stale.Job, _ = json.Marshal(map[string]any{"kind": "explore", "run_id": stale.ID, "graph": bound.Graph, "state": before, "graph_rpc": true, "result_contract_version": 2})
	err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.RegisterExecution(stale) })
	requireAPIStatus(t, err, 409)
	if !strings.Contains(err.Error(), "predates") || !reflect.DeepEqual(bound, f.state()) {
		t.Fatalf("stale input changed the remaining allowance: %v", err)
	}
	f.do(func(tx *Tx) error {
		_, err := tx.IntentLease("p", freshStep, LeaseOperation{Op: "release", Worker: stale.Lease})
		return err
	})
	fresh := f.worker("fresh-remaining-review", freshStep)
	f.do(func(tx *Tx) error { return tx.ExecutionStatus(fresh, "failed", json.RawMessage(`{"status":"failed"}`)) })
	err = f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "third-known-evidence-review", Payload: retryStepAction(freshStep, fresh.ID)})
		return err
	})
	requireAPIStatus(t, err, 409)
	if !strings.Contains(err.Error(), "review execution limit") {
		t.Fatalf("known evidence silently renewed the original allowance: %v", err)
	}
}

func TestDisputeReopeningRevokesPreparedReviewsAndPreservesNewAllowance(t *testing.T) {
	f := newOrchestrationFixture(t)
	dispute, candidates, _ := f.conflict()
	firstStep := f.step("completed independent review", dispute.ID)
	first := f.worker("completed-review", firstStep)
	firstFact := f.fact(first, "accepted-review")
	f.finish(first, firstFact)
	oldStep := f.step("prepared second review", dispute.ID)
	old := f.worker("prepared-review", oldStep)
	oldFact := f.fact(old, "prepared-observation")
	curator, input := f.curator("resolve-with-pending-review")
	f.curate(curator, input, CurateGroup{CandidateIDs: candidates, Status: "verified", Reason: "Completed independent result", DisputeID: dispute.ID, ReviewFactIDs: []string{firstFact}, Resolution: "resolved"})
	producer := f.worker("fresh-producer", "")
	freshFact := f.fact(producer, "fresh-conflict")
	candidates = append(candidates, f.candidate(producer, freshFact, "refuted"))
	f.finish(producer, freshFact)
	curator, input = f.curator("reopen-with-pending-review")
	payload, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{{CandidateIDs: candidates, Status: "verified", Reason: "Invalid old review reuse", DisputeID: dispute.ID, ReviewFactIDs: []string{firstFact}, Resolution: "resolved"}}})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", curator.Fence(), StateAction{Op: "curate", IdempotencyKey: "rejected-reopen", Payload: payload, ExpectedVersion: DecisionStateVersion(input)})
		return err
	})
	requireAPIStatus(t, err, 409)
	f.do(func(tx *Tx) error {
		revoked, err := tx.RunRevoked("p", old.Lease)
		if revoked {
			t.Fatal("rejected curation leaked a run revocation")
		}
		return err
	})
	f.curate(curator, input, CurateGroup{CandidateIDs: candidates, Status: "candidate", Reason: "New retained support requires review", DisputeID: dispute.ID})
	f.do(func(tx *Tx) error {
		revoked, err := tx.RunRevoked("p", old.Lease)
		if !revoked {
			t.Fatal("reopened evidence did not revoke its stale pending review")
		}
		return err
	})
	lateFact, _ := json.Marshal(map[string]any{"description": "Late stale review", "scope": "fixture", "observed_at": f.store.Now().Format(time.RFC3339), "evidence": []EvidenceRef{{RunID: old.Lease, Path: "/workspace/late.txt", Excerpt: "late"}}})
	for _, check := range []func(*Tx) error{
		func(tx *Tx) error { return tx.ExecutionStatus(old, "running", nil) },
		func(tx *Tx) error { return tx.ResumeExecution(old) },
		func(tx *Tx) error { _, err := tx.ConcludeEvidenceStep("p", old.Fence(), oldFact, nil); return err },
		func(tx *Tx) error {
			_, err := tx.StateAction("p", old.Fence(), StateAction{Op: "fact", IdempotencyKey: "late-stale-fact", Payload: lateFact})
			return err
		},
	} {
		requireAPIStatus(t, f.store.Do(context.Background(), check), 409)
	}
	result := json.RawMessage(`{"status":"success","text":"late result"}`)
	f.do(func(tx *Tx) error { return tx.ExecutionStatus(old, "result_pending", result) })
	requireAPIStatus(t, f.store.Do(context.Background(), func(tx *Tx) error { return tx.ExecutionStatus(old, "succeeded", result) }), 409)
	freshStep := f.step("review renewed evidence", dispute.ID)
	fresh := f.worker("fresh-review", freshStep)
	f.do(func(tx *Tx) error { return tx.ExecutionStatus(fresh, "failed", json.RawMessage(`{"status":"failed"}`)) })
	grantMainRetry(f, "fresh-review-retry", fresh)
	retrySuccessor(f, fresh, "fresh-review-retry")
	beforeCleanup := f.state().Disputes[0]
	f.do(func(tx *Tx) error { return tx.ExecutionStatus(old, "cancelled", nil) })
	if !reflect.DeepEqual(beforeCleanup, f.state().Disputes[0]) {
		t.Fatal("stale run cleanup changed the current evidence review")
	}
}
