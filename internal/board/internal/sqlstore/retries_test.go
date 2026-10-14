package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestRetryProjectStateRetainsScopeAndRoundDefaults(t *testing.T) {
	store := actionReceiptStore(t)
	if err := store.Do(context.Background(), func(q Queries) error {
		if _, err := q.Exec("UPDATE projects SET status='stopped',reason_worker='planner@run' WHERE id='p'"); err != nil {
			return err
		}
		if _, err := q.Exec("INSERT INTO pwnmesh_project_rounds(project_id,generation,restarted_at) VALUES('p',4,'now')"); err != nil {
			return err
		}
		status, generation, worker, err := RetryProjectState(q, "p")
		if err != nil || status != "stopped" || generation != 4 || worker == nil || *worker != "planner@run" {
			t.Fatalf("retry owner changed: %q, %d, %v, %v", status, generation, worker, err)
		}
		status, generation, worker, err = RetryProjectState(q, "other")
		if err != nil || status != "active" || generation != 0 || worker != nil {
			t.Fatalf("retry owner crossed project or lost defaults: %q, %d, %v, %v", status, generation, worker, err)
		}
		if _, _, _, err := RetryProjectState(q, "missing"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("missing project = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRetryPendingReadsRetainScopeAndGrantPolicy(t *testing.T) {
	store := actionReceiptStore(t)
	if err := store.Do(context.Background(), func(q Queries) error {
		for _, status := range []string{"prepared", "running", "retryable", "result_pending", "retry_requested", "failed", "rejected", "cancelled", "succeeded", "retried"} {
			for _, scope := range []string{"same-task", "project", "excluded-id", "kind", "intent", "retry-key", "generation"} {
				row := model.ExecutionSummary{ProjectID: "p", ID: "other", Namespace: "foreign-dispatcher", Kind: "curate", Intent: "step", RetryKey: "key", Generation: 3, Status: status}
				switch scope {
				case "project":
					row.ProjectID = "other"
				case "excluded-id":
					row.ID = "current"
				case "kind":
					row.Kind = "explore"
				case "intent":
					row.Intent = "other-step"
				case "retry-key":
					row.RetryKey = "other-key"
				case "generation":
					row.Generation = 2
				}
				if _, err := q.Exec("DELETE FROM pwnmesh_executions"); err != nil {
					return err
				}
				putExecutionSummary(t, q, row)
				active := slices.Contains([]string{"prepared", "running", "retryable", "result_pending", "retry_requested"}, status)
				for _, matchGrant := range []bool{false, true} {
					want := active && scope != "project" && scope != "excluded-id" && scope != "kind"
					if matchGrant && status == "retry_requested" && (scope == "retry-key" || scope == "generation") {
						want = false
					}
					got, err := OtherDecisionRetryPending(q, "p", "current", "curate", matchGrant, "key", 3)
					if err != nil || got != want {
						t.Fatalf("decision pending %s/%s, match=%v: %v, %v", status, scope, matchGrant, got, err)
					}
				}
				want := active && scope != "project" && scope != "excluded-id" && scope != "intent"
				got, err := OtherStepRetryPending(q, "p", "step", "current")
				if err != nil || got != want {
					t.Fatalf("step pending %s/%s: %v, %v", status, scope, got, err)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRepairFailureReadKeepsStoredScopeAndErrors(t *testing.T) {
	store := actionReceiptStore(t)
	if err := store.Do(context.Background(), func(q Queries) error {
		putExecutionSummary(t, q, model.ExecutionSummary{ProjectID: "p", ID: "run"})
		putExecutionSummary(t, q, model.ExecutionSummary{ProjectID: "other", ID: "run"})
		if _, err := q.Exec(`UPDATE pwnmesh_executions SET result='{"failure_kind":"repair_stale"}' WHERE project_id='other'`); err != nil {
			return err
		}
		for _, test := range []struct {
			result any
			kind   string
		}{
			{nil, ""}, {`{}`, ""}, {`{"failure_kind":"transport"}`, "transport"},
			{`{"failure_kind":false}`, "0"}, {`{"failure_kind":"repair_stale"}`, "repair_stale"},
			{`invalid historical JSON`, ""},
		} {
			if _, err := q.Exec("UPDATE pwnmesh_executions SET result=? WHERE project_id='p' AND id='run'", test.result); err != nil {
				return err
			}
			kind, err := ExecutionFailureKind(q, "p", "run")
			if test.result == `invalid historical JSON` {
				if err == nil || errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("corrupt stored result lost its read error: %v", err)
				}
			} else if err != nil || kind != test.kind {
				t.Fatalf("stored failure kind changed for %v: %q, %v", test.result, kind, err)
			}
		}
		for _, scope := range [][2]string{{"missing", "run"}, {"p", "missing"}} {
			if _, err := ExecutionFailureKind(q, scope[0], scope[1]); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("repair read crossed scope %v: %v", scope, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRetryGrantRetainsOpaqueHistoryAndCallerTransaction(t *testing.T) {
	store := actionReceiptStore(t)
	ctx := context.Background()
	before := map[string]model.Execution{}
	if err := store.Do(ctx, func(q Queries) error {
		for _, scope := range [][2]string{{"p", "run"}, {"p", "other"}, {"other", "run"}} {
			putExecutionSummary(t, q, model.ExecutionSummary{ProjectID: scope[0], ID: scope[1], Kind: "reason", Status: "failed", UpdatedAt: "settled", Resumes: 2})
			if _, err := q.Exec("UPDATE pwnmesh_executions SET job=?,result=? WHERE project_id=? AND id=?", []byte{' ', 0xff, '\n'}, []byte{0, 0xfe, '\r', '\n'}, scope[0], scope[1]); err != nil {
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
	for _, commit := range []bool{false, true} {
		rejected := errors.New("caller rejected retry")
		err := store.Do(ctx, func(q Queries) error {
			if err := SaveExecutionRetryGrant(q, "p", "run", "granted"); err != nil {
				return err
			}
			if !commit {
				return rejected
			}
			return nil
		})
		if commit && err != nil || !commit && !errors.Is(err, rejected) {
			t.Fatalf("grant transaction error: %v", err)
		}
		if err := store.View(ctx, func(q Queries) error {
			for _, want := range before {
				if commit && want.ProjectID == "p" && want.ID == "run" {
					want.Status, want.UpdatedAt = "retry_requested", "granted"
				}
				got, err := Execution(q, want.ProjectID, want.ID)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("grant changed saved history, scope or transaction: %+v, %v", got, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
