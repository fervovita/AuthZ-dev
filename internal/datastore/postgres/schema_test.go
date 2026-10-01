//go:build integration

package postgres

import (
	"errors"
	"strings"
	"testing"
)

func TestWriteSchemaStoresTheSource(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	var source string
	if err := s.pool.QueryRow(t.Context(), `SELECT source FROM schema_versions`).Scan(&source); err != nil {
		t.Fatalf("read schema_versions: %v", err)
	}

	if source != testSchema {
		t.Errorf("stored source = %q; want testSchema as written", source)
	}
}

// The reason carries the compiler's position, and nothing is stored.
func TestWriteSchemaRefusesASourceThatDoesNotCompile(t *testing.T) {
	t.Parallel()

	s := openStore(t, newDatabase(t))

	if err := s.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	err := s.WriteSchema(t.Context(), "definition document {\n\trelation viewer: nobody\n}")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "schema:2:") {
		t.Fatalf("WriteSchema = %v; want ErrInvalid with a position", err)
	}

	var n int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM schema_versions`).Scan(&n); err != nil {
		t.Fatalf("count schema_versions: %v", err)
	}

	if n != 0 {
		t.Errorf("schema_versions holds %d rows; want none", n)
	}
}
