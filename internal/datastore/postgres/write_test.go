//go:build integration

package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// publisher takes a team itself, so an unknown subject relation read as none would pass there.
const testSchema = `
	definition user {}

	definition team {
		relation member: user | team#member
		relation viewer: user
	}

	definition document {
		relation viewer: user | user:* | team | team#member
		relation publisher: team
		relation banned: user
		permission view = viewer - banned
	}
`

// edit returns testSchema with each edit's first string replaced by its second.
func edit(t *testing.T, edits ...[2]string) string {
	t.Helper()

	s := testSchema

	for _, e := range edits {
		if !strings.Contains(s, e[0]) {
			t.Fatalf("testSchema no longer contains %q", e[0])
		}

		s = strings.Replace(s, e[0], e[1], 1)
	}

	return s
}

// schemaStore opens a Store on a fresh database, migrated and holding testSchema.
func schemaStore(t *testing.T) *Store {
	t.Helper()

	s := openStore(t, newDatabase(t))

	if err := s.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if err := s.WriteSchema(t.Context(), testSchema); err != nil {
		t.Fatalf("WriteSchema: %v", err)
	}

	return s
}

// tuple reads document:d1#viewer@team:eng#member the way Tuple.String writes it.
func tuple(s string) Tuple {
	resource, subject, _ := strings.Cut(s, "@")
	object, relation, _ := strings.Cut(resource, "#")
	typ, id, _ := strings.Cut(object, ":")
	subjectObject, subjectRelation, _ := strings.Cut(subject, "#")
	subjectType, subjectID, _ := strings.Cut(subjectObject, ":")

	return Tuple{typ, id, relation, subjectType, subjectID, subjectRelation}
}

func touch(s string) Update {
	return Update{Touch, tuple(s)}
}

func remove(s string) Update {
	return Update{Delete, tuple(s)}
}

// stored returns the rows of table, which holds tuple columns, as sorted strings.
func stored(t *testing.T, s *Store, table string) []string {
	t.Helper()

	rows, err := s.pool.Query(t.Context(), `SELECT `+columns+` FROM `+table)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}

	read, err := pgx.CollectRows(rows, scanTuple)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}

	out := make([]string, 0, len(read))
	for _, u := range read {
		out = append(out, u.String())
	}

	slices.Sort(out)

	return out
}

func write(t *testing.T, s *Store, updates ...Update) {
	t.Helper()

	if err := s.Write(t.Context(), updates); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func assertRows(t *testing.T, s *Store, table string, want ...string) {
	t.Helper()

	slices.Sort(want)

	if got := stored(t, s, table); !slices.Equal(got, want) {
		t.Errorf("%s = %q; want %q", table, got, want)
	}
}

func TestWriteStoresAndLogsEverySubjectForm(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	all := []string{
		"document:d1#viewer@user:alice",
		"document:d1#viewer@user:*",
		"document:d1#viewer@team:eng#member",
		"document:d1#publisher@team:eng",
	}

	updates := make([]Update, 0, len(all))
	for _, u := range all {
		updates = append(updates, touch(u))
	}

	write(t, s, updates...)

	assertRows(t, s, "tuples", all...)
	assertRows(t, s, "change_log", all...)
}

// Two requests over the same tuples, listed in opposite orders, must both succeed: neither may wait on the other
// in a cycle, nor fail to serialize behind the other's commit.
func TestWriteRacingRequestsBothSucceed(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	const a, b = "document:d1#viewer@user:a", "document:d1#viewer@user:b"

	for range 20 {
		write(t, s, remove(a), remove(b))

		errs := together(
			func() error { return s.Write(t.Context(), []Update{touch(a), touch(b)}) },
			func() error { return s.Write(t.Context(), []Update{touch(b), touch(a)}) },
		)

		if err := errors.Join(errs...); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
}

// Requests that add a pair and requests that remove it, all at once: the store ends with the pair whole or gone,
// as some order of the requests would leave it. Rounds start from the pair whole and from it gone by turns.
func TestWriteRacingRequestsLeaveAPairWholeOrGone(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	add := []Update{touch("document:p#viewer@user:u"), touch("document:q#viewer@user:u")}
	drop := []Update{remove("document:p#viewer@user:u"), remove("document:q#viewer@user:u")}

	requests := make([]func() error, 8)
	for i := range requests {
		updates := add
		if i%2 == 1 {
			updates = drop
		}

		requests[i] = func() error { return s.Write(t.Context(), updates) }
	}

	for i := range 100 {
		if i%2 == 0 {
			write(t, s, add...)
		} else {
			write(t, s, drop...)
		}

		if err := errors.Join(together(requests...)...); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}

		if got := stored(t, s, "tuples"); len(got) == 1 {
			t.Fatalf("round %d: the store holds %q, half of the pair", i, got)
		}
	}
}

// hold takes the locks a Write of updates takes and keeps them to the end of the test, as a request in flight would.
func hold(t *testing.T, s *Store, updates ...Update) {
	t.Helper()

	if err := acquire(t.Context(), begin(t, s), "hold", writeLocks(updates)); err != nil {
		t.Fatalf("acquire: %v", err)
	}
}

// While a request is in flight, requests elsewhere go through at once, and those over its tuple, its relation
// as a whole or the schema wait for it.
func TestWriteHoldsOnlyWhatItTouches(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	hold(t, s, touch("document:d1#viewer@user:a"))

	for _, c := range []struct {
		name  string
		waits bool
		do    func(context.Context) error
	}{
		{"another tuple", false, func(ctx context.Context) error {
			return s.Write(ctx, []Update{touch("document:d1#viewer@user:b")})
		}},
		{"another relation's filter", false, func(ctx context.Context) error {
			return s.DeleteMatching(ctx, filter("document#banned"))
		}},
		{"a large request on another relation", false, func(ctx context.Context) error {
			return s.Write(ctx, touchMany("banned", maxTupleLocks+1))
		}},
		{"the same tuple", true, func(ctx context.Context) error {
			return s.Write(ctx, []Update{remove("document:d1#viewer@user:a")})
		}},
		{"its relation's filter", true, func(ctx context.Context) error {
			return s.DeleteMatching(ctx, filter("document#viewer"))
		}},
		{"a large request on its relation", true, func(ctx context.Context) error {
			return s.Write(ctx, touchMany("viewer", maxTupleLocks+1))
		}},
		{"a schema", true, func(ctx context.Context) error {
			return s.WriteSchema(ctx, testSchema)
		}},
	} {
		limit := 5 * time.Second
		if c.waits {
			limit = 200 * time.Millisecond
		}

		ctx, cancel := context.WithTimeout(t.Context(), limit)
		err := c.do(ctx)

		cancel()

		if waited := errors.Is(err, context.DeadlineExceeded); waited != c.waits || (!waited && err != nil) {
			t.Errorf("%s: err = %v; want it to wait: %v", c.name, err, c.waits)
		}
	}
}

// A large request locks every relation it names, so one the schema refuses is turned away before it locks:
// it does not wait for the request in flight on its relation.
func TestWriteRefusesALargeRequestBeforeLocking(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	hold(t, s, touch("document:d1#viewer@user:a"))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	err := s.Write(ctx, append(touchMany("viewer", maxTupleLocks), touch("document:d1#editor@user:alice")))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "document#editor is not defined") {
		t.Fatalf("Write = %v; want ErrInvalid saying document#editor is not defined, without a wait", err)
	}
}

// Touching a stored tuple or deleting an absent one changes nothing, so nothing is logged.
func TestWriteLogsOnlyChanges(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	const alice, bob = "document:d1#viewer@user:alice", "document:d1#viewer@user:bob"

	write(t, s, touch(alice))
	write(t, s, touch(alice), remove(bob))

	assertRows(t, s, "tuples", alice)
	assertRows(t, s, "change_log", alice)

	write(t, s, remove(alice))

	assertRows(t, s, "tuples")
	assertRows(t, s, "change_log", alice, alice)
}

// Every case follows a valid update, which must not be written either.
func TestWriteRefusesTheWholeRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		bad  []Update
		want string
	}{
		{"an unknown operation", []Update{{Tuple: tuple("document:d1#viewer@user:bob")}}, "unknown operation 0"},
		{"a malformed identifier", []Update{touch("document:d 1#viewer@user:bob")}, "breaks the identifier rules"},
		{"an undefined relation", []Update{touch("document:d1#editor@user:bob")}, "document#editor is not defined"},
		{"an undefined type", []Update{touch("folder:f1#viewer@user:bob")}, "folder#viewer is not defined"},
		{"a permission", []Update{touch("document:d1#view@user:bob")}, "document#view is a permission"},
		{"a subject type not accepted", []Update{touch("document:d1#banned@user:*")}, "document#banned does not accept user:*"},
		{"an unknown subject type", []Update{touch("document:d1#viewer@robot:r1")}, "document#viewer does not accept robot"},
		{"an unknown subject relation", []Update{touch("document:d1#publisher@team:eng#bogus")}, "document#publisher does not accept team#bogus"},
		{"a repeated tuple", []Update{touch("document:d1#viewer@user:bob"), remove("document:d1#viewer@user:bob")}, "updates[2] document:d1#viewer@user:bob: repeats updates[1]"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			s := schemaStore(t)

			err := s.Write(t.Context(), append([]Update{touch("document:d1#viewer@user:alice")}, c.bad...))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Write = %v; want ErrInvalid saying %q", err, c.want)
			}

			assertRows(t, s, "tuples")
			assertRows(t, s, "change_log")
		})
	}
}

func TestWriteRefusesBeforeAnySchema(t *testing.T) {
	t.Parallel()

	s := openStore(t, newDatabase(t))

	if err := s.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if err := s.Write(t.Context(), []Update{touch("document:d1#viewer@user:alice")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Write = %v; want ErrInvalid", err)
	}
}

// A schema that no longer accepts team#member on document#viewer refuses the tuple the first one took.
func TestWriteChecksTheNewestSchema(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	narrowed := edit(t, [2]string{"viewer: user | user:* | team | team#member", "viewer: user | user:* | team"})

	if err := s.WriteSchema(t.Context(), narrowed); err != nil {
		t.Fatalf("WriteSchema: %v", err)
	}

	err := s.Write(t.Context(), []Update{touch("document:d1#viewer@team:eng#member")})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "does not accept team#member") {
		t.Fatalf("Write = %v; want the newest schema to refuse team#member", err)
	}
}

// A binary with stricter rules than the one that stored the schema: the writer is not at fault.
func TestWriteReportsAStoredSchemaThatNoLongerCompiles(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	if _, err := s.pool.Exec(t.Context(), `INSERT INTO schema_versions (source) VALUES ('definition')`); err != nil {
		t.Fatalf("store a schema: %v", err)
	}

	err := s.Write(t.Context(), []Update{touch("document:d1#viewer@user:alice")})
	if err == nil || errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "no longer compiles") {
		t.Fatalf("Write = %v; want a failure that is not ErrInvalid", err)
	}
}
