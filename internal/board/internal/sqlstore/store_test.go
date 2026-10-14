package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCallbacksRetainTransactionOwnership(t *testing.T) {
	for _, memory := range []bool{false, true} {
		name, path := "disk", filepath.Join(t.TempDir(), "store.db")
		if memory {
			name, path = "memory", ":memory:"
		}
		t.Run(name, func(t *testing.T) {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			wantErr := errors.New("callback failed")
			var expired Queries
			err = store.Do(context.Background(), func(q Queries) error {
				expired = q
				for _, method := range []string{"Commit", "Rollback", "Prepare", "PrepareContext", "Stmt", "StmtContext"} {
					if _, ok := reflect.TypeOf(q).MethodByName(method); ok {
						t.Errorf("callback can recover lifecycle method %s", method)
					}
				}
				if _, ok := q.(*sql.Tx); ok {
					t.Error("callback received the raw transaction")
				}
				if _, err := q.Exec("UPDATE settings SET intent_timeout=99 WHERE rowid=1"); err != nil {
					return err
				}
				return wantErr
			})
			if !errors.Is(err, wantErr) {
				t.Fatalf("callback error = %v, want %v", err, wantErr)
			}
			if _, err := expired.Exec("UPDATE settings SET intent_timeout=88 WHERE rowid=1"); !errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("callback query handle survived rollback: %v", err)
			}
			if err := store.View(context.Background(), func(q Queries) error {
				var timeout int
				if err := q.QueryRow("SELECT intent_timeout FROM settings WHERE rowid=1").Scan(&timeout); err != nil {
					return err
				}
				if timeout != 15 {
					t.Fatalf("failed callback persisted its write: %d", timeout)
				}
				if !memory {
					if _, err := q.Exec("UPDATE settings SET intent_timeout=77 WHERE rowid=1"); err == nil {
						t.Error("disk View accepted a write")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWaitingWriterHonorsContextCancellation(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- store.Do(context.Background(), func(Queries) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = store.Do(ctx, func(Queries) error {
		t.Error("waiting writer entered a callback before the first transaction ended")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting writer did not return its cancellation: %v", err)
	}
}
