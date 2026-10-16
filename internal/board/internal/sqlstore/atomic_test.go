package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
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
