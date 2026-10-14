package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestExecutionWritesKeepInputsAndCallerTransaction(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	metadata := model.ExecutionSummary{Generation: 3, InputRevision: 9, DecisionRevision: 7, StateVersion: "version", HasState: true, FactCount: 5, HintCount: 2, OpenCount: 1}
	old := model.Execution{ProjectID: "p", ID: "old", Namespace: "ns", Backend: "worker", Kind: "explore", Intent: "step", Lease: "worker@old", RetryKey: "key", Status: "retry_requested", Job: []byte("{ \"retained\": true }\n"), Result: []byte(`{"status":"failed"}`), Resumes: 2, CreatedAt: "created", UpdatedAt: "updated"}
	foreign := old
	foreign.ProjectID = "other"
	if err := s.Do(ctx, func(q Queries) error {
		for _, e := range []model.Execution{old, foreign} {
			if _, err := q.Exec("INSERT INTO projects(id,title,status,created_at) VALUES(?,'fixture','active','now')", e.ProjectID); err != nil {
				return err
			}
			if err := InsertExecution(q, e, metadata, ""); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	next := old
	next.ID, next.Lease, next.Status, next.Result, next.Resumes = "next", "worker@next", "prepared", nil, 0
	next.CreatedAt, next.UpdatedAt = "registered", "registered"
	write := func(q Queries) error {
		if err := InsertExecution(q, next, metadata, old.ID); err != nil {
			return err
		}
		stored, err := Execution(q, "p", "next")
		if err != nil || !reflect.DeepEqual(stored, next) {
			t.Fatalf("registration lost bytes or null result: %+v, %v", stored, err)
		}
		outcome := next
		outcome.Status, outcome.Result, outcome.UpdatedAt = "failed", []byte("{ \"status\": \"failed\" }\n"), "settled"
		outcome.Job, outcome.Namespace, outcome.RetryKey = []byte("ignored"), "ignored", "ignored"
		if err := SaveExecutionOutcome(q, outcome, true); err != nil {
			return err
		}
		if err := AddExecutionResume(q, "p", "next", "delivery", 0); err != nil {
			return err
		}
		return AddExecutionResume(q, "p", "next", "resumed", 1)
	}
	rejected := errors.New("enclosing operation rejected")
	if err := s.Do(ctx, func(q Queries) error {
		if err := write(q); err != nil {
			return err
		}
		return rejected
	}); !errors.Is(err, rejected) {
		t.Fatalf("outer failure changed: %v", err)
	}
	if err := s.View(ctx, func(q Queries) error {
		stored, err := Execution(q, "p", "old")
		if err != nil || !reflect.DeepEqual(stored, old) {
			t.Fatalf("failed caller consumed grant: %+v, %v", stored, err)
		}
		_, err = Execution(q, "p", "next")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("failed caller retained successor: %v", err)
		}
		var revoked bool
		if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM pwnmesh_revoked_runs WHERE project_id='p' AND worker='worker@next')").Scan(&revoked); err != nil || revoked {
			t.Fatalf("failed caller retained revocation: %v, %v", revoked, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Do(ctx, write); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(q Queries) error {
		stored, err := ExecutionByLease(q, "p", "worker@next", "explore")
		want := next
		want.Status, want.Result, want.Resumes, want.UpdatedAt = "failed", []byte("{ \"status\": \"failed\" }\n"), 1, "resumed"
		if err != nil || !reflect.DeepEqual(stored, want) {
			t.Fatalf("outcome rewrote input or lost result/resume: %+v, %v", stored, err)
		}
		stored, err = Execution(q, "p", "old")
		want = old
		want.Status, want.UpdatedAt = "retried", next.CreatedAt
		if err != nil || !reflect.DeepEqual(stored, want) {
			t.Fatalf("grant changed saved input/result: %+v, %v", stored, err)
		}
		var revoked bool
		if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM pwnmesh_revoked_runs WHERE project_id='p' AND worker='worker@next')").Scan(&revoked); err != nil || !revoked {
			t.Fatalf("committed outcome lost revocation: %v, %v", revoked, err)
		}
		stored, err = Execution(q, "other", "old")
		if err != nil || !reflect.DeepEqual(stored, foreign) {
			t.Fatalf("grant consumption crossed project: %+v, %v", stored, err)
		}
		summary, err := ExecutionSummary(q, "ns", "p", "next")
		if err != nil || summary.Generation != metadata.Generation || summary.InputRevision != metadata.InputRevision || summary.DecisionRevision != metadata.DecisionRevision || summary.StateVersion != metadata.StateVersion || summary.HasState != metadata.HasState || summary.FactCount != metadata.FactCount || summary.HintCount != metadata.HintCount || summary.OpenCount != metadata.OpenCount {
			t.Fatalf("row writes lost metadata: %+v, %v", summary, err)
		}
		seen := []string{}
		if err := VisitExecutions(q, "p", func(e model.Execution) error {
			seen = append(seen, e.ID)
			return nil
		}); err != nil || !reflect.DeepEqual(seen, []string{"old", "next"}) {
			t.Fatalf("stream changed scope or registration order: %v, %v", seen, err)
		}
		calls := 0
		if err := VisitExecutions(q, "p", func(model.Execution) error {
			calls++
			return rejected
		}); !errors.Is(err, rejected) || calls != 1 {
			t.Fatalf("stream ignored callback error: %d, %v", calls, err)
		}
		_, err = ExecutionByLease(q, "p", "worker@next", "reason")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("lease read crossed role: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
