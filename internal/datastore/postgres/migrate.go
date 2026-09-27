package postgres

import (
	"cmp"
	"context"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version int
	sql     string
}

// Migrate brings the database up to this package's tables.
// Callers that race run one at a time, so every server may call it on start.
func (s *Store) Migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	// Read committed whatever the database's default: after the lock wait,
	// each statement sees what the previous holder committed.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("postgres: migrate: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('datastore.postgres.migrate', 0))`); err != nil {
		return fmt.Errorf("postgres: migrate: %w", err)
	}

	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS migrations (version integer PRIMARY KEY)`); err != nil {
		return fmt.Errorf("postgres: migrate: %w", err)
	}

	var applied int
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM migrations`).Scan(&applied); err != nil {
		return fmt.Errorf("postgres: migrate: %w", err)
	}

	for _, m := range migrations {
		if m.version <= applied {
			continue
		}

		if _, err := tx.Exec(ctx, m.sql); err != nil {
			return fmt.Errorf("postgres: migration %d: %w", m.version, err)
		}

		if _, err := tx.Exec(ctx, `INSERT INTO migrations (version) VALUES ($1)`, m.version); err != nil {
			return fmt.Errorf("postgres: migration %d: %w", m.version, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: migrate: %w", err)
	}

	return nil
}

// loadMigrations returns the files under migrations in version order.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}

	out := make([]migration, 0, len(entries))

	for _, e := range entries {
		prefix, _, _ := strings.Cut(e.Name(), "_")

		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("postgres: migration %s: name must start with its version", e.Name())
		}

		body, err := migrationFiles.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("postgres: %w", err)
		}

		out = append(out, migration{version: version, sql: string(body)})
	}

	slices.SortFunc(out, func(a, b migration) int { return cmp.Compare(a.version, b.version) })

	return out, nil
}
