package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
)

func actionReceiptStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(":memory:")
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

func TestActionReceiptsPreserveBytesScopeAndRollback(t *testing.T) {
	store := actionReceiptStore(t)
	ctx := context.Background()
	const key = "receipt:'?"
	const request = "legacy request\x00 \t\r\n"
	const response = "{\"number\":9007199254740993, \"text\":\"\\u0061\"}\n"
	if err := store.Do(ctx, func(q Queries) error {
		if err := InsertActionReceipt(q, "p", key, request, response); err != nil {
			return err
		}
		if err := InsertActionReceipt(q, "other", key, "other request", "other response"); err != nil {
			return err
		}
		if err := InsertActionReceipt(q, "p", key, "replacement", "replacement"); err == nil {
			t.Fatal("duplicate idempotency key replaced its original receipt")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("caller rejected receipt")
	if err := store.Do(ctx, func(q Queries) error {
		if err := InsertActionReceipt(q, "p", "rejected", request, response); err != nil {
			return err
		}
		if err := DeleteActionReceipts(q, "p"); err != nil {
			return err
		}
		return rejected
	}); !errors.Is(err, rejected) {
		t.Fatalf("caller error = %v", err)
	}
	if err := store.View(ctx, func(q Queries) error {
		gotRequest, gotResponse, err := ActionReceipt(q, "p", key)
		if err != nil || gotRequest != request || gotResponse != response {
			t.Fatalf("receipt bytes changed: %q, %q, %v", gotRequest, gotResponse, err)
		}
		gotRequest, gotResponse, err = ActionReceipt(q, "other", key)
		if err != nil || gotRequest != "other request" || gotResponse != "other response" {
			t.Fatalf("project scope changed: %q, %q, %v", gotRequest, gotResponse, err)
		}
		for _, scope := range [][2]string{{"p", "missing"}, {"p", "rejected"}, {"missing", key}} {
			if _, _, err := ActionReceipt(q, scope[0], scope[1]); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("missing receipt %v = %v", scope, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Do(ctx, func(q Queries) error { return DeleteActionReceipts(q, "p") }); err != nil {
		t.Fatal(err)
	}
	if err := store.View(ctx, func(q Queries) error {
		if _, _, err := ActionReceipt(q, "p", key); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("deleted receipt = %v", err)
		}
		_, _, err := ActionReceipt(q, "other", key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCurationReceiptReadsKeepStoredRunAndOperationScope(t *testing.T) {
	store := actionReceiptStore(t)
	if err := store.Do(context.Background(), func(q Queries) error {
		for _, row := range [][3]string{
			{"p", "fact", `{"Run":"run","Op":"fact"}`},
			{"p", "different-run", `{"Run":"different","Op":"curate"}`},
			{"p", "lowercase", `{"run":"run","op":"curate"}`},
			{"other", "other-project", `{"Run":"run","Op":"curate"}`},
		} {
			if err := InsertActionReceipt(q, row[0], row[1], row[2], row[1]); err != nil {
				return err
			}
		}
		if exists, err := HasCurationReceipt(q, "p", "run"); err != nil || exists {
			t.Fatalf("unrelated receipt matched curation: %v, %v", exists, err)
		}
		if _, err := CurationResponse(q, "p", "run"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("missing curation response = %v", err)
		}
		const response = " historical response \x00\r\n"
		if err := InsertActionReceipt(q, "p", "opaque-key", ` {"Op":"curate", "Run":"run", "Payload":{}} `, response); err != nil {
			return err
		}
		if exists, err := HasCurationReceipt(q, "p", "run"); err != nil || !exists {
			t.Fatalf("stored curation not found: %v, %v", exists, err)
		}
		if got, err := CurationResponse(q, "p", "run"); err != nil || got != response {
			t.Fatalf("curation response bytes changed: %q, %v", got, err)
		}
		if got, err := CurationResponse(q, "other", "run"); err != nil || got != "other-project" {
			t.Fatalf("curation crossed project scope: %q, %v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Do(context.Background(), func(q Queries) error {
		if err := DeleteActionReceipts(q, "p"); err != nil {
			return err
		}
		if err := InsertActionReceipt(q, "p", "legacy", "not JSON", "legacy response"); err != nil {
			return err
		}
		if _, err := HasCurationReceipt(q, "p", "run"); err == nil {
			t.Fatal("malformed stored JSON silently treated as a missing curation")
		}
		if _, err := CurationResponse(q, "p", "run"); err == nil || errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("malformed stored JSON lost its read error: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestVisitActionReceiptsKeepsOrderBytesAndCallerTransaction(t *testing.T) {
	store := actionReceiptStore(t)
	ctx := context.Background()
	want := [][3]string{{"z-first", " legacy\x00request ", " legacy\r\nresponse "}, {"a-second", `{"Run":"run"}`, ` {"accepted":true} `}}
	if err := store.Do(ctx, func(q Queries) error {
		for _, row := range want {
			if err := InsertActionReceipt(q, "p", row[0], row[1], row[2]); err != nil {
				return err
			}
		}
		return InsertActionReceipt(q, "other", "foreign", "request", "response")
	}); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("archive rejected")
	var got [][3]string
	if err := store.Do(ctx, func(q Queries) error {
		return VisitActionReceipts(q, "p", func(key, request, response string) error {
			got = append(got, [3]string{key, request, response})
			if _, err := q.Exec("UPDATE projects SET title=? WHERE id='p'", key); err != nil {
				return err
			}
			if len(got) == 2 {
				return rejected
			}
			return nil
		})
	}); !errors.Is(err, rejected) {
		t.Fatalf("visitor error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("receipt visitation changed order, bytes or project: got %q, want %q", got, want)
	}
	if err := store.View(ctx, func(q Queries) error {
		var title string
		if err := q.QueryRow("SELECT title FROM projects WHERE id='p'").Scan(&title); err != nil {
			return err
		}
		if title != "fixture" {
			t.Fatalf("visitor writes survived caller rollback: %q", title)
		}
		return VisitActionReceipts(q, "missing", func(string, string, string) error {
			t.Fatal("missing project yielded a receipt")
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}
