package board

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutionChangesMigrateExistingHistoryAndPersistCursors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	executionQueryTx(t, store, func(tx *Tx) {
		putQueryExecution(t, tx, Execution{ID: "existing", Status: "running"}, 0, "")
		// Reproduce a database predating the optional UI change projection.
		_, err := tx.Exec(`DROP TRIGGER xloom_ui_execution_insert; DROP TRIGGER xloom_ui_execution_update; DROP TRIGGER xloom_ui_execution_delete;
DROP TABLE xloom_ui_execution_changes; DROP TABLE xloom_ui_execution_revisions;`)
		if err != nil {
			t.Fatal(err)
		}
	})
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var cursor int64
	executionQueryTx(t, store, func(tx *Tx) {
		page, err := tx.ProjectExecutionChanges("p", 0, 0, 0, 0, 20)
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != "existing" {
			t.Fatalf("migration omitted existing history: %+v %v", page, err)
		}
		cursor = page.Through
	})
	rollback := errors.New("rollback")
	if err = store.Do(context.Background(), func(tx *Tx) error {
		if _, err := tx.Exec("UPDATE xloom_executions SET status='failed' WHERE id='existing'"); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	executionQueryTx(t, store, func(tx *Tx) {
		page, err := tx.ProjectExecutionChanges("p", 0, cursor, 0, 0, 20)
		if err != nil || len(page.Items)+len(page.Deleted) != 0 || page.Through != cursor {
			t.Fatalf("rollback or reopen changed the cursor: %+v %v", page, err)
		}
	})
}

func TestExecutionChangesObserveSameSecondUpdatesAndDeletions(t *testing.T) {
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		putQueryExecution(t, tx, Execution{ID: "finished", Status: "succeeded"}, 0, "")
		putQueryExecution(t, tx, Execution{ID: "mutable", Status: "running"}, 0, "")
		first, err := tx.ProjectExecutionChanges("p", 0, 0, 0, 0, 100)
		if err != nil || len(first.Items) != 2 {
			t.Fatalf("initial execution changes: %+v %v", first, err)
		}
		// Neither the timestamp nor insertion rowid changes here.
		if _, err := tx.Exec(`UPDATE xloom_executions SET status='succeeded',result='{"text":"done","thinking":"private"}' WHERE id='mutable'`); err != nil {
			t.Fatal(err)
		}
		second, err := tx.ProjectExecutionChanges("p", 0, first.Through, 0, 0, 100)
		if err != nil || len(second.Items) != 1 || second.Items[0].ID != "mutable" || second.Items[0].Status != "succeeded" || second.Items[0].Result.Text != "done" || second.Through <= first.Through {
			t.Fatalf("same-second transition lost: %+v %v", second, err)
		}
		raw, _ := json.Marshal(second)
		if strings.Contains(string(raw), "private") || strings.Contains(string(raw), `"job"`) {
			t.Fatal("execution changes exposed private execution fields")
		}
		if _, err := tx.Exec(`UPDATE xloom_executions SET result='{"text":"final observation"}' WHERE id='mutable'`); err != nil {
			t.Fatal(err)
		}
		third, err := tx.ProjectExecutionChanges("p", 0, second.Through, 0, 0, 100)
		if err != nil || len(third.Items) != 1 || third.Items[0].Result.Text != "final observation" {
			t.Fatalf("result-only update lost: %+v %v", third, err)
		}
		if _, err := tx.Exec("DELETE FROM xloom_executions WHERE id='mutable'"); err != nil {
			t.Fatal(err)
		}
		deleted, err := tx.ProjectExecutionChanges("p", 0, third.Through, 0, 0, 100)
		if err != nil || len(deleted.Items) != 0 || len(deleted.Deleted) != 1 || deleted.Deleted[0] != "mutable" {
			t.Fatalf("deletion lost: %+v %v", deleted, err)
		}
		idle, err := tx.ProjectExecutionChanges("p", 0, deleted.Through, 0, 0, 100)
		if err != nil || len(idle.Items)+len(idle.Deleted) != 0 || idle.Through != deleted.Through {
			t.Fatalf("unchanged history replayed: %+v %v", idle, err)
		}
		fresh, err := tx.ProjectExecutionChanges("p", 0, 0, 0, 0, 100)
		if err != nil || len(fresh.Deleted) != 0 || len(fresh.Items) != 1 || fresh.Items[0].ID != "finished" {
			t.Fatalf("initial snapshot replayed obsolete deletions: %+v %v", fresh, err)
		}
		if _, err := tx.Exec("DELETE FROM projects WHERE id='p'"); err != nil {
			t.Fatalf("UI change triggers prevented project deletion: %v", err)
		}
	})
}

func TestExecutionChangesRetainUpdatesPastPageBoundaryAndFenceRestarts(t *testing.T) {
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		putQueryExecution(t, tx, Execution{ID: "first", Status: "running"}, 0, "")
		putQueryExecution(t, tx, Execution{ID: "second", Status: "running"}, 0, "")
		page, err := tx.ProjectExecutionChanges("p", 0, 0, 0, 0, 1)
		if err != nil || len(page.Items) != 1 || page.NextCursor == 0 {
			t.Fatalf("missing initial page boundary: %+v %v", page, err)
		}
		if _, err := tx.Exec("UPDATE xloom_executions SET status='succeeded' WHERE id='second'"); err != nil {
			t.Fatal(err)
		}
		tail, err := tx.ProjectExecutionChanges("p", 0, 0, page.NextCursor, page.Through, 1)
		if err != nil || len(tail.Items) != 0 || tail.Through != page.Through {
			t.Fatalf("page escaped frozen revision: %+v %v", tail, err)
		}
		next, err := tx.ProjectExecutionChanges("p", 0, page.Through, 0, 0, 1)
		if err != nil || len(next.Items) != 1 || next.Items[0].ID != "second" || next.Items[0].Status != "succeeded" {
			t.Fatalf("concurrent update fell behind cursor: %+v %v", next, err)
		}
		if _, err := tx.Exec("INSERT INTO xloom_project_rounds VALUES('p',1,?)", tx.Now); err != nil {
			t.Fatal(err)
		}
		_, err = tx.ProjectExecutionChanges("p", 0, next.Through, 0, 0, 1)
		var api *APIError
		if !errors.As(err, &api) || api.Status != 409 {
			t.Fatalf("old generation retained read authority: %v", err)
		}
	})
}

func TestExecutionChangePagesKeepThePublicResponseByteLimit(t *testing.T) {
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		result, _ := json.Marshal(map[string]string{"text": strings.Repeat("\n", 65536)})
		for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
			putQueryExecution(t, tx, Execution{ID: id, Status: "succeeded", Result: result}, 0, "")
		}
		page, err := tx.ProjectExecutionChanges("p", 0, 0, 0, 0, 100)
		if err != nil || page.NextCursor == 0 {
			t.Fatalf("expected byte-bound continuation: %+v %v", page, err)
		}
		raw, _ := json.Marshal(page)
		if len(raw) > MaxExecutionViewPageBytes {
			t.Fatalf("execution change page grew to %d bytes", len(raw))
		}
	})
}

func TestRestartPrunesExecutionChangesWithoutResettingRevision(t *testing.T) {
	f := newOrchestrationFixture(t)
	f.do(func(tx *Tx) error {
		for _, id := range []string{"first", "second"} {
			putQueryExecution(t, tx, Execution{ID: id, Status: "failed"}, 0, "")
		}
		before, err := tx.ProjectExecutionChanges("p", 0, 0, 0, 0, 100)
		if err != nil {
			return err
		}
		generation := int64(0)
		if _, err := tx.RestartProject("p", &generation); err != nil {
			return err
		}
		var changes int
		if err := tx.QueryRow("SELECT count(*) FROM xloom_ui_execution_changes WHERE project_id='p'").Scan(&changes); err != nil {
			return err
		}
		if changes != 0 {
			t.Fatalf("restart retained %d obsolete execution changes", changes)
		}
		_, err = tx.ProjectExecutionChanges("p", 0, before.Through, 0, 0, 100)
		requireAPIStatus(t, err, 409)
		fresh, err := tx.ProjectExecutionChanges("p", 1, 0, 0, 0, 100)
		if err != nil || len(fresh.Items)+len(fresh.Deleted) != 0 || fresh.Through <= before.Through {
			t.Fatalf("restart lost the monotonic revision: %+v %v", fresh, err)
		}
		putQueryExecution(t, tx, Execution{ID: "new", Status: "running"}, 1, "")
		next, err := tx.ProjectExecutionChanges("p", 1, fresh.Through, 0, 0, 100)
		if err != nil || len(next.Items) != 1 || next.Items[0].ID != "new" || next.Through <= fresh.Through {
			t.Fatalf("new-round execution fell behind the old cursor: %+v %v", next, err)
		}
		return nil
	})
}
