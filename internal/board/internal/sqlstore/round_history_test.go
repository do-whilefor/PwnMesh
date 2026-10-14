package sqlstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"
)

func roundHistoryStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Do(context.Background(), func(q Queries) error {
		_, err := q.Exec("INSERT INTO projects(id,title,created_at) VALUES('p','fixture','now'),('other','fixture','now')")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestRoundArchivePreservesBytesOrderingAndScope(t *testing.T) {
	store := roundHistoryStore(t)
	raw := []byte("{ legacy: \xf0\x9f\x99\x82\x00\xff\r\n retained }")
	sum := sha256.Sum256(raw)
	wantHash := hex.EncodeToString(sum[:])
	if err := store.Do(context.Background(), func(q Queries) error {
		for _, generation := range []int64{7, 1, 3} {
			if err := InsertRound(q, "p", generation, "archive time"); err != nil {
				return err
			}
		}
		if err := InsertRound(q, "other", 1, "other time"); err != nil {
			return err
		}
		for _, entry := range []struct {
			project    string
			generation int64
			id         string
			data       []byte
		}{
			{"p", 1, "z-first", raw},
			{"other", 1, "foreign-project", []byte("other project")},
			{"p", 3, "foreign-generation", []byte("other generation")},
			{"p", 1, "a-second", []byte{}},
			{"p", 1, "m-third", []byte("last")},
		} {
			if err := InsertRoundEntry(q, entry.project, entry.generation, "event", entry.id, entry.data); err != nil {
				return err
			}
		}
		if err := InsertRound(q, "p", 1, "replacement"); err == nil {
			t.Fatal("duplicate round replaced archive metadata")
		}
		if err := InsertRoundEntry(q, "p", 1, "event", "z-first", []byte("replacement")); err == nil {
			t.Fatal("duplicate entry replaced archived bytes")
		}
		for _, scope := range []struct {
			project    string
			generation int64
			want       bool
		}{{"p", 1, true}, {"p", 2, false}, {"other", 3, false}, {"missing", 1, false}} {
			if exists, err := RoundExists(q, scope.project, scope.generation); err != nil || exists != scope.want {
				t.Fatalf("round existence escaped scope: %v, %v", exists, err)
			}
		}
		for _, page := range []struct {
			after int64
			want  []int64
		}{{0, []int64{1, 3}}, {3, []int64{7}}, {7, nil}} {
			var generations []int64
			if err := VisitRoundHistory(q, "p", page.after, 2, func(generation int64, archivedAt string) error {
				generations = append(generations, generation)
				if archivedAt != "archive time" {
					t.Fatalf("round timestamp changed: %q", archivedAt)
				}
				return nil
			}); err != nil {
				return err
			}
			if !reflect.DeepEqual(generations, page.want) {
				t.Fatalf("history escaped order, cursor, or limit: %v", generations)
			}
		}
		var cursor, first int64
		var ids []string
		if err := VisitRoundEntries(q, "p", 1, 0, 2, func(rowID int64, kind, id string, size int64, checksum string) error {
			if rowID <= cursor || kind != "event" {
				t.Fatalf("entry lost insertion order or kind: %d, %q", rowID, kind)
			}
			if len(ids) == 0 {
				first = rowID
				if size != int64(len(raw)) || checksum != wantHash {
					t.Fatalf("entry byte metadata changed: %d, %s", size, checksum)
				}
			} else if size != 0 {
				t.Fatalf("empty BLOB has nonzero length: %d", size)
			}
			cursor = rowID
			ids = append(ids, id)
			return nil
		}); err != nil {
			return err
		}
		if !reflect.DeepEqual(ids, []string{"z-first", "a-second"}) {
			t.Fatalf("entries escaped scope, insertion order, or limit: %v", ids)
		}
		ids = nil
		if err := VisitRoundEntries(q, "p", 1, cursor, 2, func(_ int64, _, id string, _ int64, _ string) error {
			ids = append(ids, id)
			return nil
		}); err != nil {
			return err
		}
		if !reflect.DeepEqual(ids, []string{"m-third"}) {
			t.Fatalf("entry cursor repeated or omitted an entry: %v", ids)
		}
		var restored []byte
		for offset := int64(0); offset <= int64(len(raw)); offset += 2 {
			data, total, checksum, err := ReadRoundEntry(q, "p", 1, first, offset, 2)
			if err != nil {
				return err
			}
			if len(data) > 2 || total != int64(len(raw)) || checksum != wantHash {
				t.Fatalf("chunk changed byte limit or metadata: %d, %d, %s", len(data), total, checksum)
			}
			restored = append(restored, data...)
		}
		if !bytes.Equal(restored, raw) {
			t.Fatalf("BLOB read changed bytes: %q", restored)
		}
		for _, scope := range []struct {
			project    string
			generation int64
		}{{"other", 1}, {"p", 3}} {
			if _, _, _, err := ReadRoundEntry(q, scope.project, scope.generation, first, 0, 2); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("entry read escaped project or generation: %v", err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRoundArchiveVisitorFailureRollsBackCallerWrites(t *testing.T) {
	store := roundHistoryStore(t)
	stop := errors.New("archive visitor stopped")
	for _, entries := range []bool{false, true} {
		calls := 0
		err := store.Do(context.Background(), func(q Queries) error {
			for generation := int64(0); generation < 2; generation++ {
				if err := InsertRound(q, "p", generation, "now"); err != nil {
					return err
				}
			}
			for _, id := range []string{"first", "second"} {
				if err := InsertRoundEntry(q, "p", 0, "state", id, []byte(id)); err != nil {
					return err
				}
			}
			if entries {
				return VisitRoundEntries(q, "p", 0, 0, 2, func(int64, string, string, int64, string) error { calls++; return stop })
			}
			return VisitRoundHistory(q, "p", -1, 2, func(int64, string) error { calls++; return stop })
		})
		if !errors.Is(err, stop) || calls != 1 {
			t.Fatalf("visitor did not stop with original failure: calls=%d, err=%v", calls, err)
		}
		if err := store.View(context.Background(), func(q Queries) error {
			if exists, err := RoundExists(q, "p", 0); err != nil || exists {
				t.Fatalf("failed callback committed an archive: %v, %v", exists, err)
			}
			return VisitRoundEntries(q, "p", 0, 0, 2, func(int64, string, string, int64, string) error {
				t.Fatal("failed callback retained archived entries")
				return nil
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
}
