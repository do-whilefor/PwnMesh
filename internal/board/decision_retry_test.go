package board

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAutomaticDecisionRetryRollsBackHandledGrantFailure(t *testing.T) {
	for _, kind := range []string{"reason", "curate"} {
		t.Run(kind, func(t *testing.T) {
			var store *Store
			var execution Execution
			if kind == "curate" {
				fixture, failed := failedCuratorFixture(t, "transport", "")
				store, execution = fixture.store, failed
			} else {
				store = executionQueryStore(t)
				execution = Execution{ProjectID: "p", ID: "failed", Status: "failed", Result: json.RawMessage(`{"status":"failed","failure_kind":"transport"}`)}
				executionQueryTx(t, store, func(tx *Tx) {
					putQueryExecution(t, tx, execution, 0, "")
					if _, err := tx.Exec("UPDATE projects SET reason_worker='planner@failed',reason_trigger='fixture',reason_started_at='started',reason_last_heartbeat_at='heartbeat' WHERE id='p'"); err != nil {
						t.Fatal(err)
					}
				})
			}
			var original Execution
			var project Project
			executionQueryTx(t, store, func(tx *Tx) {
				var err error
				original, err = tx.Execution("p", execution.ID)
				if err != nil {
					t.Fatal(err)
				}
				graph, err := tx.Load("p")
				if err != nil {
					t.Fatal(err)
				}
				project = graph.Project
				// Legacy terminal rows may lack a revocation. Defensive revocation
				// and lease release must roll back when their grant cannot be saved.
				if _, err := tx.Exec("DELETE FROM pwnmesh_revoked_runs WHERE project_id=? AND worker=?", "p", original.Lease); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(`CREATE TRIGGER reject_retry_grant BEFORE UPDATE OF status ON pwnmesh_executions WHEN NEW.status='retry_requested' BEGIN SELECT RAISE(ABORT,'retry grant unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			})
			if err := store.Do(context.Background(), func(tx *Tx) error {
				err := tx.RequestAutomaticDecisionRetry(execution)
				if err == nil || !strings.Contains(err.Error(), "retry grant unavailable") {
					t.Fatalf("expected injected grant failure, got %v", err)
				}
				// Handling this error must still permit an unrelated outer write.
				_, err = tx.Exec("UPDATE projects SET title='outer write' WHERE id='p'")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			executionQueryTx(t, store, func(tx *Tx) {
				current, err := tx.Execution("p", execution.ID)
				if err != nil || !reflect.DeepEqual(current, original) {
					t.Fatalf("failed grant changed saved execution: %+v, %v", current, err)
				}
				graph, err := tx.Load("p")
				if err != nil || graph.Project.Title != "outer write" || !reflect.DeepEqual(graph.Project.Reason, project.Reason) || !reflect.DeepEqual(graph.Project.Curator, project.Curator) {
					t.Fatalf("failed grant released a lease or rolled back caller work: %+v, %v", graph.Project, err)
				}
				revoked, err := tx.RunRevoked("p", original.Lease)
				if err != nil || revoked {
					t.Fatalf("failed grant persisted defensive revocation: %v, %v", revoked, err)
				}
			})
		})
	}
}
