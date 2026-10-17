// Package boardfixture owns independent SQL connections for test setup and
// fault injection without exposing production Board transaction internals.
package boardfixture

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"pwnmesh/internal/board"
)

var databasePaths sync.Map

func Open(t testing.TB, path string) (*board.Store, error) {
	store, err := board.Open(path)
	if err == nil {
		databasePaths.Store(store, path)
		t.Cleanup(func() { databasePaths.Delete(store) })
	}
	return store, err
}

type SQLStore struct{ path string }

func SQL(store *board.Store) SQLStore {
	path, _ := databasePaths.Load(store)
	value, _ := path.(string)
	return SQLStore{path: value}
}

func (store SQLStore) Do(ctx context.Context, fn func(*sql.Tx) error) error {
	return store.transaction(ctx, false, fn)
}

func (store SQLStore) View(ctx context.Context, fn func(*sql.Tx) error) error {
	return store.transaction(ctx, true, fn)
}

func (store SQLStore) transaction(ctx context.Context, readOnly bool, fn func(*sql.Tx) error) error {
	if store.path == "" || store.path == ":memory:" {
		return errors.New("fixture SQL requires a registered file database")
	}
	options := "?_txlock=immediate&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	if readOnly {
		options = "?_txlock=deferred&_pragma=query_only(1)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", store.path+options)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
