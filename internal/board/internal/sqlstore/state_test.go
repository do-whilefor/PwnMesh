package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestStateRecordsPreserveMetadataRevisionsAndCallerRollback(t *testing.T) {
	store := actionReceiptStore(t)
	ctx := context.Background()
	const retained = "{ \"unknown\":9007199254740993, \"text\":\"\\u0061\" }\n"
	if err := store.Do(ctx, func(q Queries) error {
		if err := SaveStateMetadata(q, "p", "original", 4, 2); err != nil {
			return err
		}
		if err := SaveStateMetadata(q, "p", "replacement", 7, 5); err != nil {
			return err
		}
		if err := UpdateStateMetadata(q, "p", retained); err != nil {
			return err
		}
		if err := UpdateStateMetadata(q, "other", "must not insert"); err != nil {
			return err
		}
		if _, _, _, err := StateMetadata(q, "other"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("update created an absent state: %v", err)
		}
		if revision, err := AdvanceStateRevision(q, "other", "initial bytes", 1); err != nil || revision != 1 {
			t.Fatalf("initial revision = %d, %v", revision, err)
		}
		if _, err := q.Exec(`CREATE TRIGGER guard_event_metadata BEFORE UPDATE OF data ON pwnmesh_state BEGIN SELECT RAISE(ABORT,'event changed metadata'); END`); err != nil {
			return err
		}
		for n, increment := range []int{0, 1} {
			revision, err := AdvanceStateRevision(q, "p", "must not overwrite", increment)
			if err != nil || revision != int64(8+n) {
				t.Fatalf("event revision = %d, %v", revision, err)
			}
		}
		for _, project := range []string{"p", "other"} {
			if err := InsertStateEvent(q, project, 9, "raw event bytes"); err != nil {
				return err
			}
			if err := InsertActionReceipt(q, project, "key", "request", "response"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("caller rejected reset")
	if err := store.Do(ctx, func(q Queries) error {
		if err := DeleteStateRecords(q, "p"); err != nil {
			return err
		}
		return rejected
	}); !errors.Is(err, rejected) {
		t.Fatalf("caller rollback = %v", err)
	}
	check := func(deleted bool) {
		t.Helper()
		if err := store.View(ctx, func(q Queries) error {
			for _, project := range []string{"p", "other"} {
				raw, revision, decision, err := StateMetadata(q, project)
				if deleted && project == "p" {
					if !errors.Is(err, sql.ErrNoRows) {
						t.Fatalf("reset retained metadata: %v", err)
					}
				} else {
					want, wantRevision, wantDecision := retained, int64(9), int64(6)
					if project == "other" {
						want, wantRevision, wantDecision = "initial bytes", 1, 1
					}
					if err != nil || string(raw) != want || revision != wantRevision || decision != wantDecision {
						t.Fatalf("metadata/revisions changed for %s: %q %d/%d %v", project, raw, revision, decision, err)
					}
				}
				count := 0
				if err := VisitStateEvents(q, project, nil, 0, func(_ int64, raw []byte) error { count++; return nil }); err != nil {
					return err
				}
				_, _, receiptErr := ActionReceipt(q, project, "key")
				if deleted && project == "p" {
					if count != 0 || !errors.Is(receiptErr, sql.ErrNoRows) {
						t.Fatal("reset retained events or receipts")
					}
				} else if count != 1 || receiptErr != nil {
					t.Fatalf("reset escaped project scope or caller rollback: %d, %v", count, receiptErr)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	check(false)
	if err := store.Do(ctx, func(q Queries) error { return DeleteStateRecords(q, "p") }); err != nil {
		t.Fatal(err)
	}
	check(true)
}
