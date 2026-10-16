package board

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func recoveryFixture(t *testing.T, kind string) (*orchestrationFixture, Execution) {
	t.Helper()
	f := newOrchestrationFixture(t)
	worker := f.worker("recovery-worker", "")
	curator, _ := f.curator("recovery-curator")
	planner := Execution{ProjectID: "p", ID: "plan", Namespace: "test", Backend: "planner", Kind: "reason", Lease: f.planner.Run, RetryKey: "reason:recovery"}
	f.do(func(tx *Tx) error {
		state, err := tx.State("p")
		if err != nil {
			return err
		}
		planner.Job, err = json.Marshal(map[string]any{"kind": "reason", "run_id": planner.ID, "graph": state.Graph, "state": state, "graph_rpc": true, "result_contract_version": 2, "decision": map[string]any{"version": 2, "state_version": DecisionStateVersion(state)}})
		if err != nil {
			return err
		}
		return tx.RegisterExecution(planner)
	})
	return f, map[string]Execution{"reason": planner, "curate": curator, "explore": worker}[kind]
}

func releaseRecoveryLease(tx *Tx, e Execution) error {
	switch e.Kind {
	case "reason":
		_, err := tx.ReasonLease("p", LeaseOperation{Op: "release", Worker: e.Lease})
		return err
	case "curate":
		return tx.ReleaseCurator("p", e.Lease)
	default:
		_, err := tx.IntentLease("p", e.Intent, LeaseOperation{Op: "release", Worker: e.Lease})
		return err
	}
}

func TestResumeExecutionHandledDebitFailureKeepsLeaseAndBudgetAtomic(t *testing.T) {
	for _, kind := range []string{"reason", "curate", "explore"} {
		t.Run(kind, func(t *testing.T) {
			f, e := recoveryFixture(t, kind)
			var before Execution
			f.do(func(tx *Tx) error {
				if err := releaseRecoveryLease(tx, e); err != nil {
					return err
				}
				var err error
				before, err = tx.Execution("p", e.ID)
				if err != nil {
					return err
				}
				_, err = tx.transactionQueries.Exec(`CREATE TRIGGER reject_resume BEFORE UPDATE OF resumes ON pwnmesh_executions BEGIN SELECT RAISE(ABORT,'resume unavailable'); END`)
				return err
			})
			state := f.state()
			f.do(func(tx *Tx) error {
				tx.Now = "2026-09-28T08:00:02Z"
				if err := tx.ResumeExecution(e); err == nil || !strings.Contains(err.Error(), "resume unavailable") {
					t.Fatalf("recovery did not reach injected failure: %v", err)
				}
				return tx.UpdateSettings(Settings{IntentTimeout: 40, ReasonTimeout: 40})
			})
			if after := f.state(); !reflect.DeepEqual(after, state) {
				t.Fatal("handled recovery failure retained a lease or changed shared state")
			}
			f.do(func(tx *Tx) error {
				after, err := tx.Execution("p", e.ID)
				if err != nil || !reflect.DeepEqual(after, before) {
					t.Fatalf("handled recovery failure changed execution: %+v, %v", after, err)
				}
				settings, err := tx.Settings()
				if err != nil || settings.IntentTimeout != 40 || settings.ReasonTimeout != 40 {
					t.Fatalf("recovery rolled back unrelated caller work: %+v, %v", settings, err)
				}
				_, err = tx.transactionQueries.Exec("DROP TRIGGER reject_resume")
				return err
			})
			f.do(func(tx *Tx) error { return tx.ResumeExecution(e) })
			f.do(func(tx *Tx) error {
				after, err := tx.Execution("p", e.ID)
				if err == nil && after.Resumes != before.Resumes+1 {
					t.Fatalf("successful recovery did not debit its allowance: %+v", after)
				}
				return err
			})
		})
	}
}

func TestResumeExecutionWritesOnlyItsLeaseAndRetainsDeliveryAllowance(t *testing.T) {
	for _, kind := range []string{"reason", "curate", "explore"} {
		for _, pending := range []bool{false, true} {
			name := kind + "/execution"
			if pending {
				name = kind + "/delivery"
			}
			t.Run(name, func(t *testing.T) {
				f, e := recoveryFixture(t, kind)
				if pending {
					f.do(func(tx *Tx) error {
						return tx.ExecutionStatus(e, "result_pending", json.RawMessage(`{"status":"success"}`))
					})
				}
				f.do(func(tx *Tx) error { return releaseRecoveryLease(tx, e) })
				before := f.state()
				f.do(func(tx *Tx) error {
					for _, statement := range []string{
						`CREATE TRIGGER guard_resume_project BEFORE UPDATE OF title,status,bootstrap_enabled ON projects BEGIN SELECT RAISE(ABORT,'recovery rewrote project'); END`,
						`CREATE TRIGGER guard_resume_orchestration BEFORE INSERT ON pwnmesh_project_orchestration BEGIN SELECT RAISE(ABORT,'recovery rewrote orchestration'); END`,
						`CREATE TRIGGER guard_resume_fact BEFORE INSERT ON facts BEGIN SELECT RAISE(ABORT,'recovery rewrote facts'); END`,
						`CREATE TRIGGER guard_resume_hint BEFORE INSERT ON hints BEGIN SELECT RAISE(ABORT,'recovery rewrote hints'); END`,
						`CREATE TRIGGER guard_resume_intent BEFORE INSERT ON intents BEGIN SELECT RAISE(ABORT,'recovery rewrote intents'); END`,
						`CREATE TRIGGER guard_resume_sources BEFORE INSERT ON intent_sources BEGIN SELECT RAISE(ABORT,'recovery rewrote sources'); END`,
						`CREATE TRIGGER guard_resume_state BEFORE UPDATE ON pwnmesh_state BEGIN SELECT RAISE(ABORT,'recovery rewrote state'); END`,
					} {
						if _, err := tx.transactionQueries.Exec(statement); err != nil {
							return err
						}
					}
					tx.Now = "2026-09-28T08:00:03Z"
					if err := tx.ResumeExecution(e); err != nil {
						return err
					}
					after, err := tx.Execution("p", e.ID)
					if err != nil {
						return err
					}
					want := 1
					if pending {
						want = 0
					}
					if after.Resumes != want || pending && (after.Status != "result_pending" || string(after.Result) != `{"status":"success"}`) {
						t.Fatalf("recovery changed delivery result or allowance: %+v", after)
					}
					state, err := tx.State("p")
					if err != nil {
						return err
					}
					if err := tx.CheckExecution(state.Graph, e.Fence()); err != nil {
						return err
					}
					if DecisionStateVersion(state) != DecisionStateVersion(before) || state.Revision != before.Revision {
						t.Fatal("recovery changed shared decision input or revision")
					}
					if kind != "reason" && !reflect.DeepEqual(state.Graph.Project.Reason, before.Graph.Project.Reason) || kind != "curate" && !reflect.DeepEqual(state.Graph.Project.Curator, before.Graph.Project.Curator) || kind != "explore" && !reflect.DeepEqual(state.Graph.Intents, before.Graph.Intents) {
						t.Fatal("recovery changed another lease owner")
					}
					return nil
				})
			})
		}
	}
}
