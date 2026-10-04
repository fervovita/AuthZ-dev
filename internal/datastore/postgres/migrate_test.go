//go:build integration

package postgres

import (
	"slices"
	"testing"
)

func TestMigrateTwiceAppliesOnce(t *testing.T) {
	t.Parallel()

	s := openStore(t, newDatabase(t))

	for range 2 {
		if err := s.Migrate(t.Context()); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}

	assertMigrated(t, s)
}

// Servers that start together all migrate the same database.
func TestMigrateConcurrentCallersApplyOnce(t *testing.T) {
	t.Parallel()

	const servers = 8

	db := newDatabase(t)

	migrate := make([]func() error, servers)
	for i := range migrate {
		s := openStore(t, db)

		migrate[i] = func() error { return s.Migrate(t.Context()) }
	}

	for i, err := range together(migrate...) {
		if err != nil {
			t.Errorf("server %d: Migrate: %v", i, err)
		}
	}

	assertMigrated(t, openStore(t, db))
}

// assertMigrated checks that every version is recorded once and that the tables they create exist.
func assertMigrated(t *testing.T, s *Store) {
	t.Helper()

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}

	want := make([]int, 0, len(migrations))
	for _, m := range migrations {
		want = append(want, m.version)
	}

	var got []int
	if err := s.pool.QueryRow(t.Context(), `SELECT array_agg(version ORDER BY version) FROM migrations`).Scan(&got); err != nil {
		t.Fatalf("read migrations: %v", err)
	}

	if !slices.Equal(got, want) {
		t.Errorf("migrations = %v; want %v", got, want)
	}

	for _, table := range []string{"tuples", "change_log", "schema_versions"} {
		var exists bool
		if err := s.pool.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatalf("look up %s: %v", table, err)
		}

		if !exists {
			t.Errorf("table %s does not exist", table)
		}
	}
}
