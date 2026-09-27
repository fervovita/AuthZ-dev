package postgres

import (
	"context"
	"fmt"

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
