//go:build integration

package postgres

import (
	"errors"
	"slices"
	"strings"
	"sync"
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

	if n := versions(t, s); n != 0 {
		t.Errorf("schema_versions holds %d rows; want none", n)
	}
}

// versions counts the schemas s has stored.
func versions(t *testing.T, s *Store) int {
	t.Helper()

	var n int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM schema_versions`).Scan(&n); err != nil {
		t.Fatalf("count schema_versions: %v", err)
	}

	return n
}

// The reason lists a filter for everything dropped that tuples use, and nothing else.
// Deleting what those filters match lets the same schema through.
func TestWriteSchemaRefusesToDropWhatTuplesUse(t *testing.T) {
	t.Parallel()

	const viewer = "viewer: user | user:* | team | team#member"

	cases := []struct {
		name   string
		tuples []string
		edits  [][2]string
		want   []string
	}{
		{
			"a relation",
			[]string{"document:d1#banned@user:alice"},
			[][2]string{{"relation banned: user", ""}, {"viewer - banned", "viewer"}},
			[]string{"document#banned"},
		},
		{
			"a relation that becomes a permission",
			[]string{"document:d1#banned@user:alice"},
			[][2]string{{"relation banned: user", "permission banned = viewer"}},
			[]string{"document#banned"},
		},
		{
			"a subject type",
			[]string{"document:d1#viewer@user:alice"},
			[][2]string{{viewer, "viewer: user:* | team | team#member"}},
			[]string{"document#viewer@user"},
		},
		{
			"a wildcard",
			[]string{"document:d1#viewer@user:*"},
			[][2]string{{viewer, "viewer: user | team | team#member"}},
			[]string{"document#viewer@user:*"},
		},
		{
			"an object type beside its userset",
			[]string{"document:d1#viewer@team:eng"},
			[][2]string{{viewer, "viewer: user | user:* | team#member"}},
			[]string{"document#viewer@team"},
		},
		{
			"a userset",
			[]string{"document:d1#viewer@team:eng#member"},
			[][2]string{{viewer, "viewer: user | user:* | team"}},
			[]string{"document#viewer@team#member"},
		},
		{
			"the relation a userset names",
			[]string{"document:d1#viewer@team:eng#member", "team:eng#member@user:alice"},
			[][2]string{{"relation member: user | team#member", ""}, {viewer, "viewer: user | user:* | team"}},
			[]string{"team#member", "document#viewer@team#member"},
		},
		{
			"a definition",
			[]string{"document:d1#publisher@team:eng", "team:eng#member@user:alice"},
			[][2]string{
				{"definition team {\n\t\trelation member: user | team#member\n\t\trelation viewer: user\n\t}", ""},
				{viewer, "viewer: user | user:*"},
				{"relation publisher: team", ""},
			},
			[]string{"team#member", "document#publisher"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			s := schemaStore(t)

			for _, u := range c.tuples {
				write(t, s, touch(u))
			}

			next := edit(t, c.edits...)

			err := s.WriteSchema(t.Context(), next)
			if !errors.Is(err, ErrTuplesRemain) {
				t.Fatalf("WriteSchema = %v; want ErrTuplesRemain", err)
			}

			_, list, _ := strings.Cut(err.Error(), "delete them first: ")
			got := strings.Split(list, ", ")
			slices.Sort(got)

			want := slices.Sorted(slices.Values(c.want))
			if !slices.Equal(got, want) {
				t.Errorf("WriteSchema lists %q; want %q", got, want)
			}

			if n := versions(t, s); n != 1 {
				t.Fatalf("schema_versions holds %d rows after the refusal; want 1", n)
			}

			for _, f := range c.want {
				deleteMatching(t, s, filter(f))
			}

			if err := s.WriteSchema(t.Context(), next); err != nil {
				t.Fatalf("WriteSchema after deleting: %v", err)
			}
		})
	}
}

// Each subject type document#viewer drops goes through beside tuples of the other three,
// and so does what stores no tuples.
func TestWriteSchemaDropsWhatNoTupleUses(t *testing.T) {
	t.Parallel()

	const viewer = "viewer: user | user:* | team | team#member"

	cases := []struct {
		name    string
		without []string
		edits   [][2]string
	}{
		{
			"user",
			[]string{"document:d1#viewer@user:alice", "document:d2#viewer@user:bob"},
			[][2]string{{viewer, "viewer: user:* | team | team#member"}},
		},
		{"user:*", []string{"document:d1#viewer@user:*"}, [][2]string{{viewer, "viewer: user | team | team#member"}}},
		{"team", []string{"document:d1#viewer@team:eng"}, [][2]string{{viewer, "viewer: user | user:* | team#member"}}},
		{"team#member", []string{"document:d1#viewer@team:eng#member"}, [][2]string{{viewer, "viewer: user | user:* | team"}}},
		{"a relation", []string{"document:d1#banned@user:alice"}, [][2]string{{"relation banned: user", ""}, {"viewer - banned", "viewer"}}},
		{"a permission", nil, [][2]string{{"permission view = viewer - banned", ""}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			s := schemaStore(t)

			var updates []Update

			for _, u := range filterTuples {
				if !slices.Contains(c.without, u) {
					updates = append(updates, touch(u))
				}
			}

			write(t, s, updates...)

			if err := s.WriteSchema(t.Context(), edit(t, c.edits...)); err != nil {
				t.Fatalf("WriteSchema: %v", err)
			}

			if n := versions(t, s); n != 2 {
				t.Errorf("schema_versions holds %d rows; want 2", n)
			}
		})
	}
}

// A schema write and a tuple write the new schema refuses, started together: exactly one goes through,
// so the tuple is never stored under a schema that refuses it.
func TestWriteSchemaRacingAWriteNeverStrandsATuple(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	narrowed := edit(t, [2]string{"viewer: user | user:* | team | team#member", "viewer: user | user:* | team"})

	for i := range 50 {
		if err := s.WriteSchema(t.Context(), testSchema); err != nil {
			t.Fatalf("round %d: restore the schema: %v", i, err)
		}

		deleteMatching(t, s, filter("document#viewer@team#member"))

		var (
			schemaErr, writeErr error
			wg                  sync.WaitGroup
		)

		start := make(chan struct{})

		wg.Go(func() {
			<-start

			schemaErr = s.WriteSchema(t.Context(), narrowed)
		})
		wg.Go(func() {
			<-start

			writeErr = s.Write(t.Context(), []Update{touch("document:d1#viewer@team:eng#member")})
		})

		close(start)
		wg.Wait()

		schemaFirst := schemaErr == nil && errors.Is(writeErr, ErrInvalid)
		writeFirst := writeErr == nil && errors.Is(schemaErr, ErrTuplesRemain)

		if !schemaFirst && !writeFirst {
			t.Fatalf("round %d: WriteSchema = %v, Write = %v; want exactly one refused", i, schemaErr, writeErr)
		}
	}
}

// A binary with stricter rules than the one that stored the schema cannot tell what a new one drops.
func TestWriteSchemaReportsAStoredSchemaThatNoLongerCompiles(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	if _, err := s.pool.Exec(t.Context(), `INSERT INTO schema_versions (source) VALUES ('definition')`); err != nil {
		t.Fatalf("store a schema: %v", err)
	}

	err := s.WriteSchema(t.Context(), testSchema)
	if err == nil || errors.Is(err, ErrInvalid) || errors.Is(err, ErrTuplesRemain) || !strings.Contains(err.Error(), "no longer compiles") {
		t.Fatalf("WriteSchema = %v; want a failure that blames neither the schema nor the tuples", err)
	}
}
