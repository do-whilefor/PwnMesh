package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestExecutionDetailReadsKeepScopeAndOpaqueBytes(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Do(context.Background(), func(q Queries) error {
		base := model.ExecutionSummary{ProjectID: "p", ID: "run", Namespace: "ns", Kind: "reason", Lease: "worker@run", Status: "succeeded"}
		putExecutionSummary(t, q, base)
		for _, scope := range []string{"project", "lease", "kind"} {
			other := base
			other.ID = scope
			switch scope {
			case "project":
				other.ProjectID = "other"
			case "lease":
				other.Lease = "other"
			case "kind":
				other.Kind = "curate"
			}
			putExecutionSummary(t, q, other)
		}
		// Invalid historical JSON must remain opaque in storage. Neither an
		// identity lookup nor a result lookup needs to decode the saved job.
		job, result := []byte{' ', '{', '\n', 0xff}, []byte{'\n', 0, 0xfe}
		if _, err := q.Exec("UPDATE pwnmesh_executions SET job=?,result=? WHERE project_id='p' AND id='run'", job, result); err != nil {
			return err
		}
		id, raw, err := ExecutionInputByLease(q, "p", "worker@run", "reason")
		if err != nil || id != "run" || !bytes.Equal(raw, job) {
			t.Fatalf("input scope or bytes changed: %q, %x, %v", id, raw, err)
		}
		for _, scope := range [][3]string{{"missing", "worker@run", "reason"}, {"p", "missing", "reason"}, {"p", "worker@run", "explore"}} {
			_, _, err := ExecutionInputByLease(q, scope[0], scope[1], scope[2])
			if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("input crossed scope %v: %v", scope, err)
			}
		}
		row, err := ExecutionResult(q, "p", "run")
		if err != nil || row.Kind != "reason" || row.Intent != "" || row.Lease != base.Lease || row.Status != "succeeded" || !bytes.Equal(row.Result, result) {
			t.Fatalf("result identity or bytes changed: %+v, %v", row, err)
		}
		if _, err := ExecutionResult(q, "other", "run"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("result crossed project: %v", err)
		}
		producer := base
		producer.ID, producer.Kind, producer.Intent, producer.Lease = "producer", "explore", "step", "worker@producer"
		putExecutionSummary(t, q, producer)
		id, status, intent, err := ExecutionIdentityByLease(q, "p", producer.Lease)
		if err != nil || id != producer.ID || status != producer.Status || intent != producer.Intent {
			t.Fatalf("identity read required valid job/result: %q, %q, %q, %v", id, status, intent, err)
		}
		if _, _, _, err := ExecutionIdentityByLease(q, "other", producer.Lease); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("identity crossed project: %v", err)
		}
		status, err = CuratorExecutionStatus(q, "p", base.Lease)
		if err != nil || status != "succeeded" {
			t.Fatalf("curator status read required valid blobs: %q, %v", status, err)
		}
		for _, row := range []model.ExecutionSummary{
			{ProjectID: "p", ID: "wrong-intent", Kind: "curate", Intent: "step", Lease: "wrong-intent", Status: "succeeded"},
			{ProjectID: "p", ID: "wrong-kind", Kind: "reason", Lease: "wrong-kind", Status: "running"},
		} {
			putExecutionSummary(t, q, row)
			if _, err := CuratorExecutionStatus(q, "p", row.Lease); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("curator status crossed kind or intent: %+v, %v", row, err)
			}
		}
		for _, scope := range [][2]string{{"missing", base.Lease}, {"p", "missing"}} {
			if _, err := CuratorExecutionStatus(q, scope[0], scope[1]); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("curator status crossed scope %v: %v", scope, err)
			}
		}
		for _, status := range []string{"prepared", "running", "retryable", "result_pending", "succeeded", "failed"} {
			run := producer
			run.ID, run.Lease, run.Status = status, status, status
			putExecutionSummary(t, q, run)
			running, err := RunningStepExecution(q, "p", "step", status)
			if err != nil || running != (status == "running" || status == "result_pending") {
				t.Fatalf("running status changed for %q: %v, %v", status, running, err)
			}
		}
		for _, scope := range [][3]string{{"other", "step", "running"}, {"p", "other", "running"}, {"p", "step", "missing"}, {"p", "", "wrong-kind"}} {
			running, err := RunningStepExecution(q, scope[0], scope[1], scope[2])
			if err != nil || running {
				t.Fatalf("running lookup crossed scope %v: %v, %v", scope, running, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionObservationWritesRetainReadViewsAndCallerTransaction(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	view, result := []byte{' ', 0xff, '\n'}, []byte{'\n', 0, 0xfe}
	before := map[string]model.Execution{}
	if err := s.Do(ctx, func(q Queries) error {
		for _, scope := range [][2]string{{"p", "run"}, {"p", "other"}, {"other", "run"}} {
			putExecutionSummary(t, q, model.ExecutionSummary{ProjectID: scope[0], ID: scope[1], Kind: "reason", Lease: "worker@" + scope[1], Status: "succeeded", UpdatedAt: "settled"})
			if err := SaveDecisionReadView(q, scope[0], scope[1], "version", view); err != nil {
				return err
			}
			e, err := Execution(q, scope[0], scope[1])
			if err != nil {
				return err
			}
			before[scope[0]+":"+scope[1]] = e
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	write := func(q Queries) error {
		if err := SaveDecisionReadView(q, "p", "run", "refreshed", view); err != nil {
			return err
		}
		return SaveExecutionObservation(q, "p", "run", result)
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
	verify := func(committed bool) {
		t.Helper()
		if err := s.View(ctx, func(q Queries) error {
			for _, want := range before {
				version := "version"
				if committed && want.ProjectID == "p" && want.ID == "run" {
					want.Result, version = result, "refreshed"
				}
				stored, err := Execution(q, want.ProjectID, want.ID)
				if err != nil || !reflect.DeepEqual(stored, want) {
					t.Fatalf("observation changed another field, scope, or transaction: %+v, %v", stored, err)
				}
				actual, raw, err := DecisionReadView(q, want.ProjectID, want.ID)
				if err != nil || actual != version || !bytes.Equal(raw, view) {
					t.Fatalf("observation lost view or refresh crossed scope: %q, %x, %v", actual, raw, err)
				}
			}
			for _, scope := range [][2]string{{"missing", "run"}, {"p", "missing"}} {
				if _, _, err := DecisionReadView(q, scope[0], scope[1]); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("view crossed scope %v: %v", scope, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	verify(false)
	if err := s.Do(ctx, write); err != nil {
		t.Fatal(err)
	}
	verify(true)
}
