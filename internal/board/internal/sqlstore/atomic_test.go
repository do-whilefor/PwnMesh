package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestAtomicNestedScopesPreserveCallerWrites(t *testing.T) {
	for _, outcome := range []string{"commit", "error", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			store, err := Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			failure := errors.New("operation failed")
			err = store.Do(context.Background(), func(q Queries) error {
				if _, err := q.Exec("UPDATE settings SET reason_timeout=77"); err != nil {
					return err
				}
				var opErr error
				var panicked any
				func() {
					defer func() { panicked = recover() }()
					opErr = Atomic(q, func() error {
						if _, err := q.Exec("UPDATE settings SET intent_timeout=55"); err != nil {
							return err
						}
						innerErr := Atomic(q, func() error {
							if _, err := q.Exec("UPDATE settings SET reason_timeout=88"); err != nil {
								return err
							}
							return failure
						})
						if !errors.Is(innerErr, failure) {
							t.Fatalf("inner error = %v", innerErr)
						}
						settings, err := ReadSettings(q)
						if err != nil || settings.IntentTimeout != 55 || settings.ReasonTimeout != 77 {
							t.Fatalf("inner rollback crossed its scope: %+v, %v", settings, err)
						}
						if err := Atomic(q, func() error {
							_, err := q.Exec("UPDATE settings SET reason_timeout=99")
							return err
						}); err != nil {
							return err
						}
						switch outcome {
						case "error":
							return failure
						case "panic":
							panic(failure)
						}
						return nil
					})
				}()
				if outcome == "error" && !errors.Is(opErr, failure) || outcome != "error" && opErr != nil || outcome == "panic" && panicked != failure || outcome != "panic" && panicked != nil {
					t.Fatalf("outcome error=%v panic=%v", opErr, panicked)
				}
				return nil // The caller deliberately handles the operation failure.
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.View(context.Background(), func(q Queries) error {
				settings, err := ReadSettings(q)
				wantIntent, wantReason := 15, 77
				if outcome == "commit" {
					wantIntent, wantReason = 55, 99
				}
				if err == nil && (settings.IntentTimeout != wantIntent || settings.ReasonTimeout != wantReason) {
					t.Fatalf("nested operation changed caller commit: %+v", settings)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type operationFailureQueries struct {
	Queries
	panicAt    string
	releaseErr error
}

func (q *operationFailureQueries) Exec(query string, args ...any) (sql.Result, error) {
	if q.panicAt != "" && strings.HasPrefix(query, q.panicAt) {
		panic("operation panic")
	}
	if q.releaseErr != nil && strings.HasPrefix(query, "RELEASE ") {
		err := q.releaseErr
		q.releaseErr = nil
		return nil, err
	}
	return q.Queries.Exec(query, args...)
}

func TestComposedWritesRollbackHandledPanicAndReleaseFailure(t *testing.T) {
	for _, operation := range []struct {
		name, panicAt string
		write         func(Queries) error
	}{
		{"state", "INSERT INTO pwnmesh_fact_metadata", func(q Queries) error {
			return SaveStateMetadata(q, "p", `{"facts":[{"id":"f"}]}`, 1, 1)
		}},
		{"snapshot", "INSERT OR IGNORE INTO pwnmesh_input_snapshots", func(q Queries) error {
			return SaveInputSnapshot(q, "p", "snapshot", []byte(`{}`), bytes.Repeat([]byte("snapshot data"), snapshotChunkMax))
		}},
		{"schedule", "INSERT INTO pwnmesh_schedule_steps", func(q Queries) error {
			return SaveSchedule(q, "p", []byte(`{}`), []model.ScheduleStep{{ID: "step"}})
		}},
	} {
		for _, outcome := range []string{"panic", "release"} {
			t.Run(operation.name+"/"+outcome, func(t *testing.T) {
				store := actionReceiptStore(t)
				if err := store.Do(context.Background(), func(q Queries) error {
					if _, err := q.Exec("UPDATE settings SET reason_timeout=77"); err != nil {
						return err
					}
					failure := errors.New("release failed")
					injected := &operationFailureQueries{Queries: q}
					if outcome == "panic" {
						injected.panicAt = operation.panicAt
					} else {
						injected.releaseErr = failure
					}
					var panicked any
					var writeErr error
					func() {
						defer func() { panicked = recover() }()
						writeErr = operation.write(injected)
					}()
					if outcome == "panic" && panicked != "operation panic" || outcome == "release" && (!errors.Is(writeErr, failure) || panicked != nil) {
						t.Fatalf("injected failure was lost: %v, %v", writeErr, panicked)
					}
					return nil // The caller deliberately handles the failure.
				}); err != nil {
					t.Fatal(err)
				}
				if err := store.View(context.Background(), func(q Queries) error {
					for _, table := range []string{"pwnmesh_state", "pwnmesh_fact_metadata", "pwnmesh_snapshot_chunks", "pwnmesh_input_snapshots", "pwnmesh_schedule_cache", "pwnmesh_schedule_steps"} {
						var count int
						if err := q.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
							return err
						}
						if count != 0 {
							t.Fatalf("handled %s committed %d partial rows in %s", outcome, count, table)
						}
					}
					settings, err := ReadSettings(q)
					if err == nil && settings.ReasonTimeout != 77 {
						t.Fatal("operation rollback discarded caller writes")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

type atomicFailureQueries struct {
	Queries
	failures map[string]error
	calls    map[string]int
}

func (q atomicFailureQueries) Exec(query string, args ...any) (sql.Result, error) {
	q.calls[query]++
	if err := q.failures[query]; err != nil {
		delete(q.failures, query)
		return nil, err
	}
	return q.Queries.Exec(query, args...)
}

func TestAtomicReportsCleanupFailuresToTransactionOwner(t *testing.T) {
	operationErr := errors.New("write failed")
	rollbackErr := errors.New("rollback failed")
	releaseErr := errors.New("release failed")
	for _, cleanup := range []string{"rollback", "release", "both"} {
		t.Run(cleanup, func(t *testing.T) {
			store, err := Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			failures := map[string]error{}
			if cleanup != "release" {
				failures["ROLLBACK TO pwnmesh_operation"] = rollbackErr
			}
			if cleanup != "rollback" {
				failures["RELEASE pwnmesh_operation"] = releaseErr
			}
			calls := map[string]int{}
			err = store.Do(context.Background(), func(q Queries) error {
				return Atomic(atomicFailureQueries{q, failures, calls}, func() error {
					if _, err := q.Exec("UPDATE settings SET intent_timeout=55"); err != nil {
						return err
					}
					if cleanup == "release" {
						return nil
					}
					return operationErr
				})
			})
			if cleanup != "release" && (!errors.Is(err, operationErr) || !errors.Is(err, rollbackErr)) || cleanup != "rollback" && !errors.Is(err, releaseErr) {
				t.Fatalf("cleanup lost an operation or cleanup error: %v", err)
			}
			if calls["ROLLBACK TO pwnmesh_operation"] != 1 || cleanup == "release" && calls["RELEASE pwnmesh_operation"] != 2 {
				t.Fatalf("cleanup skipped rollback or release retry: %+v", calls)
			}
			if err := store.View(context.Background(), func(q Queries) error {
				settings, err := ReadSettings(q)
				if err == nil && settings.IntentTimeout != 15 {
					t.Fatal("transaction owner committed after cleanup failure")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
