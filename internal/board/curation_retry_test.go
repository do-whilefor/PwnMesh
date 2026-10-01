package board

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func failedCuratorFixture(t *testing.T, failure, cause string) (*orchestrationFixture, Execution) {
	t.Helper()
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	f.fact(producer, "observation")
	e, _ := f.curator("first")
	failCurator(t, f, e, failure, cause)
	return f, e
}

func failCurator(t *testing.T, f *orchestrationFixture, e Execution, failure, cause string) {
	t.Helper()
	// The text deliberately resembles a network failure even for denied cases:
	// old records lacking typed failure_cause must not gain retry permission.
	result, _ := json.Marshal(map[string]any{"status": "failed", "failure_kind": failure, "failure_cause": cause, "error": "transport: model stream read failed: context deadline exceeded"})
	f.do(func(tx *Tx) error { return tx.ExecutionStatus(e, "failed", result) })
}

func curatorRetryCheck(f *orchestrationFixture) ExecutionCheck {
	f.t.Helper()
	var check ExecutionCheck
	f.do(func(tx *Tx) error {
		state, err := tx.State("p")
		if err != nil {
			return err
		}
		check, err = tx.CheckExecutions(ExecutionCheckQuery{ProjectID: "p", Namespace: "test", Kind: "curate", Generation: state.Graph.Project.Generation, RetryKey: CurationRetryKey(state)})
		return err
	})
	return check
}

func registerCuratorSuccessor(f *orchestrationFixture, id, previous string) (Execution, error) {
	e := Execution{ProjectID: "p", ID: id, Namespace: "test", Backend: "curator", Kind: "curate", Lease: "curator@" + id}
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		if _, err := tx.ClaimCurator("p", e.Lease, "infrastructure_recovery"); err != nil {
			return err
		}
		state, err := tx.State("p")
		if err != nil {
			return err
		}
		e.RetryKey = CurationRetryKey(state)
		e.Job, _ = json.Marshal(map[string]any{"kind": "curate", "run_id": id, "previous_run_id": previous, "graph": state.Graph, "state": state, "graph_rpc": true, "result_contract_version": 2})
		return tx.RegisterExecution(e)
	})
	return e, err
}

func TestAutomaticCuratorRetryRequiresInfrastructureProvenance(t *testing.T) {
	for _, tc := range []struct {
		failure, cause string
		allowed        bool
	}{
		{"transport", "", true}, {"request_timeout", "", true}, {"rate_limit", "", true}, {"unavailable", "", true}, {"transient_infrastructure", "", true},
		{"budget_exhausted", "transport", true}, {"recovery_exhausted", "rate_limit", true},
		{"budget_exhausted", "", false}, {"recovery_exhausted", "", false}, {"budget_exhausted", "provider", false},
		{"invalid_output", "transport", false}, {"result_contract", "transport", false}, {"configuration", "transport", false},
		{"hard_cancelled", "transport", false}, {"state_changed", "transport", false}, {"execution", "", false},
	} {
		t.Run(tc.failure+"_"+tc.cause, func(t *testing.T) {
			f, e := failedCuratorFixture(t, tc.failure, tc.cause)
			check := curatorRetryCheck(f)
			if (check.AutomaticRetryID == e.ID) != tc.allowed || !check.Blocked {
				t.Fatalf("retry discovery mismatched eligibility: %+v", check)
			}
			err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.RequestAutomaticDecisionRetry(e) })
			if tc.allowed && err != nil {
				t.Fatal(err)
			} else if !tc.allowed {
				requireAPIStatus(t, err, 409)
			}
		})
	}
}

func TestAutomaticCuratorRetryGrantIsDurableBoundedAndPreservesHistory(t *testing.T) {
	f, first := failedCuratorFixture(t, "budget_exhausted", "transport")
	var original Execution
	f.do(func(tx *Tx) error { var err error; original, err = tx.Execution("p", first.ID); return err })
	for range 2 {
		f.do(func(tx *Tx) error { return tx.RequestAutomaticDecisionRetry(first) })
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if check := curatorRetryCheck(f); check.PreviousRunID != first.ID || check.Blocked {
		t.Fatalf("restart lost its one-use authorization: %+v", check)
	}
	second, err := registerCuratorSuccessor(f, "second", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	failCurator(t, f, second, "transport", "")
	err = f.store.Do(context.Background(), func(tx *Tx) error { return tx.RequestAutomaticDecisionRetry(second) })
	requireAPIStatus(t, err, 409)
	if check := curatorRetryCheck(f); check.AutomaticRetryID != "" || !check.Blocked || check.Attempts != 2 {
		t.Fatalf("successor failure replenished the allowance: %+v", check)
	}
	f.do(func(tx *Tx) error {
		after, err := tx.Execution("p", first.ID)
		if err == nil && (after.Status != "retried" || string(after.Job) != string(original.Job) || string(after.Result) != string(original.Result)) {
			t.Fatal("successor changed the original input or failure evidence")
		}
		return err
	})
}

func TestAutomaticCuratorRetryCannotCrossSafetyBoundaries(t *testing.T) {
	for _, mode := range []string{"stopped", "restart", "stale", "other_lease", "pending", "other_pending", "receipt", "rejected", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f, e := failedCuratorFixture(t, "transport", "")
			f.do(func(tx *Tx) error {
				switch mode {
				case "stopped":
					g, err := tx.Load("p")
					if err != nil {
						return err
					}
					if err = tx.SetStatus(&g, "stopped"); err != nil {
						return err
					}
					return tx.Save(g)
				case "restart":
					_, err := tx.RestartProject("p", nil)
					return err
				case "stale":
					g, err := tx.Load("p")
					if err != nil {
						return err
					}
					g.Hints = append(g.Hints, Hint{ID: "later", Content: "Later evidence", Creator: "fixture", CreatedAt: tx.Now})
					return tx.Save(g)
				case "other_lease":
					if err := tx.ReleaseCurator("p", e.Lease); err != nil {
						return err
					}
					_, err := tx.ClaimCurator("p", "other@curator", "other input")
					return err
				case "pending":
					_, err := tx.Exec("UPDATE xloom_executions SET status='running' WHERE project_id='p' AND id=?", e.ID)
					return err
				case "other_pending":
					if err := tx.ReleaseCurator("p", e.Lease); err != nil {
						return err
					}
					other := Execution{ProjectID: "p", ID: "other", Namespace: "other-dispatcher", Backend: "curator", Kind: "curate", Lease: "curator@other"}
					if _, err := tx.ClaimCurator("p", other.Lease, "observations"); err != nil {
						return err
					}
					state, err := tx.State("p")
					if err != nil {
						return err
					}
					other.RetryKey = CurationRetryKey(state)
					other.Job, _ = json.Marshal(map[string]any{"kind": "curate", "run_id": other.ID, "graph": state.Graph, "state": state, "graph_rpc": true, "result_contract_version": 2})
					if err := tx.RegisterExecution(other); err != nil {
						return err
					}
					// A persisted pending run still owns recovery priority after its
					// live lease disappears; this must not rely on lease conflict.
					return tx.ReleaseCurator("p", other.Lease)
				case "receipt":
					// Simulate a lost delivery after an accepted transaction without
					// changing the content hash, to exercise the independent receipt guard.
					request, _ := json.Marshal(map[string]any{"Run": e.Lease, "Op": "curate"})
					response, _ := json.Marshal(StateActionResult{Op: "curate", Committed: true})
					_, err := tx.Exec("INSERT INTO xloom_state_actions(project_id,idempotency_key,request,response) VALUES('p','committed',?,?)", string(request), string(response))
					return err
				default:
					_, err := tx.Exec("UPDATE xloom_executions SET status=? WHERE project_id='p' AND id=?", mode, e.ID)
					return err
				}
			})
			before := f.state()
			err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.RequestAutomaticDecisionRetry(e) })
			if err == nil {
				t.Fatal("automatic retry crossed " + mode)
			}
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("rejected retry changed another role's state or lease")
			}
		})
	}
}

func TestCuratorGrantCannotBeConsumedByNewInput(t *testing.T) {
	f, first := failedCuratorFixture(t, "transport", "")
	f.do(func(tx *Tx) error { return tx.RequestAutomaticDecisionRetry(first) })
	producer := f.worker("later-producer", "")
	f.fact(producer, "later-evidence")
	if check := curatorRetryCheck(f); check.PreviousRunID != "" || check.Blocked {
		t.Fatalf("new input inherited an obsolete retry grant: %+v", check)
	}
	_, err := registerCuratorSuccessor(f, "wrong-successor", first.ID)
	requireAPIStatus(t, err, 409)
	next, err := registerCuratorSuccessor(f, "fresh-input", "")
	if err != nil {
		t.Fatal(err)
	}
	failCurator(t, f, next, "transport", "")
	// The stale grant must also not block this new input's own bounded allowance.
	f.do(func(tx *Tx) error { return tx.RequestAutomaticDecisionRetry(next) })
	if check := curatorRetryCheck(f); check.PreviousRunID != next.ID {
		t.Fatalf("obsolete authorization blocked new input recovery: %+v", check)
	}
}
