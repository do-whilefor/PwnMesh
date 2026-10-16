package server

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"pwnmesh/internal/board"
)

func TestProjectListSummariesHideExpiredLeasesWithoutWritingOrLoadingGraph(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	old := f.store.Now().Add(-time.Minute).Format(time.RFC3339)
	var want board.Summary
	if err := f.store.Do(context.Background(), func(tx *board.Tx) error {
		g, err := tx.Load(f.project)
		if err != nil {
			return err
		}
		g.Project.Reason = &board.Reason{Worker: "expired-planner", Trigger: "initial", StartedAt: old, Heartbeat: old}
		g.Intents = []board.Intent{
			{ID: "expired", From: []string{"origin"}, Description: "Expired work", Worker: board.Ptr("expired-worker"), Heartbeat: &old, CreatedAt: old},
			{ID: "working", From: []string{"origin"}, Description: "Current work", Worker: board.Ptr("current-worker"), Heartbeat: &tx.Now, CreatedAt: tx.Now},
			{ID: "completed", From: []string{"origin"}, Description: "Retained worker", To: board.Ptr("origin"), Worker: board.Ptr("completed-worker"), Heartbeat: &old, ConcludedAt: &old, CreatedAt: old},
		}
		if err := tx.Save(g); err != nil {
			return err
		}
		want = board.Summary{Project: g.Project, FactCount: len(g.Facts), IntentCount: 3, Working: 1, Unclaimed: 1, HintCount: len(g.Hints)}
		want.Project.Reason = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The list must not depend on graph edges or hydrate a full graph merely
	// to count its nodes. A full Load would fail after this table is removed.
	if err := fixtureSQL(f.store).Do(context.Background(), func(tx *fixtureSQLTx) error {
		_, err := tx.Exec("DROP TABLE intent_sources")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var retained []board.Summary
	if err := f.store.View(context.Background(), func(tx *board.Tx) error {
		var err error
		retained, err = tx.ProjectSummaries()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var got []board.Summary
	f.request("GET", "/projects", nil, false, http.StatusOK, &got)
	if !reflect.DeepEqual(got, []board.Summary{want}) {
		t.Fatalf("list changed its public projection: got=%+v want=%+v", got, want)
	}
	if err := f.store.View(context.Background(), func(tx *board.Tx) error {
		after, err := tx.ProjectSummaries()
		if err == nil && !reflect.DeepEqual(after, retained) {
			t.Fatal("observational project list expired persisted leases")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
