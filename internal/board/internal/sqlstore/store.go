// Package sqlstore owns SQLite connections, schema validation and transaction
// finalization. Domain operations use its query surface within a callback.
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type Store struct {
	db      *sql.DB
	readers *sql.DB
}

// Queries allows statements within a transaction without exposing its handle
// or transaction lifecycle. The Store finalizes every callback transaction.
type Queries interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

type queries struct {
	transaction *sql.Tx
	ctx         context.Context
}

func (q queries) Exec(query string, args ...any) (sql.Result, error) {
	return q.transaction.ExecContext(q.ctx, query, args...)
}

func (q queries) Query(query string, args ...any) (*sql.Rows, error) {
	return q.transaction.QueryContext(q.ctx, query, args...)
}

func (q queries) QueryRow(query string, args ...any) *sql.Row {
	return q.transaction.QueryRowContext(q.ctx, query, args...)
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
	}
	// Acquire the write reservation at BEGIN, including when a second process
	// opens this database. This keeps check-and-claim atomic across connections.
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	db, err := sql.Open("sqlite", path+separator+"_txlock=immediate&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	// Reject unsupported existing stores before WAL changes their file header.
	if err = migrateStore(db); err != nil {
		db.Close()
		return nil, err
	}
	for _, q := range []string{"PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	if path != ":memory:" {
		// WAL readers keep a consistent view without reserving the only writer.
		// Mutations routed through View fail at the connection level.
		s.readers, err = sql.Open("sqlite", path+separator+"_txlock=deferred&_pragma=query_only(1)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
		if err == nil {
			s.readers.SetMaxOpenConns(4)
			err = s.readers.Ping()
		}
		if err != nil {
			if s.readers != nil {
				s.readers.Close()
			}
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Close() error {
	var readErr error
	if s.readers != nil {
		readErr = s.readers.Close()
	}
	return errors.Join(readErr, s.db.Close())
}

// View is observational. A private :memory: database reuses its only connection
// instead of opening a separate, empty database.
func (s *Store) View(ctx context.Context, fn func(Queries) error) error {
	if s.readers == nil {
		return s.memoryView(ctx, fn)
	}
	t, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer t.Rollback()
	if err := fn(queries{transaction: t, ctx: ctx}); err != nil {
		return err
	}
	return t.Commit()
}

func (s *Store) memoryView(ctx context.Context, fn func(Queries) error) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Retain the connection until rollback and reset finish, including after
	// cancellation or panic. The writer must never inherit query_only.
	defer func() {
		_, resetErr := conn.ExecContext(context.Background(), "PRAGMA query_only=OFF")
		err = errors.Join(err, resetErr)
	}()
	if _, err = conn.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return err
	}
	t, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer t.Rollback()
	if err := fn(queries{transaction: t, ctx: ctx}); err != nil {
		return err
	}
	return t.Commit()
}

// The one-connection pool serializes transactions with context-aware waiting.
// SQLite's immediate transactions also serialize other Store instances.
func (s *Store) Do(ctx context.Context, fn func(Queries) error) error {
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer t.Rollback()
	if err := fn(queries{transaction: t, ctx: ctx}); err != nil {
		return err
	}
	return t.Commit()
}
