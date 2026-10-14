package sqlstore

import (
	"context"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestIntentExecutionCountIncludesEveryRegisteredAttempt(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Do(context.Background(), func(q Queries) error {
		statuses := []string{"prepared", "running", "retryable", "result_pending", "succeeded", "failed", "rejected", "cancelled", "retry_requested", "retried"}
		for n, status := range statuses {
			// The fixture's job and result are deliberately invalid JSON. An
			// attempt remains counted regardless of its payload or execution scope.
			putExecutionSummary(t, q, model.ExecutionSummary{ProjectID: "p", ID: status, Intent: "review", Namespace: status, Kind: status, Generation: int64(n), Status: status})
		}
		putExecutionSummary(t, q, model.ExecutionSummary{ProjectID: "other", ID: "foreign", Intent: "review"})
		putExecutionSummary(t, q, model.ExecutionSummary{ProjectID: "p", ID: "unrelated", Intent: "other"})
		for _, scope := range []struct {
			project, intent string
			want            int
		}{{"p", "review", len(statuses)}, {"other", "review", 1}, {"p", "other", 1}, {"missing", "review", 0}, {"p", "missing", 0}} {
			count, err := IntentExecutionCount(q, scope.project, scope.intent)
			if err != nil || count != scope.want {
				t.Fatalf("attempt count crossed scope or filtered history for %+v: %d, %v", scope, count, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPendingDecisionExecutionMatchesExactSavedInput(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Do(context.Background(), func(q Queries) error {
		input := DecisionExecutionInput{ProjectID: "p", Lease: "planner@plan", Generation: 3, DecisionRevision: 7, StateVersion: "state"}
		base := model.ExecutionSummary{ProjectID: input.ProjectID, ID: "plan", Namespace: "any", Kind: "reason", Lease: input.Lease, Status: "prepared", Generation: input.Generation, DecisionRevision: input.DecisionRevision, StateVersion: input.StateVersion}
		check := func(row model.ExecutionSummary, job string, want, wantError bool) {
			t.Helper()
			putExecutionSummary(t, q, row)
			if _, err := q.Exec("UPDATE pwnmesh_executions SET job=? WHERE project_id=? AND id=?", job, row.ProjectID, row.ID); err != nil {
				t.Fatal(err)
			}
			exists, err := PendingDecisionExecution(q, input)
			if (err != nil) != wantError || !wantError && exists != want {
				t.Fatalf("planning input match changed for %+v, job %q: %v, %v", row, job, exists, err)
			}
			if _, err := q.Exec("DELETE FROM pwnmesh_executions WHERE project_id=? AND id=?", row.ProjectID, row.ID); err != nil {
				t.Fatal(err)
			}
		}
		const currentJob = `{"decision":{"version":2}}`
		for _, status := range []string{"prepared", "running", "retryable", "result_pending", "succeeded", "failed", "rejected", "cancelled", "retry_requested", "retried"} {
			row := base
			row.Status = status
			pending := status == "prepared" || status == "running" || status == "retryable" || status == "result_pending"
			check(row, currentJob, pending, false)
		}
		for _, field := range []string{"project", "kind", "lease", "generation", "decision_revision", "state_version"} {
			row := base
			switch field {
			case "project":
				row.ProjectID = "other"
			case "kind":
				row.Kind = "curate"
			case "lease":
				row.Lease = "planner@other"
			case "generation":
				row.Generation++
			case "decision_revision":
				row.DecisionRevision++
			case "state_version":
				row.StateVersion = "other"
			}
			check(row, currentJob, false, false)
		}
		for _, job := range []string{`{}`, `{"decision":{"version":1}}`, `{"decision":{"version":"2"}}`} {
			check(base, job, false, false)
		}
		check(base, "malformed", false, true)
		exists, err := PendingDecisionExecution(q, input)
		if err != nil || exists {
			t.Fatalf("absent planner invented a match: %v, %v", exists, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
