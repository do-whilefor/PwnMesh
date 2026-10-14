package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"pwnmesh/internal/board/model"
)

func stateEventStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Do(context.Background(), func(q Queries) error {
		for _, project := range []string{"p", "other"} {
			if _, err := q.Exec("INSERT INTO projects(id,title,created_at) VALUES(?,'fixture','now')", project); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestStateEventVisitsPreserveRawHistoryBoundsAndRollback(t *testing.T) {
	store := stateEventStore(t, ":memory:")
	ctx := context.Background()
	rawEvent := func(revision int64) string {
		if revision == 1003 {
			return "legacy event\x00\r\n retained bytes"
		}
		return fmt.Sprintf("{ \"revision\":-1, \"op\":\"future_operation\", \"id\":\"%d\", \"unknown\":\"\\u0061\" }\n", revision)
	}
	if err := store.Do(ctx, func(q Queries) error {
		for revision := int64(1003); revision > 0; revision-- {
			if err := InsertStateEvent(q, "p", revision, rawEvent(revision)); err != nil {
				return err
			}
		}
		if err := InsertStateEvent(q, "p", 1, "replacement"); err == nil {
			t.Fatal("duplicate revision replaced its retained event")
		}
		return InsertStateEvent(q, "other", 1, "foreign event")
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.View(ctx, func(q Queries) error {
		count := int64(0)
		if err := VisitStateEvents(q, "p", nil, 0, func(revision int64, raw []byte) error {
			count++
			if revision != count || string(raw) != rawEvent(revision) {
				t.Fatalf("archive changed SQL order or event bytes: %d, %q", revision, raw)
			}
			return nil
		}); err != nil {
			return err
		}
		if count != 1003 {
			t.Fatalf("archive was limited to %d events", count)
		}
		after := int64(1)
		count = 0
		if err := VisitStateEvents(q, "p", &after, 1000, func(revision int64, raw []byte) error {
			count++
			if revision != after+count || string(raw) != rawEvent(revision) {
				t.Fatalf("page changed cursor or bytes: %d, %q", revision, raw)
			}
			return nil
		}); err != nil {
			return err
		}
		if count != 1000 {
			t.Fatalf("discovery page size = %d", count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("archive visitor stopped")
	calls := 0
	if err := store.Do(ctx, func(q Queries) error {
		if err := InsertStateEvent(q, "p", 1004, "uncommitted"); err != nil {
			return err
		}
		return VisitStateEvents(q, "p", nil, 0, func(int64, []byte) error {
			calls++
			return stop
		})
	}); !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("visitor did not stop on its first error: %d, %v", calls, err)
	}
	if err := store.View(ctx, func(q Queries) error {
		after := int64(1003)
		return VisitStateEvents(q, "p", &after, 1000, func(int64, []byte) error {
			t.Fatal("visitor error committed the caller's event")
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEventProjectionsPreserveSQLValuesAndCurationFilter(t *testing.T) {
	store := stateEventStore(t, ":memory:")
	if err := store.Do(context.Background(), func(q Queries) error {
		for n, raw := range []string{
			`{"revision":99,"op":7,"id":true}`,
			`{"op":"step_completed","id":"before","run_id":"old"}`,
			`{"op":"step_completed","id":"step","run_id":"worker","result":null}`,
			`{"op":"fact_relation","id":"relation","run_id":"author","result":"legacy relation result"}`,
			`{"op":"curate","id":"batch","run_id":"curator","payload":{"relations":[{}]},"result":false}`,
			`{"op":"curate","id":"empty","run_id":"curator","payload":{"relations":[]}}`,
			`{"op":"fact","id":"observation","run_id":"worker"}`,
			`{"op":"curate","id":"no-relations","run_id":"curator"}`,
		} {
			if err := InsertStateEvent(q, "p", int64(n+1), raw); err != nil {
				return err
			}
		}
		if err := InsertStateEvent(q, "other", 3, `{"op":"step_completed","id":"foreign","run_id":"foreign"}`); err != nil {
			return err
		}
		changes, err := StateChanges(q, "p", 0, 1, 1000)
		if err != nil || !reflect.DeepEqual(changes, []model.StateChange{{Revision: 1, Op: "7", ID: "1"}}) {
			t.Fatalf("SQL scalar coercion or revision source changed: %+v, %v", changes, err)
		}
		changes, err = StateChanges(q, "p", 1, 5, 2)
		if err != nil || !reflect.DeepEqual(changes, []model.StateChange{{Revision: 2, Op: "step_completed", ID: "before"}, {Revision: 3, Op: "step_completed", ID: "step"}}) {
			t.Fatalf("change projection escaped cursor or limit: %+v, %v", changes, err)
		}
		changes, err = StateChanges(q, "p", 8, 8, 1000)
		if err != nil || changes == nil || len(changes) != 0 {
			t.Fatalf("empty change projection changed shape: %#v, %v", changes, err)
		}
		got := map[string]CurationEvent{}
		count := 0
		if err := VisitCurationEvents(q, "p", 2, func(event CurationEvent) error {
			count++
			got[event.ID] = event
			return nil
		}); err != nil {
			return err
		}
		want := map[string]CurationEvent{
			"step":     {Op: "step_completed", ID: "step", RunID: "worker", Result: "{}"},
			"relation": {Op: "fact_relation", ID: "relation", RunID: "author", Result: "legacy relation result"},
			"batch":    {Op: "curate", ID: "batch", RunID: "curator", Result: "{}"},
		}
		if count != len(want) || !reflect.DeepEqual(got, want) {
			t.Fatalf("curation filter interpreted unrelated results or changed scope: %+v", got)
		}
		if err := InsertStateEvent(q, "p", 9, `{"op":null,"id":"null-operation"}`); err != nil {
			return err
		}
		if _, err := StateChanges(q, "p", 8, 9, 1000); err == nil {
			t.Fatal("NULL operation became an empty change field")
		}
		if err := InsertStateEvent(q, "p", 10, `{"op":"fact_relation","id":"null-result","run_id":"author","result":null}`); err != nil {
			return err
		}
		if err := VisitCurationEvents(q, "p", 9, func(CurationEvent) error { return nil }); err == nil {
			t.Fatal("NULL relation result became an empty projection field")
		}
		stop := errors.New("curation visitor stopped")
		if err := VisitCurationEvents(q, "p", 2, func(CurationEvent) error { return stop }); !errors.Is(err, stop) {
			t.Fatalf("later malformed result replaced the first visitor error: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEventReadsRemainInCallerWALSnapshot(t *testing.T) {
	store := stateEventStore(t, filepath.Join(t.TempDir(), "events.db"))
	ctx := context.Background()
	const first = `{"op":"step_completed","id":"original","run_id":"worker","result":null}`
	const next = `{"op":"curate","id":"next","run_id":"curator","payload":{"relations":[{}]}}`
	if err := store.Do(ctx, func(q Queries) error { return InsertStateEvent(q, "p", 1, first) }); err != nil {
		t.Fatal(err)
	}
	check := func(q Queries, revision int64, raw, op, id, run string) error {
		t.Helper()
		count := 0
		if err := VisitStateEvents(q, "p", nil, 0, func(gotRevision int64, got []byte) error {
			count++
			if gotRevision != revision || string(got) != raw {
				t.Fatalf("event escaped caller snapshot: %d, %q", gotRevision, got)
			}
			return nil
		}); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("snapshot event count = %d", count)
		}
		changes, err := StateChanges(q, "p", 0, 10, 1000)
		if err != nil || !reflect.DeepEqual(changes, []model.StateChange{{Revision: revision, Op: op, ID: id}}) {
			t.Fatalf("change projection escaped caller snapshot: %+v, %v", changes, err)
		}
		count = 0
		if err := VisitCurationEvents(q, "p", 0, func(event CurationEvent) error {
			count++
			if event != (CurationEvent{Op: op, ID: id, RunID: run, Result: "{}"}) {
				t.Fatalf("curation projection escaped caller snapshot: %+v", event)
			}
			return nil
		}); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("snapshot curation count = %d", count)
		}
		return nil
	}
	if err := store.View(ctx, func(q Queries) error {
		if err := check(q, 1, first, "step_completed", "original", "worker"); err != nil {
			return err
		}
		if err := store.Do(ctx, func(writer Queries) error {
			if _, err := writer.Exec("DELETE FROM pwnmesh_state_events WHERE project_id='p'"); err != nil {
				return err
			}
			return InsertStateEvent(writer, "p", 2, next)
		}); err != nil {
			return err
		}
		if err := InsertStateEvent(q, "p", 3, next); err == nil {
			t.Fatal("event insert bypassed read-only query ownership")
		}
		return check(q, 1, first, "step_completed", "original", "worker")
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.View(ctx, func(q Queries) error { return check(q, 2, next, "curate", "next", "curator") }); err != nil {
		t.Fatal(err)
	}
}
