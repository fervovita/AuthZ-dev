//go:build integration

package postgres

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// follower keeps a copy of a store the way a sidecar would, through Snapshot and Changes alone.
type follower struct {
	s      *Store
	rev    Revision
	schema string
	tuples map[Tuple]bool
}

func follow(t *testing.T, s *Store) *follower {
	t.Helper()

	st, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	return newFollower(s, st)
}

func newFollower(s *Store, st State) *follower {
	f := &follower{s: s, rev: st.Revision, schema: st.Schema, tuples: make(map[Tuple]bool)}
	for _, u := range st.Tuples {
		f.tuples[u] = true
	}

	return f
}

func (f *follower) catchUp(t *testing.T) Delta {
	t.Helper()

	d, err := f.s.Changes(t.Context(), f.rev)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}

	f.advance(d)

	return d
}

func (f *follower) advance(d Delta) {
	f.rev = d.Revision

	if d.Schema != nil {
		f.schema = *d.Schema
	}

	for _, c := range d.Changes {
		if c.Present {
			f.tuples[c.Tuple] = true
		} else {
			delete(f.tuples, c.Tuple)
		}
	}
}

// held returns the tuples f holds as sorted strings, the form stored returns.
func (f *follower) held() []string {
	var out []string
	for u := range maps.Keys(f.tuples) {
		out = append(out, u.String())
	}

	slices.Sort(out)

	return out
}

// assertCaughtUp checks f against the store's tables, read directly rather than through the read path.
func (f *follower) assertCaughtUp(t *testing.T) {
	t.Helper()

	if got, want := f.held(), stored(t, f.s, "tuples"); !slices.Equal(got, want) {
		t.Errorf("follower holds %q; the store holds %q", got, want)
	}
}

// schemaIn formats d's schema for a failure: quoted, or none.
func schemaIn(d Delta) string {
	if d.Schema == nil {
		return "none"
	}

	return strconv.Quote(*d.Schema)
}

// changes returns d's changes as sorted strings: the tuple, then "+" if present or "-" if not.
func changes(d Delta) []string {
	var out []string

	for _, c := range d.Changes {
		state := "-"
		if c.Present {
			state = "+"
		}

		out = append(out, c.Tuple.String()+" "+state)
	}

	slices.Sort(out)

	return out
}

func TestSnapshotHoldsTheNewestSchemaAndEveryTuple(t *testing.T) {
	t.Parallel()

	s := filterStore(t)

	widened := edit(t, [2]string{"relation banned: user", "relation banned: user | team#member"})
	if err := s.WriteSchema(t.Context(), widened); err != nil {
		t.Fatalf("WriteSchema: %v", err)
	}

	f := follow(t, s)

	if f.schema != widened {
		t.Errorf("Snapshot schema = %q; want the last one written", f.schema)
	}

	f.assertCaughtUp(t)
}

// Nothing is written yet, and what comes later arrives as changes.
func TestSnapshotBeforeAnySchema(t *testing.T) {
	t.Parallel()

	s := openStore(t, newDatabase(t))

	if err := s.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	f := follow(t, s)

	if f.schema != "" || len(f.tuples) != 0 || len(f.rev) == 0 {
		t.Fatalf("Snapshot = revision %q, schema %q, %d tuples; want a revision and nothing else", f.rev, f.schema, len(f.tuples))
	}

	if err := s.WriteSchema(t.Context(), testSchema); err != nil {
		t.Fatalf("WriteSchema: %v", err)
	}

	write(t, s, touch("document:d1#viewer@user:alice"))

	d := f.catchUp(t)
	if d.Schema == nil || *d.Schema != testSchema {
		t.Errorf("Changes schema = %s; want testSchema", schemaIn(d))
	}

	f.assertCaughtUp(t)
}

// Each tuple a commit touched arrives once, as it stands now; one no commit changed does not arrive.
func TestChangesSendsStateNotOperations(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	const (
		added    = "document:d1#viewer@user:a"
		dropped  = "document:d1#viewer@user:b"
		restored = "document:d1#viewer@user:c"
		kept     = "document:d1#viewer@user:d"
		banned   = "document:d1#banned@user:e"
	)

	write(t, s, touch(restored), touch(kept), touch(banned))

	f := follow(t, s)

	write(t, s, touch(added))
	write(t, s, touch(dropped))
	write(t, s, remove(dropped))
	write(t, s, remove(restored))
	write(t, s, touch(restored), touch(kept))
	deleteMatching(t, s, filter("document#banned"))

	want := []string{added + " +", banned + " -", dropped + " -", restored + " +"}
	slices.Sort(want)

	if got := changes(f.catchUp(t)); !slices.Equal(got, want) {
		t.Errorf("Changes = %q; want %q", got, want)
	}

	f.assertCaughtUp(t)
}

// Only the newest of the schemas written since arrives, and none when none was written.
func TestChangesCarriesTheNewestSchema(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	f := follow(t, s)

	first := edit(t, [2]string{"relation banned: user", "relation banned: user | team#member"})
	second := edit(t, [2]string{"relation publisher: team", "relation publisher: team | user"})

	for _, source := range []string{first, second} {
		if err := s.WriteSchema(t.Context(), source); err != nil {
			t.Fatalf("WriteSchema: %v", err)
		}
	}

	if d := f.catchUp(t); d.Schema == nil || *d.Schema != second {
		t.Errorf("Changes schema = %s; want the last one written", schemaIn(d))
	}

	if d := f.catchUp(t); d.Schema != nil || len(d.Changes) != 0 {
		t.Errorf("Changes = schema %s, %q; want nothing after catching up", schemaIn(d), changes(d))
	}
}

// An empty source compiles to a schema with nothing in it, so it arrives like any other.
func TestChangesCarriesAnEmptySchema(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	f := follow(t, s)

	if err := s.WriteSchema(t.Context(), ""); err != nil {
		t.Fatalf("WriteSchema: %v", err)
	}

	if d := f.catchUp(t); d.Schema == nil || *d.Schema != "" {
		t.Errorf("Changes schema = %s; want the empty one written", schemaIn(d))
	}
}

// begin opens a transaction a test drives by hand, rolled back when the test ends.
func begin(t *testing.T, s *Store) pgx.Tx {
	t.Helper()

	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	return tx
}

func apply(t *testing.T, tx pgx.Tx, u Update) {
	t.Helper()

	sql := touchSQL
	if u.Op == Delete {
		sql = deleteSQL
	}

	v := u.Tuple
	if _, err := tx.Exec(t.Context(), sql, v.ResourceType, v.ResourceID, v.Relation, v.SubjectType, v.SubjectID, v.SubjectRelation); err != nil {
		t.Fatalf("apply %v: %v", u, err)
	}
}

func commit(t *testing.T, tx pgx.Tx) {
	t.Helper()

	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// T1, T2 and T3 take their transaction IDs in that order. T3 unbans alice and commits first; T1 bans her again
// and commits after it, while T2 is still open. A reader that catches up then, and again once T2 commits, must
// end where the store did: alice banned. Replaying by transaction ID would put the unban last.
func TestChangesFollowsCommitsNotTransactionOrder(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	const ban = "document:d1#banned@user:alice"

	write(t, s, touch(ban))

	f := follow(t, s)

	t1, t2, t3 := begin(t, s), begin(t, s), begin(t, s)

	apply(t, t1, touch("document:d2#viewer@user:bob"))
	apply(t, t2, touch("document:d3#viewer@user:carol"))
	apply(t, t3, remove(ban))
	commit(t, t3)
	apply(t, t1, touch(ban))
	commit(t, t1)

	f.catchUp(t)
	f.assertCaughtUp(t)

	commit(t, t2)

	f.catchUp(t)
	f.assertCaughtUp(t)

	if !f.tuples[tuple(ban)] {
		t.Error("the follower does not hold alice's ban")
	}
}

// readWhileLocked runs read while another transaction holds tuples, and has that transaction apply u and
// commit once read waits on the table: after read has taken its revision, before it has read a tuple.
func readWhileLocked[T any](t *testing.T, s *Store, u Update, read func() (T, error)) T {
	t.Helper()

	blocker := begin(t, s)

	if _, err := blocker.Exec(t.Context(), `LOCK TABLE tuples IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock tuples: %v", err)
	}

	type result struct {
		v   T
		err error
	}

	done := make(chan result, 1)

	go func() {
		v, err := read()
		done <- result{v, err}
	}()

	for waiting := false; !waiting; {
		select {
		case r := <-done:
			t.Fatalf("the read finished without waiting on tuples: %v", r.err)
		case <-time.After(time.Millisecond):
		}

		if err := s.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_locks
			WHERE database = (SELECT oid FROM pg_database WHERE datname = current_database())
				AND relation = 'tuples'::regclass AND NOT granted)`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
	}

	apply(t, blocker, u)
	commit(t, blocker)

	r := <-done
	if r.err != nil {
		t.Fatalf("read: %v", r.err)
	}

	return r.v
}

// A commit that lands while Snapshot reads is not in it, and comes as a change after it.
func TestSnapshotReadsAtItsRevision(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	const alice = "document:d1#viewer@user:alice"

	st := readWhileLocked(t, s, touch(alice), func() (State, error) { return s.Snapshot(t.Context()) })
	if len(st.Tuples) != 0 {
		t.Fatalf("Snapshot holds %v; want the commit after its revision left out", st.Tuples)
	}

	d, err := s.Changes(t.Context(), st.Revision)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}

	if got, want := changes(d), []string{alice + " +"}; !slices.Equal(got, want) {
		t.Errorf("Changes = %q; want %q", got, want)
	}
}

// A tuple Changes reports stands as it did at the delta's revision, not as a commit during the read left it.
func TestChangesReadsAtItsRevision(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	const alice = "document:d1#viewer@user:alice"

	f := follow(t, s)

	write(t, s, touch(alice))

	d := readWhileLocked(t, s, remove(alice), func() (Delta, error) { return s.Changes(t.Context(), f.rev) })
	if got, want := changes(d), []string{alice + " +"}; !slices.Equal(got, want) {
		t.Fatalf("Changes = %q; want %q", got, want)
	}

	next, err := s.Changes(t.Context(), d.Revision)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}

	if got, want := changes(next), []string{alice + " -"}; !slices.Equal(got, want) {
		t.Errorf("Changes after = %q; want %q", got, want)
	}
}
