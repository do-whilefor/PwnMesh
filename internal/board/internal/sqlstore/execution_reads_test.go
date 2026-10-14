package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"pwnmesh/internal/board/model"
)

func putExecutionSummary(t *testing.T, q Queries, e model.ExecutionSummary) {
	t.Helper()
	if _, err := q.Exec(`INSERT OR IGNORE INTO projects(id,title,status,created_at) VALUES(?,'fixture','active','now')`, e.ProjectID); err != nil {
		t.Fatal(err)
	}
	_, err := q.Exec(`INSERT INTO pwnmesh_executions(`+executionSummaryColumns+`,job,result,metadata_version) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'unavailable','unavailable',1)`,
		e.ProjectID, e.ID, e.Namespace, e.Backend, e.Kind, e.Intent, e.Lease, e.RetryKey, e.Status, e.Resumes, e.CreatedAt, e.UpdatedAt,
		e.Generation, e.InputRevision, e.DecisionRevision, e.StateVersion, e.HasState, e.FactCount, e.HintCount, e.OpenCount)
	if err != nil {
		t.Fatal(err)
	}
}

func TestExecutionReadsKeepCallerSnapshotAndPendingCursor(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "executions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	first := model.ExecutionSummary{ProjectID: "p", ID: "first", Namespace: "ns", Backend: "backend", Kind: "explore", Intent: "step", Lease: "backend@first", RetryKey: "explore:step", Status: "running", Resumes: 2, CreatedAt: "later", UpdatedAt: "updated", Generation: 3, InputRevision: 9, DecisionRevision: 7, StateVersion: "state", HasState: true, FactCount: 4, HintCount: 2, OpenCount: 1}
	second := first
	second.ID, second.CreatedAt = "second", "earlier"
	if err := s.Do(ctx, func(q Queries) error {
		putExecutionSummary(t, q, first)
		foreign := first
		foreign.ID, foreign.Namespace = "foreign", "elsewhere"
		putExecutionSummary(t, q, foreign)
		putExecutionSummary(t, q, second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(q Queries) error {
		page, err := PendingExecutions(q, "ns", 0, 1)
		if err != nil || !reflect.DeepEqual(page.Items, []model.ExecutionSummary{first}) || page.NextCursor == 0 {
			t.Fatalf("first page lost metadata or insertion order: %+v, %v", page, err)
		}
		if err := s.Do(ctx, func(writer Queries) error {
			if _, err := writer.Exec("UPDATE pwnmesh_executions SET status='failed' WHERE id='first'"); err != nil {
				return err
			}
			late := first
			late.ID = "late"
			putExecutionSummary(t, writer, late)
			return nil
		}); err != nil {
			return err
		}
		stored, err := ExecutionSummary(q, "ns", "p", "first")
		if err != nil || stored != first {
			t.Fatalf("summary escaped caller snapshot: %+v, %v", stored, err)
		}
		next, err := PendingExecutions(q, "ns", page.NextCursor, 1)
		if err != nil || !reflect.DeepEqual(next.Items, []model.ExecutionSummary{second}) || next.NextCursor != 0 {
			t.Fatalf("cursor escaped caller snapshot: %+v, %v", next, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(q Queries) error {
		page, err := PendingExecutions(q, "ns", 0, 10)
		if err != nil || len(page.Items) != 2 || page.Items[0].ID != "second" || page.Items[1].ID != "late" {
			t.Fatalf("next transaction missed committed rows: %+v, %v", page, err)
		}
		empty, err := PendingExecutions(q, "absent", 0, 10)
		if err != nil || empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != 0 {
			t.Fatalf("empty page changed shape: %+v, %v", empty, err)
		}
		_, err = ExecutionSummary(q, "elsewhere", "p", "first")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("summary crossed namespace or translated missing row: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionAdmissionReadsPreserveScopesAndGrantOrder(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Do(context.Background(), func(q Queries) error {
		base := model.ExecutionSummary{ProjectID: "p", Namespace: "ns", Kind: "explore", Intent: "step", RetryKey: "key", Status: "retry_requested", CreatedAt: "same", Generation: 2}
		for _, id := range []string{"first-grant", "last-grant"} {
			base.ID = id
			putExecutionSummary(t, q, base)
		}
		for _, scope := range []string{"namespace", "project", "kind", "intent"} {
			other := base
			other.ID, other.CreatedAt = scope, "z"
			switch scope {
			case "namespace":
				other.Namespace = "other"
			case "project":
				other.ProjectID = "other"
			case "kind":
				other.Kind = "other"
			case "intent":
				other.Intent = "other"
			}
			putExecutionSummary(t, q, other)
		}
		candidate := ExecutionCandidate{Kind: "explore", Intent: "step", RetryKey: "key"}
		filter := ExecutionAdmissionFilter{ProjectID: "p", Namespace: "ns", ExecutionCandidate: candidate, Generation: 2}
		individual, err := ReadExecutionAdmission(q, filter)
		want := ExecutionAdmission{PreviousRunID: "last-grant"}
		if err != nil || individual != want {
			t.Fatalf("individual scope/order: %+v, %v", individual, err)
		}
		batch, err := ReadExecutionAdmissions(q, "p", "ns", []ExecutionCandidate{candidate, {Kind: "explore", Intent: "new", RetryKey: "new"}})
		if err != nil || len(batch) != 2 || batch["key"] != individual || batch["new"] != (ExecutionAdmission{}) {
			t.Fatalf("batch scope/order: %+v, %v", batch, err)
		}
		// Storage returns the grant facts even when an older-generation pending
		// attempt exists. Board is responsible for combining them into a block.
		old := base
		old.ID, old.Status, old.Intent, old.Generation, old.StateVersion = "old", "running", "other", 1, "state"
		putExecutionSummary(t, q, old)
		filter.AnyPendingIntent = true
		individual, err = ReadExecutionAdmission(q, filter)
		if err != nil || !individual.Pending || !individual.Tried || individual.PreviousRunID != "last-grant" {
			t.Fatalf("raw admission facts lost: %+v, %v", individual, err)
		}
		filter.GrantMatchesInput, filter.Generation = true, 3
		individual, err = ReadExecutionAdmission(q, filter)
		if err != nil || individual.PreviousRunID != "" {
			t.Fatalf("input-scoped grant crossed generation: %+v, %v", individual, err)
		}
		attempts, err := ExecutionAttempts(q, "ns", "p", "explore", "key", 2)
		if err != nil || attempts != 3 { // Both grants and the other intent share this input key.
			t.Fatalf("attempt scope changed: %d, %v", attempts, err)
		}
		exists, err := ExecutionStateExists(q, "ns", "p", "explore", "state")
		if err != nil || !exists {
			t.Fatalf("state lookup lost historical generation: %v, %v", exists, err)
		}
		_, failure, err := ExecutionSummaryWithFailure(q, "p", "old")
		if err != nil || failure != (ExecutionFailure{}) {
			t.Fatalf("malformed historical blobs broke summary: %+v, %v", failure, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
