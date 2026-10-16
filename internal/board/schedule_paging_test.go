package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board/internal/sqlstore"
)

type boundedScheduleQueries struct{ transactionQueries }

func (q boundedScheduleQueries) Query(query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "FROM facts ") || strings.Contains(query, "FROM intents ") || strings.Contains(query, "FROM hints ") {
		return nil, errors.New("scheduling page reloaded the full graph")
	}
	return q.transactionQueries.Query(query, args...)
}

func TestScheduleCacheTracksLeasesMetadataAndRollback(t *testing.T) {
	store := executionQueryStore(t)
	executionQueryTx(t, store, func(tx *Tx) {
		for _, id := range []string{"p", "other"} {
			if err := tx.Save(Graph{Project: Project{ID: id, Title: id, Status: "active", CreatedAt: tx.Now}, Intents: []Intent{{ID: "work", Description: "work", CreatedAt: tx.Now}}}); err != nil {
				t.Fatal(err)
			}
			if err := sqlstore.SaveStateMetadata(tx.transactionQueries, id, `{"steps":[{"id":"work","write_paths":["/workspace/out"]}]}`, 0, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ScheduleInput(id, 0, ""); err != nil {
				t.Fatal(err)
			}
		}
		first, err := tx.ScheduleInput("p", 0, "")
		if err != nil || !first.Steps[0].Ready || !reflect.DeepEqual(first.Steps[0].WritePaths, []string{"/workspace/out"}) {
			t.Fatalf("cached admission fields changed: %+v, %v", first, err)
		}
		if _, err := tx.transactionQueries.Exec("UPDATE intents SET worker='run',last_heartbeat_at=? WHERE project_id='p'", tx.Now); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := tx.transactionQueries.QueryRow("SELECT COUNT(*) FROM pwnmesh_schedule_cache WHERE project_id='other'").Scan(&count); err != nil || count != 1 {
			t.Fatalf("one project invalidated another: count=%d, %v", count, err)
		}
		running, err := tx.ScheduleInput("p", 0, first.StateVersion)
		if err != nil || running.Steps[0].Ready || !running.Steps[0].Running || running.StateVersion != first.StateVersion {
			t.Fatalf("lease changed decision identity or left stale readiness: %+v, %v", running, err)
		}
		if _, err := tx.transactionQueries.Exec("SAVEPOINT schedule_test"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.transactionQueries.Exec("UPDATE projects SET title='temporary' WHERE id='p'"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.transactionQueries.Exec("ROLLBACK TO schedule_test; RELEASE schedule_test"); err != nil {
			t.Fatal(err)
		}
		tx.transactionQueries = boundedScheduleQueries{tx.transactionQueries}
		after, err := tx.ScheduleInput("p", 0, first.StateVersion)
		if err != nil || !reflect.DeepEqual(after, running) {
			t.Fatalf("rollback did not restore the cached projection: %+v, %v", after, err)
		}
	})
	executionQueryTx(t, store, func(tx *Tx) {
		before, err := tx.ScheduleInput("p", 0, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := sqlstore.UpdateStateMetadata(tx.transactionQueries, "p", `{"steps":[{"id":"work","priority":7,"write_paths":["/workspace/new"]}]}`); err != nil {
			t.Fatal(err)
		}
		_, err = tx.ScheduleInput("p", 0, before.StateVersion)
		var api *APIError
		if !errors.As(err, &api) || api.Status != 409 {
			t.Fatalf("changed metadata accepted a stale page: %v", err)
		}
		after, err := tx.ScheduleInput("p", 0, "")
		if err != nil || after.Steps[0].Priority != 7 || !reflect.DeepEqual(after.Steps[0].WritePaths, []string{"/workspace/new"}) {
			t.Fatalf("metadata update left stale scheduling fields: %+v, %v", after, err)
		}
		if _, err := tx.transactionQueries.Exec("DELETE FROM projects WHERE id='p'"); err != nil {
			t.Fatal(err)
		}
		_, err = tx.ScheduleInput("p", 0, "")
		if !errors.As(err, &api) || api.Status != 404 {
			t.Fatalf("deleted project retained scheduling data: %v", err)
		}
	})
}

func TestSchedulePagesReadStoredProjection(t *testing.T) {
	store := executionQueryStore(t)
	var version string
	executionQueryTx(t, store, func(tx *Tx) {
		g := Graph{Project: Project{ID: "p", Title: "Paging", Status: "active", CreatedAt: tx.Now}}
		for n := range 205 {
			g.Intents = append(g.Intents, Intent{ID: fmt.Sprintf("i%03d", n), CreatedAt: "now", Creator: "test", Description: "work", From: []string{}})
		}
		if err := tx.Save(g); err != nil {
			t.Fatal(err)
		}
		page, err := tx.ScheduleInput("p", 0, "")
		if err != nil || len(page.Steps) != 100 || page.NextOffset != 100 {
			t.Fatalf("first page: %+v, %v", page, err)
		}
		version = page.StateVersion
	})
	// A later transaction must obtain only the requested projection rows.
	if err := store.View(context.Background(), func(tx *Tx) error {
		tx.transactionQueries = boundedScheduleQueries{tx.transactionQueries}
		for _, offset := range []int{0, 100, 200, 205} {
			page, err := tx.ScheduleInput("p", offset, version)
			if err != nil {
				return err
			}
			want := min(100, 205-offset)
			if len(page.Steps) != want || page.Steps == nil {
				t.Fatalf("offset %d returned %d steps, want %d", offset, len(page.Steps), want)
			}
			if want > 0 && page.Steps[0].ID != fmt.Sprintf("i%03d", offset) {
				t.Fatalf("lost persisted ordering: %+v", page.Steps[0])
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
