package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestFailureDiagnosticsKeepLatestLeaseReasons(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Do(context.Background(), func(q Queries) error {
		if _, err := q.Exec("INSERT INTO projects(id,title,status,created_at) VALUES('p','Fixture','active','now'),('other','Other','active','now')"); err != nil {
			return err
		}
		for n, event := range []struct{ project, raw string }{
			{"p", `{"op":"execution_failed","run_id":"empty","payload":{"reason":"older reason"}}`},
			{"p", `{"op":"execution_failed","run_id":"empty","payload":{"reason":""}}`},
			{"p", `{"op":"execution_failed","run_id":"latest","payload":{"reason":null}}`},
			{"p", `{"op":"execution_failed","run_id":"latest","payload":{"reason":"latest reason"}}`},
			{"p", `{"op":"execution_failed","run_id":"unrequested","payload":{"reason":"unrelated lease"}}`},
			{"p", `{"op":"fact","run_id":"latest","payload":{"reason":"unrelated operation"}}`},
			{"other", `{"op":"execution_failed","run_id":"latest","payload":{"reason":"foreign project"}}`},
		} {
			if _, err := q.Exec("INSERT INTO pwnmesh_state_events(project_id,revision,event) VALUES(?,?,?)", event.project, n+1, event.raw); err != nil {
				return err
			}
		}
		// A missing lease keeps the scan open past both old events. Duplicate
		// requests do not change cardinality, and old null reasons stay hidden.
		reasons, err := ExecutionFailureReasons(q, "p", []string{"empty", "latest", "missing", "empty"})
		if err != nil || !reflect.DeepEqual(reasons, map[string]string{"empty": "", "latest": "latest reason"}) {
			t.Fatalf("diagnostics lost latest-event precedence or project/lease scope: %v, %v", reasons, err)
		}
		if _, err := q.Exec(`INSERT INTO pwnmesh_state_events(project_id,revision,event) VALUES('p',8,'{"op":"execution_failed","run_id":"latest","payload":{"reason":null}}')`); err != nil {
			return err
		}
		if _, err := ExecutionFailureReasons(q, "p", []string{"latest"}); err == nil {
			t.Fatal("invalid latest diagnostic was silently replaced by older history")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectionReadsKeepSnapshotScopeAndRawMetadata(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Do(ctx, func(q Queries) error {
		for _, project := range []string{"p", "other"} {
			if err := SaveGraph(q, model.Graph{Project: model.Project{ID: project, Title: project, Status: "active", CreatedAt: "now"}, Intents: []model.Intent{{ID: "step", Description: "Work", Creator: "planner", CreatedAt: "now"}}}); err != nil {
				return err
			}
		}
		if raw, _, _, err := StateMetadata(q, "p"); !errors.Is(err, sql.ErrNoRows) || raw != nil {
			t.Fatalf("missing metadata = %q, %v", raw, err)
		}
		if _, err := q.Exec(`INSERT INTO pwnmesh_state(project_id,data,revision,decision_revision) VALUES('p','{"unknown":7}',4,2)`); err != nil {
			return err
		}
		for _, run := range []model.ExecutionSummary{
			{ProjectID: "p", ID: "current", Intent: "step", Kind: "explore", Generation: 2, Status: "succeeded"},
			{ProjectID: "p", ID: "latest", Intent: "step", Kind: "explore", Generation: 1, Status: "failed", Lease: "backend@latest"},
			{ProjectID: "p", ID: "planner", Intent: "step", Kind: "reason", Generation: 2},
			{ProjectID: "p", ID: "unlinked", Intent: "absent", Kind: "explore"},
			{ProjectID: "other", ID: "foreign", Intent: "step", Kind: "explore"},
		} {
			putExecutionSummary(t, q, run)
		}
		if _, err := q.Exec(`INSERT INTO pwnmesh_state_events(project_id,revision,event) VALUES('p',4,'{"op":"execution_failed","run_id":"backend@latest","payload":{"reason":"original diagnostic"}}')`); err != nil {
			return err
		}
		if _, err := q.Exec(`INSERT INTO pwnmesh_assets(project_id,id,kind,value,method) VALUES('p','a','url','first',''),('p','b','url','second',''),('other','c','url','foreign','')`); err != nil {
			return err
		}
		_, err := q.Exec(`INSERT INTO pwnmesh_asset_anchors(project_id,generation,node_kind,node_id,asset_id) VALUES('p',2,'fact','f','a'),('p',1,'fact','old','b'),('other',2,'fact','f','c')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.View(ctx, func(q Queries) error {
		raw, revision, decision, err := StateMetadata(q, "p")
		if err != nil || string(raw) != `{"unknown":7}` || revision != 4 || decision != 2 {
			t.Fatalf("stored metadata was interpreted or changed: %q %d/%d %v", raw, revision, decision, err)
		}
		if err := store.Do(ctx, func(writer Queries) error {
			if _, err := writer.Exec(`UPDATE pwnmesh_state SET data='',revision=5 WHERE project_id='p'`); err != nil {
				return err
			}
			putExecutionSummary(t, writer, model.ExecutionSummary{ProjectID: "p", ID: "after-snapshot", Intent: "step", Kind: "explore", Generation: 3})
			if _, err := writer.Exec(`DELETE FROM pwnmesh_state_events WHERE project_id='p'`); err != nil {
				return err
			}
			_, err := writer.Exec(`DELETE FROM pwnmesh_asset_anchors WHERE project_id='p' AND generation=2`)
			return err
		}); err != nil {
			return err
		}
		runs, err := LatestStepExecutions(q, "p")
		if err != nil || len(runs) != 1 || runs[0].ID != "latest" || runs[0].Generation != 1 || runs[0].Lease != "backend@latest" {
			t.Fatalf("latest attempt escaped snapshot or changed scope: %+v %v", runs, err)
		}
		reasons, err := ExecutionFailureReasons(q, "p", []string{"backend@latest"})
		if err != nil || reasons["backend@latest"] != "original diagnostic" {
			t.Fatalf("failure diagnostic escaped snapshot: %v %v", reasons, err)
		}
		assets, anchors, err := ProjectAssets(q, "p", 2)
		if err != nil || len(assets) != 1 || assets[0].ID != "a" || len(anchors) != 1 || anchors[0].NodeID != "f" {
			t.Fatalf("assets escaped snapshot or generation/project scope: %+v %+v %v", assets, anchors, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.View(ctx, func(q Queries) error {
		raw, revision, _, err := StateMetadata(q, "p")
		if err == nil || !strings.Contains(err.Error(), "invalid current state metadata") || raw == nil || len(raw) != 0 || revision != 5 {
			t.Fatalf("damaged current metadata was accepted or treated as absent: %v %d %v", raw, revision, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
