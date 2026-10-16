package board

import (
	"context"
	"time"

	"pwnmesh/internal/board/internal/sqlstore"
)

// Store adds the domain clock to the private SQLite transaction owner.
type Store struct {
	storage *sqlstore.Store
	Now     func() time.Time
}

type transactionQueries = sqlstore.Queries

type Tx struct {
	transactionQueries sqlstore.Queries
	Now                string
	readOnly           bool
}

func Open(path string) (*Store, error) {
	storage, err := sqlstore.Open(path)
	if err != nil {
		return nil, err
	}
	return &Store{storage: storage, Now: time.Now}, nil
}

func (s *Store) Close() error {
	return s.storage.Close()
}

// View keeps domain reads on one snapshot. Lease expiry and state changes use Do.
func (s *Store) View(ctx context.Context, fn func(*Tx) error) error {
	return s.storage.View(ctx, func(q sqlstore.Queries) error {
		return fn(&Tx{transactionQueries: q, Now: s.Now().UTC().Format(time.RFC3339), readOnly: true})
	})
}

func (s *Store) Do(ctx context.Context, fn func(*Tx) error) error {
	return s.storage.Do(ctx, func(q sqlstore.Queries) error {
		return fn(&Tx{transactionQueries: q, Now: s.Now().UTC().Format(time.RFC3339)})
	})
}
