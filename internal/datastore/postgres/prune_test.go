//go:build integration

package postgres

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func prune(t *testing.T, s *Store, keep time.Duration) {
	t.Helper()

	if err := s.Prune(t.Context(), keep); err != nil {
		t.Fatalf("Prune: %v", err)
	}
}

// age makes every mark d older, as if the Prune calls so far had been made that much earlier.
func age(t *testing.T, s *Store, d time.Duration) {
	t.Helper()

	exec(t, s, `UPDATE log_marks SET marked_at = marked_at - $1::interval`, d)
}

// assertPruned checks that Changes refuses f's revision.
func (f *follower) assertPruned(t *testing.T) {
	t.Helper()

	if d, err := f.s.Changes(t.Context(), f.rev); !errors.Is(err, ErrPruned) {
		t.Fatalf("Changes = %q, %v; want ErrPruned", changes(d), err)
	}
}

// assertNextDelta checks that f's next delta holds exactly want, and that f then holds what the store does.
func (f *follower) assertNextDelta(t *testing.T, want ...string) {
	t.Helper()

	slices.Sort(want)

	if got := changes(f.catchUp(t)); !slices.Equal(got, want) {
		t.Errorf("Changes = %q; want %q", got, want)
	}

	f.assertCaughtUp(t)
}

// until waits for sql, a condition on s's database, to hold.
func until(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()

	for held := false; !held; {
		if err := s.pool.QueryRow(t.Context(), sql, args...).Scan(&held); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}

		if !held {
			time.Sleep(time.Millisecond)
		}
	}
}

// settled waits until every transaction that has begun writing has ended, here or in another test's database:
// transaction IDs are the server's, so one still open would hold a mark back to before the changes so far.
func settled(t *testing.T, s *Store) {
	t.Helper()

	var newest string
	if err := s.pool.QueryRow(t.Context(), `SELECT pg_current_xact_id()::text`).Scan(&newest); err != nil {
		t.Fatalf("take a transaction ID: %v", err)
	}

	until(t, s, `SELECT pg_snapshot_xmin(pg_current_snapshot()) > $1::xid8`, newest)
}

// oldest waits until tx, which has written, is the oldest transaction still running: a mark is then tx's own ID.
func oldest(t *testing.T, s *Store, tx pgx.Tx) {
	t.Helper()

	var id string
	if err := tx.QueryRow(t.Context(), `SELECT pg_current_xact_id()::text`).Scan(&id); err != nil {
		t.Fatalf("read the transaction ID: %v", err)
	}

	until(t, s, `SELECT pg_snapshot_xmin(pg_current_snapshot()) = $1::xid8`, id)
}

// The log is cut where a call stood at least keep ago: a reader that last read before that call starts over,
// and one that read after it still gets every change.
func TestPruneCutsWhereACallStoodKeepAgo(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	const before, after = "document:d1#viewer@user:before", "document:d1#viewer@user:after"

	behind := follow(t, s)

	write(t, s, touch(before))
	settled(t, s)
	prune(t, s, time.Hour)

	within := follow(t, s)

	write(t, s, touch(after))
	assertRows(t, s, "change_log", before, after)

	age(t, s, 2*time.Hour)
	prune(t, s, time.Hour)

	assertRows(t, s, "change_log", after)
	behind.assertPruned(t)
	within.assertNextDelta(t, after+" +")
}

// Calls less than keep apart drop nothing.
func TestPruneDropsNothingYoungerThanKeep(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	const alice = "document:d1#viewer@user:alice"

	f := follow(t, s)

	write(t, s, touch(alice))
	settled(t, s)

	for range 3 {
		prune(t, s, time.Hour)
	}

	assertRows(t, s, "change_log", alice)
	f.assertNextDelta(t, alice+" +")
}

// With nothing to keep, the call cuts where it stands itself. A reader that starts over is answered again,
// and a call that then keeps more finds no mark that old and cuts nothing.
func TestPruneKeepingNothingCutsAtOnce(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	const alice = "document:d1#viewer@user:alice"

	behind := follow(t, s)

	write(t, s, touch(alice))
	settled(t, s)
	prune(t, s, 0)

	assertRows(t, s, "change_log")
	behind.assertPruned(t)

	again := follow(t, s)

	write(t, s, remove(alice))
	prune(t, s, time.Hour)

	assertRows(t, s, "change_log", alice)
	again.assertNextDelta(t, alice+" -")
}

// A mark is the oldest transaction still running, not the newest one done, so the log is never cut past a change
// that is yet to commit: its line is the first one kept.
func TestPruneKeepsWhatATransactionInFlightWrites(t *testing.T) {
	t.Parallel()

	const alice, bob = "document:d1#viewer@user:alice", "document:d1#viewer@user:bob"

	// inFlight opens a transaction that writes alice, and commits bob after it while it stays open.
	inFlight := func(t *testing.T, s *Store) pgx.Tx {
		t.Helper()

		tx := begin(t, s)

		apply(t, tx, touch(alice))
		oldest(t, s, tx)
		write(t, s, touch(bob))

		return tx
	}

	t.Run("marked while open, cut later", func(t *testing.T) {
		t.Parallel()

		s := schemaStore(t)
		tx := inFlight(t, s)

		prune(t, s, time.Hour)

		f := follow(t, s)

		commit(t, tx)
		age(t, s, 2*time.Hour)
		prune(t, s, time.Hour)

		assertRows(t, s, "change_log", alice, bob)
		f.assertNextDelta(t, alice+" +")
	})

	t.Run("cut while open", func(t *testing.T) {
		t.Parallel()

		s := schemaStore(t)
		tx := inFlight(t, s)
		f := follow(t, s)

		prune(t, s, 0)
		commit(t, tx)

		assertRows(t, s, "change_log", alice, bob)
		f.assertNextDelta(t, alice+" +")
	})

	// A transaction left open holds every later mark at its own ID. The first of them keeps its time,
	// so the log before the transaction is still cut once that mark is old enough.
	t.Run("open at every call", func(t *testing.T) {
		t.Parallel()

		const carol = "document:d1#viewer@user:carol"

		s := schemaStore(t)

		write(t, s, touch(carol))
		inFlight(t, s)

		prune(t, s, time.Hour)
		age(t, s, 2*time.Hour)
		prune(t, s, time.Hour)

		assertRows(t, s, "change_log", bob)
	})

	// The reader saw dave's write end but not alice's, so it still needs alice's change.
	// second holds the cut between the two: a check against dave, the newest write the reader saw, would let it through.
	t.Run("cut once committed", func(t *testing.T) {
		t.Parallel()

		const carol, dave = "document:d1#viewer@user:carol", "document:d1#viewer@user:dave"

		s := schemaStore(t)
		first := inFlight(t, s)
		second := begin(t, s)

		apply(t, second, touch(carol))
		write(t, s, touch(dave))

		f := follow(t, s)

		commit(t, first)
		oldest(t, s, second)
		prune(t, s, 0)

		f.assertPruned(t)
	})
}

// Calls that race run one after another, and each works from what the one before it left: all succeed.
func TestPruneRacingCallsAllSucceed(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	calls := make([]func() error, 8)
	for i := range calls {
		calls[i] = func() error { return s.Prune(t.Context(), 0) }
	}

	for round := range 20 {
		f := follow(t, s)

		write(t, s, touch("document:d"+strconv.Itoa(round)+"#viewer@user:alice"))
		settled(t, s)

		if err := errors.Join(together(calls...)...); err != nil {
			t.Fatalf("round %d: Prune: %v", round, err)
		}

		assertRows(t, s, "change_log")
		f.assertPruned(t)
	}
}

// A Prune that commits while Changes reads does not empty the delta: Changes checks the marks and reads the log
// at one revision. The next call sees the cut.
func TestChangesReadsTheLogItCheckedDespiteAPrune(t *testing.T) {
	t.Parallel()

	s := schemaStore(t)

	const alice = "document:d1#viewer@user:alice"

	f := follow(t, s)

	write(t, s, touch(alice))
	settled(t, s)

	d := readWhileLocked(t, s, func(pgx.Tx) { prune(t, s, 0) },
		func() (Delta, error) { return s.Changes(t.Context(), f.rev) })

	assertRows(t, s, "change_log")

	if got, want := changes(d), []string{alice + " +"}; !slices.Equal(got, want) {
		t.Fatalf("Changes = %q; want %q", got, want)
	}

	f.assertPruned(t)
}

// Prune waits for another Prune and for nothing else, and no write waits for it.
func TestPruneWaitsOnlyForAnotherPrune(t *testing.T) {
	t.Parallel()

	alice := touch("document:d1#viewer@user:alice")
	pruneNow := func(ctx context.Context, s *Store) error { return s.Prune(ctx, 0) }

	for _, c := range []struct {
		name  string
		held  []lock
		waits bool
		do    func(context.Context, *Store) error
	}{
		{"Prune behind a prune", []lock{pruneLock()}, true, pruneNow},
		{"Prune behind a write", writeLocks([]Update{alice}), false, pruneNow},
		{"Prune behind a schema write", []lock{schemaLock(true)}, false, pruneNow},
		{"Write behind a prune", []lock{pruneLock()}, false, func(ctx context.Context, s *Store) error {
			return s.Write(ctx, []Update{alice})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			s := schemaStore(t)

			if err := acquire(t.Context(), begin(t, s), "hold", c.held); err != nil {
				t.Fatalf("acquire: %v", err)
			}

			limit := 5 * time.Second
			if c.waits {
				limit = 200 * time.Millisecond
			}

			ctx, cancel := context.WithTimeout(t.Context(), limit)
			defer cancel()

			err := c.do(ctx, s)
			if waited := errors.Is(err, context.DeadlineExceeded); waited != c.waits || (!waited && err != nil) {
				t.Errorf("err = %v; want it to wait: %v", err, c.waits)
			}
		})
	}
}
