package board

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestManagementOperationsOwnProjectLifecycle(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.Do(context.Background(), func(tx *Tx) error {
		graph, err := tx.CreateProject(ProjectInput{Title: "Inspect fixture", Origin: "Local fixture", Goal: "Explain its behavior", Scenario: "audit", StartPaused: true, Hints: []HintInput{{Content: "Keep the evidence", Creator: "user"}}})
		if err != nil {
			return err
		}
		id := graph.Project.ID
		if id != "proj_001" || graph.Project.Status != "stopped" || graph.Project.OrchestrationVersion != 1 || graph.Project.Bootstrap || len(graph.Hints) != 1 || graph.Hints[0].ID != "h001" || graph.Intents == nil {
			t.Fatalf("creation lost canonical initial state: %+v", graph)
		}
		if err = tx.RequireCurrentProject(id); err != nil {
			return err
		}
		stale := int64(1)
		_, err = tx.SetProjectStatus(id, "active", &stale)
		requireAPIStatus(t, err, 409)
		current := int64(0)
		if _, err = tx.SetProjectStatus(id, "active", &current); err != nil {
			return err
		}
		if _, err = tx.SetProjectTitle(id, "Renamed fixture"); err != nil {
			return err
		}
		hint, err := tx.AddProjectHint(id, HintInput{Content: "Recheck the boundary", Creator: "user"}, "human-request")
		if err != nil {
			return err
		}
		state, err := tx.State(id)
		if err != nil {
			return err
		}
		if hint.ID != "h002" || state.Revision != 1 || state.DecisionRevision != 1 || state.Graph.Project.Title != "Renamed fixture" || state.Graph.Project.Scenario != "audit" {
			t.Fatalf("management changed unrelated state or lost human-input revision: %+v", state)
		}
		var raw string
		if err = tx.QueryRow("SELECT event FROM pwnmesh_state_events WHERE project_id=?", id).Scan(&raw); err != nil {
			return err
		}
		var event StateEvent
		if err = json.Unmarshal([]byte(raw), &event); err != nil {
			return err
		}
		if event.Op != "hint" || event.ID != hint.ID || event.RunID != "human-request" {
			t.Fatalf("human input did not publish its event: %+v", event)
		}
		if err = tx.UpdateSettings(Settings{IntentTimeout: 25, ReasonTimeout: 35}); err != nil {
			return err
		}
		settings, err := tx.Settings()
		if err != nil {
			return err
		}
		if settings.IntentTimeout != 25 || settings.ReasonTimeout != 35 {
			t.Fatalf("settings update was lost: %+v", settings)
		}
		if err = tx.DeleteProject(id); err != nil {
			return err
		}
		var count int
		err = tx.QueryRow("SELECT (SELECT COUNT(*) FROM facts)+(SELECT COUNT(*) FROM hints)+(SELECT COUNT(*) FROM pwnmesh_state_events)").Scan(&count)
		if count != 0 {
			t.Fatalf("project deletion retained dependent rows: %d", count)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReopenProjectRollsBackAdmissionAndPublishesHumanInput(t *testing.T) {
	f := newOrchestrationFixture(t)
	f.do(func(tx *Tx) error {
		graph, err := tx.Load("p")
		if err != nil {
			return err
		}
		graph.Project.Status = "completed"
		graph.Facts = append(graph.Facts, Fact{ID: "proof", Description: "Observed fixture"})
		graph.Intents = append(graph.Intents, Intent{ID: "completion", From: []string{"proof"}, To: Ptr("goal"), Description: "Accepted result", Worker: Ptr("old-worker"), CreatedAt: tx.Now, ConcludedAt: Ptr(tx.Now)})
		return tx.Save(graph)
	})
	before := f.state()
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.ReopenProject("p", strings.Repeat("x", DefaultContextViewBytes+1), "user", "request")
		return err
	})
	requireAPIStatus(t, err, 422)
	if after := f.state(); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected feedback changed completion, facts or revisions")
	}
	f.do(func(tx *Tx) error {
		revoked, err := tx.RunRevoked("p", "old-worker")
		if err != nil {
			return err
		}
		if revoked {
			t.Fatal("rejected feedback retained revocations")
		}
		result, err := tx.ReopenProject("p", "Observe the new response", "user", "request")
		if err != nil {
			return err
		}
		if result.Fact.ID != "f001" || result.Intent.ID != "i001" || result.Project.Status != "active" || result.Project.Reason != nil || Value(result.Intent.To) != result.Fact.ID {
			t.Fatalf("reopen lost identities or consumed IDs on rollback: %+v", result)
		}
		revoked, err = tx.RunRevoked("p", "old-worker")
		if err != nil {
			return err
		}
		if !revoked {
			t.Fatal("reopen retained authority from completed work")
		}
		state, err := tx.State("p")
		if err != nil {
			return err
		}
		_, ids := state.UserInputFacts()
		if !ids[result.Fact.ID] || state.Revision != 1 || state.DecisionRevision != 1 || len(state.Graph.Intents) != 1 {
			t.Fatalf("reopen lost human input or retained completion: %+v", state)
		}
		return nil
	})
}

func TestManagementOperationsRollbackHandledFailure(t *testing.T) {
	for _, operation := range []string{"create", "hint", "reopen", "status", "restart_archive", "restart_reset", "terminate"} {
		t.Run(operation, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			if operation == "reopen" {
				f.do(func(tx *Tx) error {
					graph, err := tx.Load("p")
					if err != nil {
						return err
					}
					graph.Project.Status = "completed"
					graph.Facts = append(graph.Facts, Fact{ID: "proof", Description: "Observed fixture"})
					graph.Intents = append(graph.Intents, Intent{ID: "completion", From: []string{"proof"}, To: Ptr("goal"), Description: "Accepted result", Worker: Ptr("old-worker"), CreatedAt: tx.Now, ConcludedAt: Ptr(tx.Now)})
					return tx.Save(graph)
				})
			}
			if operation == "status" {
				f.do(func(tx *Tx) error {
					_, err := tx.Exec(`CREATE TRIGGER reject_management_status BEFORE UPDATE OF status ON projects WHEN NEW.status='stopped' BEGIN SELECT RAISE(ABORT,'status persistence failed'); END`)
					return err
				})
			}
			if strings.HasPrefix(operation, "restart_") || operation == "terminate" {
				pending := f.worker("pending", "")
				retry := f.worker("retry", "")
				f.do(func(tx *Tx) error {
					if err := tx.ExecutionStatus(pending, "result_pending", json.RawMessage(`{"text":"retained result"}`)); err != nil {
						return err
					}
					if err := tx.ExecutionStatus(retry, "failed", json.RawMessage(`{"error":"retained failure"}`)); err != nil {
						return err
					}
					if err := tx.ExecutionStatus(retry, "retry_requested", nil); err != nil {
						return err
					}
					if _, err := tx.Exec("INSERT INTO pwnmesh_paused_executions(project_id,execution_id) VALUES('p',?)", pending.ID); err != nil {
						return err
					}
					triggers := map[string]string{
						"restart_archive": "BEFORE INSERT ON pwnmesh_round_entries WHEN NEW.kind='inputs'",
						"restart_reset":   "BEFORE INSERT ON pwnmesh_project_rounds",
						"terminate":       "BEFORE INSERT ON pwnmesh_project_termination",
					}
					_, err := tx.Exec("CREATE TRIGGER reject_management_lifecycle " + triggers[operation] + " BEGIN SELECT RAISE(ABORT,'lifecycle persistence failed'); END")
					return err
				})
			}
			snapshot := func(tx *Tx) (string, error) {
				state, err := tx.State("p")
				if err != nil {
					return "", err
				}
				raw, err := json.Marshal(state)
				if err != nil {
					return "", err
				}
				var projects, facts, hints, intents, revoked, paused, events, counters, scopedCount, scopedValue int
				err = tx.QueryRow(`SELECT (SELECT COUNT(*) FROM projects),(SELECT COUNT(*) FROM facts),(SELECT COUNT(*) FROM hints),(SELECT COUNT(*) FROM intents),(SELECT COUNT(*) FROM pwnmesh_revoked_runs),(SELECT COUNT(*) FROM pwnmesh_paused_executions),(SELECT COUNT(*) FROM pwnmesh_state_events),(SELECT COALESCE(SUM(value),0) FROM counters),(SELECT COUNT(*) FROM scoped_counters),(SELECT COALESCE(SUM(value),0) FROM scoped_counters)`).Scan(&projects, &facts, &hints, &intents, &revoked, &paused, &events, &counters, &scopedCount, &scopedValue)
				if err != nil {
					return "", err
				}
				var rounds, entries, terminations, executionCount, uiRevision int
				var executionBytes string
				err = tx.QueryRow(`SELECT (SELECT COUNT(*) FROM pwnmesh_round_history),(SELECT COUNT(*) FROM pwnmesh_round_entries),(SELECT COUNT(*) FROM pwnmesh_project_termination),(SELECT COUNT(*) FROM pwnmesh_executions),(SELECT COALESCE(SUM(revision),0) FROM pwnmesh_ui_execution_revisions),(SELECT COALESCE(json_group_array(json_object('id',id,'status',status,'job',CAST(job AS TEXT),'result',CAST(result AS TEXT),'updated_at',updated_at)),'[]') FROM pwnmesh_executions ORDER BY rowid)`).Scan(&rounds, &entries, &terminations, &executionCount, &uiRevision, &executionBytes)
				return fmt.Sprintf("%s|%d,%d,%d,%d,%d,%d,%d,%d,%d,%d|%d,%d,%d,%d,%d|%s", raw, projects, facts, hints, intents, revoked, paused, events, counters, scopedCount, scopedValue, rounds, entries, terminations, executionCount, uiRevision, executionBytes), err
			}
			var before string
			f.do(func(tx *Tx) error { var err error; before, err = snapshot(tx); return err })
			f.do(func(tx *Tx) error {
				if err := tx.UpdateSettings(Settings{IntentTimeout: 25, ReasonTimeout: 35}); err != nil {
					return err
				}
				oversized := strings.Repeat("x", DefaultContextViewBytes+1)
				var err error
				switch operation {
				case "create":
					_, err = tx.CreateProject(ProjectInput{Title: "Rejected fixture", Origin: oversized, Goal: "Explain it"})
				case "hint":
					_, err = tx.AddProjectHint("p", HintInput{Content: oversized, Creator: "user"}, "request")
				case "reopen":
					_, err = tx.ReopenProject("p", oversized, "user", "request")
				case "status":
					_, err = tx.SetProjectStatus("p", "stopped", nil)
				case "restart_archive", "restart_reset":
					_, err = tx.RestartProject("p", nil)
				case "terminate":
					_, err = tx.TerminateProject("p", nil)
				}
				if err == nil {
					t.Fatal("invalid operation unexpectedly succeeded")
				}
				if (strings.HasPrefix(operation, "restart_") || operation == "terminate") && !strings.Contains(err.Error(), "lifecycle persistence failed") {
					t.Fatalf("operation failed before reaching the injected fault: %v", err)
				}
				settings, err := tx.Settings()
				if err != nil {
					return err
				}
				if settings.IntentTimeout != 25 || settings.ReasonTimeout != 35 {
					t.Fatal("operation rollback discarded earlier transaction work")
				}
				// Handling a rejection permits subsequent work in the same transaction.
				return tx.UpdateSettings(Settings{IntentTimeout: 45, ReasonTimeout: 55})
			})
			f.do(func(tx *Tx) error {
				after, err := snapshot(tx)
				if err == nil && before != after {
					t.Fatal("handled failure committed partial management state or authority")
				}
				if err != nil {
					return err
				}
				settings, err := tx.Settings()
				if err == nil && (settings.IntentTimeout != 45 || settings.ReasonTimeout != 55) {
					t.Fatal("handled rejection prevented later transaction work from committing")
				}
				return err
			})
		})
	}
}
