package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fervovita/AuthZ-dev/internal/core"
	"github.com/fervovita/AuthZ-dev/internal/schema"
)

// schemaLabel names the source in compile errors, since a schema arrives as text.
const schemaLabel = "schema"

// WriteSchema stores source as the newest schema.
func (s *Store) WriteSchema(ctx context.Context, source string) error {
	if _, err := schema.Compile(schemaLabel, source, core.NewDictionary()); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	if _, err := s.pool.Exec(ctx, `INSERT INTO schema_versions (source) VALUES ($1)`, source); err != nil {
		return fmt.Errorf("postgres: write schema: %w", err)
	}

	return nil
}

// latestSchema compiles the newest schema tx sees, with the dictionary that names its IDs.
func latestSchema(ctx context.Context, tx pgx.Tx) (*core.Schema, *core.Dictionary, error) {
	var (
		version int64
		source  string
	)

	err := tx.QueryRow(ctx, `SELECT version, source FROM schema_versions ORDER BY version DESC LIMIT 1`).Scan(&version, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: no schema has been written", ErrInvalid)
	}

	if err != nil {
		return nil, nil, fmt.Errorf("postgres: read schema: %w", err)
	}

	d := core.NewDictionary()

	compiled, err := schema.Compile(schemaLabel, source, d)
	if err != nil {
		// Stored by a binary whose rules this one no longer shares: not the writer's fault, so not ErrInvalid.
		return nil, nil, fmt.Errorf("postgres: schema version %d no longer compiles: %w", version, err)
	}

	return compiled, d, nil
}
