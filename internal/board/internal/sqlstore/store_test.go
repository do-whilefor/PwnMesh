package sqlstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
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

var activeCancellationID atomic.Uint64

func TestActiveStatementsHonorTransactionCancellation(t *testing.T) {
	for _, view := range []bool{false, true} {
		for _, method := range []string{"exec", "query", "query_row"} {
			t.Run(fmt.Sprintf("view=%v/%s", view, method), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var entered, completed atomic.Bool
				name := fmt.Sprintf("pwnmesh_cancel_active_%d", activeCancellationID.Add(1))
				// Cancel from inside SQLite, after statement execution has begun.
				// The final call detects completed work even if Rows later reports
				// the cancelled transaction. No timing threshold or sleep is needed.
				if err := sqlite.RegisterScalarFunction(name, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
					if args[0].(int64) == 0 {
						entered.Store(true)
						cancel()
						return int64(1), nil
					}
					completed.Store(true)
					return args[0], nil
				}); err != nil {
					t.Fatal(err)
				}
				store, err := Open(filepath.Join(t.TempDir(), "cancel.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				transaction := store.Do
				if view {
					transaction = store.View
				}
				query := fmt.Sprintf("WITH RECURSIVE work(n) AS (SELECT %s(0) UNION ALL SELECT n+1 FROM work WHERE n<1000000) SELECT %s(sum(n)) FROM work", name, name)
				err = transaction(ctx, func(q Queries) error {
					switch method {
					case "exec":
						if !view {
							query = "UPDATE settings SET intent_timeout=(" + query + ") WHERE rowid=1"
						}
						_, err := q.Exec(query)
						return err
					case "query":
						rows, err := q.Query(query)
						if err != nil {
							return err
						}
						defer rows.Close()
						for rows.Next() {
						}
						return rows.Err()
					default:
						var value int64
						return q.QueryRow(query).Scan(&value)
					}
				})
				if !entered.Load() || completed.Load() || !errors.Is(err, context.Canceled) {
					t.Errorf("active statement was not interrupted: entered=%v completed=%v error=%v", entered.Load(), completed.Load(), err)
				}
				followup, stop := context.WithTimeout(context.Background(), 2*time.Second)
				defer stop()
				if err := store.Do(followup, func(q Queries) error {
					var timeout int
					if err := q.QueryRow("SELECT intent_timeout FROM settings WHERE rowid=1").Scan(&timeout); err != nil {
						return err
					}
					if timeout != 15 {
						t.Fatalf("cancelled transaction retained its write: %d", timeout)
					}
					_, err := q.Exec("UPDATE settings SET intent_timeout=16 WHERE rowid=1")
					return err
				}); err != nil {
					t.Fatalf("cancelled transaction retained its connection or writer: %v", err)
				}
			})
		}
	}
}
