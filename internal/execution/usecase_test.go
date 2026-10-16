package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	b "pwnmesh/internal/board"
	"pwnmesh/internal/board/model"
	"pwnmesh/internal/protocol"
)

func TestExecutionRejectsOverflowingBudgetsBeforeMutation(t *testing.T) {
	const overflow = int((1<<63-1)/time.Second) + 1
	for _, operation := range []string{"prepare", "register"} {
		for _, field := range []string{"timeout", "conclude_timeout"} {
			t.Run(operation+"/"+field, func(t *testing.T) {
				store, template := useCaseFixture(t)
				var job protocol.Job
				if err := json.Unmarshal(template.Job, &job); err != nil {
					t.Fatal(err)
				}
				if field == "timeout" {
					job.Budget.Timeout = overflow
				} else {
					job.Budget.ConcludeTimeout = overflow
				}
				if operation == "register" {
					job.State = &model.State{Graph: job.Graph}
					job.Decision = &model.DecisionContext{Version: 2, StateVersion: model.DecisionStateVersion(*job.State)}
				}
				template.Job, _ = json.Marshal(job)
				template.RetryKey = "reason:test"
				ctx := context.Background()
				var snapshotID string
				if err := store.Do(ctx, func(tx *b.Tx) error {
					state, err := tx.State(template.ProjectID)
					if err != nil {
						return err
					}
					raw, err := json.Marshal(state)
					if err != nil {
						return err
					}
					sum := sha256.Sum256(raw)
					snapshotID = hex.EncodeToString(sum[:])
					if operation == "prepare" {
						_, err = PrepareRun(tx, template, template.Fence())
					} else {
						_, err = Register(tx, template, template.Fence())
					}
					if !hasStatus(err, 422) || !strings.Contains(err.Error(), "timeout") {
						t.Errorf("overflowing budget was not rejected: %v", err)
					}
					return nil // A handled validation error must leave no registered run.
				}); err != nil {
					t.Fatal(err)
				}
				if err := store.View(ctx, func(tx *b.Tx) error {
					if _, err := tx.Execution(template.ProjectID, template.ID); !hasStatus(err, 404) {
						t.Errorf("invalid budget retained an execution: %v", err)
					}
					if _, err := tx.InputSnapshotMetadata(template.ProjectID, snapshotID); !hasStatus(err, 404) {
						t.Errorf("invalid budget froze an input snapshot: %v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func useCaseFixture(t *testing.T) (*b.Store, b.Execution) {
	t.Helper()
	store, err := b.Open(filepath.Join(t.TempDir(), "execution.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.Now = func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) }
	var template b.Execution
	if err = store.Do(context.Background(), func(tx *b.Tx) error {
		graph, err := tx.CreateProject(b.ProjectInput{Title: "Execution use case", Origin: "Synthetic fixture", Goal: "Check the fixture"})
		if err != nil {
			return err
		}
		project, err := tx.ReasonLease(graph.Project.ID, b.LeaseOperation{Op: "claim", Worker: "agent@plan", Trigger: "initial"})
		if err != nil {
			return err
		}
		job, err := json.Marshal(protocol.Job{RunID: "plan", Kind: "reason", Graph: b.Graph{Project: project}, Workspace: "/workspace", GraphRPC: true, ResultContractVersion: 2, DecisionTrigger: "initial", Budget: model.TaskBudget{MaxIntents: 3}})
		template = b.Execution{ProjectID: project.ID, ID: "plan", Namespace: "test", Backend: "agent", Kind: "reason", Lease: "agent@plan", Job: job}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return store, template
}

func TestPrepareUsesCallerTransactionAndReplaysFrozenIdentity(t *testing.T) {
	store, template := useCaseFixture(t)
	ctx := context.Background()
	abort := errors.New("caller abort")
	var prepared Prepared
	err := store.Do(ctx, func(tx *b.Tx) error {
		var err error
		prepared, err = PrepareRun(tx, template, template.Fence())
		if err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) || !prepared.Created {
		t.Fatalf("prepare: created=%v, error=%v", prepared.Created, err)
	}
	var job protocol.Job
	if err = json.Unmarshal(prepared.Execution.Job, &job); err != nil || job.InputSnapshot == nil {
		t.Fatalf("missing bound input: %v", err)
	}
	if err = store.View(ctx, func(tx *b.Tx) error {
		if _, err := tx.Execution(template.ProjectID, template.ID); err == nil {
			t.Fatal("use case committed an execution outside its caller's transaction")
		}
		if _, err := tx.ReadInputSnapshot(template.ProjectID, job.InputSnapshot.ID); err == nil {
			t.Fatal("use case committed an input snapshot outside its caller's transaction")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = store.Do(ctx, func(tx *b.Tx) error {
		var err error
		prepared, err = PrepareRun(tx, template, template.Fence())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err = store.Do(ctx, func(tx *b.Tx) error {
		if _, err := tx.AddProjectHint(template.ProjectID, b.HintInput{Content: "Later input", Creator: "test"}, ""); err != nil {
			return err
		}
		replayed, err := PrepareRun(tx, template, template.Fence())
		if err == nil && (replayed.Created || !bytes.Equal(prepared.Execution.Job, replayed.Execution.Job)) {
			t.Fatal("replay rebuilt the immutable assignment from new input")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err = store.Do(ctx, func(tx *b.Tx) error {
		_, err := PrepareRun(tx, template, b.ExecutionFence{Run: "other", Lease: "reason"})
		return err
	}); !hasStatus(err, 403) {
		t.Fatalf("replay accepted an unrelated identity: %v", err)
	}
}

func preparedExecute(t *testing.T) (*b.Store, b.Execution) {
	t.Helper()
	store, planner := useCaseFixture(t)
	var execute b.Execution
	if err := store.Do(context.Background(), func(tx *b.Tx) error {
		prepared, err := PrepareRun(tx, planner, planner.Fence())
		if err != nil {
			return err
		}
		var job protocol.Job
		if err = json.Unmarshal(prepared.Execution.Job, &job); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"action": "add", "from": []string{"origin"}, "description": "Observe the fixture"})
		receipt, err := tx.CommitDecision(planner.ProjectID, planner.Fence(), b.DecisionBatch{ExpectedVersion: job.Decision.StateVersion, Actions: []b.DecisionAction{{Op: "step", Ref: "observe", Payload: payload}}})
		if err != nil {
			return err
		}
		intent, err := tx.IntentLease(planner.ProjectID, receipt.IDs["observe"], b.LeaseOperation{Op: "heartbeat", Worker: "agent@execute"})
		if err != nil {
			return err
		}
		graph, err := tx.Load(planner.ProjectID)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(protocol.Job{RunID: "execute", Kind: "explore", Graph: b.Graph{Project: graph.Project}, Intent: &intent, Workspace: "/workspace", GraphRPC: true, ResultContractVersion: 2})
		if err != nil {
			return err
		}
		template := b.Execution{ProjectID: planner.ProjectID, ID: "execute", Namespace: "test", Backend: "agent", Kind: "explore", Intent: intent.ID, Lease: "agent@execute", Job: raw}
		prepared, err = PrepareRun(tx, template, template.Fence())
		execute = prepared.Execution
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return store, execute
}

func TestSettlementRemainsAtomicIdempotentAndLeaseBoundWithoutHTTP(t *testing.T) {
	store, run := preparedExecute(t)
	ctx := context.Background()
	fact := map[string]any{"description": "Fixture returned 401", "scope": "one fixture request", "observed_at": "2026-09-22T10:00:00Z", "evidence": []b.EvidenceRef{{RunID: run.ID, Path: "/run/evidence/frozen.raw", Excerpt: "HTTP/1.1 401 Unauthorized"}}}
	text, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": fact}})
	result, _ := json.Marshal(protocol.Result{Status: "success", Text: string(text)})
	if err := store.Do(ctx, func(tx *b.Tx) error {
		_, err := ChangeStatus(tx, run.ProjectID, run.ID, run.Fence(), "result_pending", result)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Do(ctx, func(tx *b.Tx) error {
		_, err := Apply(tx, run.ProjectID, run.ID, b.ExecutionFence{Run: "other", Lease: run.Kind, Intent: run.Intent})
		return err
	}); !hasStatus(err, 403) {
		t.Fatalf("settlement accepted another execution: %v", err)
	}
	abort := errors.New("caller abort")
	if err := store.Do(ctx, func(tx *b.Tx) error {
		if status, err := Apply(tx, run.ProjectID, run.ID, run.Fence()); err != nil || status != "succeeded" {
			t.Fatalf("apply: status=%s error=%v", status, err)
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if err := store.View(ctx, func(tx *b.Tx) error {
		state, err := tx.State(run.ProjectID)
		if err != nil {
			return err
		}
		saved, err := tx.Execution(run.ProjectID, run.ID)
		if err == nil && (saved.Status != "result_pending" || len(state.Graph.Facts) != 2 || state.Steps[0].Result != nil) {
			t.Fatal("rolled-back settlement retained its fact, Step result or execution status")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.Do(ctx, func(tx *b.Tx) error {
			_, err := Apply(tx, run.ProjectID, run.ID, run.Fence())
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.View(ctx, func(tx *b.Tx) error {
		state, err := tx.State(run.ProjectID)
		if err == nil && (len(state.Graph.Facts) != 3 || state.Steps[0].Status != "completed") {
			t.Fatal("settlement replay duplicated evidence or lost completion")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRetryKeepsManagementAuthorityOutsideExecutionLease(t *testing.T) {
	store, run := preparedExecute(t)
	ctx := context.Background()
	if err := store.Do(ctx, func(tx *b.Tx) error {
		_, err := ChangeStatus(tx, run.ProjectID, run.ID, run.Fence(), "failed", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, automatic := range []bool{false, true} {
		if err := store.Do(ctx, func(tx *b.Tx) error {
			return Retry(tx, run.ProjectID, run.ID, run.Fence(), automatic)
		}); !hasStatus(err, 403) {
			t.Fatalf("execution granted its own retry: %v", err)
		}
	}
	if err := store.Do(ctx, func(tx *b.Tx) error { return Retry(tx, run.ProjectID, run.ID, b.ExecutionFence{}, false) }); err != nil {
		t.Fatal(err)
	}
	if err := store.Do(ctx, func(tx *b.Tx) error { return Retry(tx, run.ProjectID, run.ID, b.ExecutionFence{}, false) }); !hasStatus(err, 409) {
		t.Fatalf("retry grant was duplicated: %v", err)
	}
}

func hasStatus(err error, status int) bool {
	var api *b.APIError
	return errors.As(err, &api) && api.Status == status
}
