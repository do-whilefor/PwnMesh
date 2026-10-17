package board

import (
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board/internal/sqlstore"
)

type transactionQueries = sqlstore.Queries

func TestStateActionAndRegistrationRollbackReleaseFailure(t *testing.T) {
	for _, operation := range []string{"action", "registration"} {
		t.Run(operation, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			var execution Execution
			if operation == "registration" {
				execution = dependencyRun(f, "release", f.step("release fixture", ""))
			}
			before := f.state()
			failure := errors.New("outer savepoint release failed")
			f.do(func(tx *Tx) error {
				original := tx.transactionQueries
				injected := &outerReleaseFailureQueries{transactionQueries: original, failure: failure}
				tx.transactionQueries = injected
				var err error
				if operation == "action" {
					var result StateActionResult
					result, err = tx.StateAction("p", f.planner, StateAction{Op: "goal", IdempotencyKey: "release", Payload: json.RawMessage(`{"action":"add","condition":"Uncommitted goal"}`)})
					if !reflect.DeepEqual(result, StateActionResult{}) {
						t.Error("failed action returned an uncommitted result")
					}
				} else {
					err = tx.RegisterExecution(execution)
				}
				tx.transactionQueries = original
				if !errors.Is(err, failure) || !injected.fired {
					t.Fatalf("operation did not reach release failure: %v", err)
				}
				return tx.UpdateSettings(Settings{IntentTimeout: 25, ReasonTimeout: 35})
			})
			if !reflect.DeepEqual(f.state(), before) {
				t.Error("handled release failure committed partial state")
			}
			f.do(func(tx *Tx) error {
				if operation == "registration" {
					_, err := tx.Execution("p", execution.ID)
					requireAPIStatus(t, err, 404)
				}
				settings, err := tx.Settings()
				if err == nil && (settings.IntentTimeout != 25 || settings.ReasonTimeout != 35) {
					t.Error("operation rollback discarded caller writes")
				}
				return err
			})
		})
	}
}

type failureProjectionQueries struct {
	transactionQueries
	eventReads int
}

func (q *failureProjectionQueries) Query(query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "FROM pwnmesh_state_events") {
		q.eventReads++
	}
	return q.transactionQueries.Query(query, args...)
}

func (q *failureProjectionQueries) QueryRow(query string, args ...any) *sql.Row {
	if strings.Contains(query, "FROM pwnmesh_state_events") {
		q.eventReads++
	}
	return q.transactionQueries.QueryRow(query, args...)
}

func TestStateBatchesFailureDiagnostics(t *testing.T) {
	f := newPlanFixture(t)
	steps := []string{}
	for _, key := range []string{"empty", "missing", "recorded"} {
		steps = append(steps, f.action("step", key, stepInput("Check "+key, []string{"origin"}, "goal", 0)).ID)
	}
	check := func(wantReads int, reasons []string) {
		f.tx(func(tx *Tx) error {
			queries := &failureProjectionQueries{transactionQueries: tx.transactionQueries}
			tx.transactionQueries = queries
			state, err := tx.State("proj_001")
			if err != nil {
				return err
			}
			if queries.eventReads != wantReads {
				t.Fatalf("state read %d failure-history queries, want %d", queries.eventReads, wantReads)
			}
			if reasons != nil {
				for n, step := range state.Steps {
					if step.ID != steps[n] || step.Status != "failed" || step.Reason != reasons[n] {
						t.Fatalf("failed Step %d lost its latest diagnostic or fallback: %+v", n, step)
					}
				}
			}
			return nil
		})
	}
	check(0, nil)
	f.tx(func(tx *Tx) error {
		for n, status := range []string{"failed", "rejected", "cancelled"} {
			putQueryExecution(t, tx, Execution{ProjectID: "proj_001", ID: steps[n], Kind: "explore", Intent: steps[n], Status: status, Job: json.RawMessage("unavailable")}, 0, "")
		}
		for n, event := range []struct{ lease, reason string }{
			{"planner@" + steps[0], "obsolete diagnostic"},
			{"planner@" + steps[0], ""},
			{"planner@" + steps[2], "old diagnostic"},
			{"planner@" + steps[2], "latest diagnostic"},
		} {
			raw, _ := json.Marshal(map[string]any{"op": "execution_failed", "run_id": event.lease, "payload": map[string]string{"reason": event.reason}})
			if _, err := tx.transactionQueries.Exec("INSERT INTO pwnmesh_state_events(project_id,revision,event) VALUES('proj_001',?,?)", 100+n, string(raw)); err != nil {
				return err
			}
		}
		return nil
	})
	check(1, []string{"execution failed", "execution rejected", "latest diagnostic"})
}

func TestStateProjectsLatestStepRuntimeWithoutArchivedInputs(t *testing.T) {
	f := newPlanFixture(t)
	step := f.action("step", "step", stepInput("Check fixture", []string{"origin"}, "goal", 0)).ID
	f.tx(func(tx *Tx) error {
		for _, e := range []Execution{
			{ID: "old", Kind: "explore", Status: "failed", Result: json.RawMessage(`{"error":"obsolete failure"}`)},
			{ID: "latest", Kind: "explore", Status: "rejected", Result: json.RawMessage(`{"text":"{\"reason\":\"fixture declined\"}"}`)},
			{ID: "planner", Kind: "reason", Status: "failed", Result: json.RawMessage(`{"error":"unrelated planner"}`)},
		} {
			e.ProjectID, e.Intent, e.Job = "proj_001", step, json.RawMessage("archived, intentionally not JSON")
			putQueryExecution(t, tx, e, 0, "")
		}
		return nil
	})
	state := f.state()
	if len(state.Steps) != 1 || state.Steps[0].Status != "failed" || state.Steps[0].Reason != "execution rejected" {
		t.Fatalf("wrong latest execution projection: %+v", state.Steps)
	}
	f.tx(func(tx *Tx) error {
		putQueryExecution(t, tx, Execution{ProjectID: "proj_001", ID: "retry", Kind: "explore", Intent: step, Status: "running", Job: json.RawMessage("archived")}, 0, "")
		return nil
	})
	if step := f.state().Steps[0]; step.Status == "failed" || step.Reason != "" {
		t.Fatalf("old failure leaked into the retry: %+v", step)
	}
}

func TestStateFailureUsesEventsWithoutReadingRetainedResults(t *testing.T) {
	f := newPlanFixture(t)
	step := f.action("step", "step", stepInput("Check fixture", []string{"origin"}, "goal", 0)).ID
	f.tx(func(tx *Tx) error {
		result, _ := json.Marshal(map[string]string{"error": strings.Repeat("证据", 4000), "failure_kind": "request_timeout", "text": strings.Repeat("unused", 1<<18)})
		putQueryExecution(t, tx, Execution{ProjectID: "proj_001", ID: "failure", Kind: "explore", Intent: step, Status: "failed", Result: result}, 0, "")
		return nil
	})
	if reason := f.state().Steps[0].Reason; reason != "execution failed" {
		t.Fatalf("missing event read a retained result: %q", reason)
	}
	f.tx(func(tx *Tx) error {
		_, err := tx.transactionQueries.Exec(`UPDATE pwnmesh_executions SET result='unavailable' WHERE id='failure'`)
		return err
	})
	if reason := f.state().Steps[0].Reason; reason != "execution failed" {
		t.Fatalf("missing event should use status: %q", reason)
	}
	f.tx(func(tx *Tx) error {
		event, _ := json.Marshal(StateEvent{Revision: 2, Op: "execution_failed", ID: step, RunID: "planner@failure", Payload: json.RawMessage(`{"reason":"recorded graph failure"}`)})
		_, err := tx.transactionQueries.Exec(`INSERT INTO pwnmesh_state_events(project_id,revision,event) VALUES('proj_001',2,?)`, string(event))
		return err
	})
	if reason := f.state().Steps[0].Reason; reason != "recorded graph failure" {
		t.Fatalf("graph event did not remain authoritative: %q", reason)
	}
}
