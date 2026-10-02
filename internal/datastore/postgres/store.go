package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the central relationship store.
// It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// Open returns a Store on the database connString names, as a URL or key=value pairs.
// It does not change the database: call Migrate before the first use.
func Open(ctx context.Context, connString string) (*Store, error) {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}

	return &Store{pool: pool}, nil
}

// Close closes every connection.
func (s *Store) Close() {
	s.pool.Close()
}

// inTx runs fn in a transaction, and commits unless fn fails.
// fn's errors come back as they are; op names the step in the others.
func (s *Store) inTx(ctx context.Context, op string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("postgres: %s: %w", op, err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: %s: %w", op, err)
	}

	return nil
}
