package server

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"pwnmesh/internal/board"
)

// Test fixtures own their SQL connections. Production Board transactions expose
// only domain operations; fault injection never adds a public storage backdoor.
var fixtureDatabasePaths sync.Map

type fixtureSQLTx = sql.Tx

type fixtureSQLStore struct{ path string }

func openFixtureStore(t testing.TB, path string) (*board.Store, error) {
	store, err := board.Open(path)
	if err == nil {
		fixtureDatabasePaths.Store(store, path)
		t.Cleanup(func() { fixtureDatabasePaths.Delete(store) })
	}
	return store, err
}

func fixtureSQL(store *board.Store) fixtureSQLStore {
	path, _ := fixtureDatabasePaths.Load(store)
	value, _ := path.(string)
	return fixtureSQLStore{path: value}
}

func (store fixtureSQLStore) Do(ctx context.Context, fn func(*fixtureSQLTx) error) error {
	if store.path == "" || store.path == ":memory:" {
		return fmt.Errorf("fixture SQL requires a registered file database")
	}
	db, err := sql.Open("sqlite", store.path+"?_txlock=immediate&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
