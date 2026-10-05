//go:build integration

package postgres

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fervovita/AuthZ-dev/internal/core"
	"github.com/fervovita/AuthZ-dev/internal/schema"
)

// The contract any store keeps, checked through its methods alone so that the checks do not change
// when another database's store runs them.
// While writers race, readers that follow the store hold at every revision only what one moment of it held:
// no request in part, and no tuple that revision's schema refuses.
// Once the writers stop, every reader holds what the store holds.
func TestContract(t *testing.T) {
	t.Parallel()

	if 2*contractLargePairs <= maxTupleLocks {
		t.Fatalf("a large request writes %d tuples, which this store still locks one by one; raise contractLargePairs",
			2*contractLargePairs)
	}

	runContract(t, func(t *testing.T) *Store {
		t.Helper()

		s := openStore(t, newDatabase(t))

		if err := s.Migrate(t.Context()); err != nil {
			t.Fatalf("Migrate: %v", err)
		}

		return s
	})
}

const (
	contractSeeds      = 4
	contractWriters    = 4
	contractOps        = 150 // per writer
	contractReaders    = 3   // the first starts before any schema, the last once the writers are halfway
	contractPairs      = 4   // the pairs most requests write: few, so that requests meet
	contractLargePairs = 40  // the pairs a large request writes at once, the first contractPairs among them
)

// pairForms are the tuples a pair can hold, each with %s for the resource ID.
// A pair's two tuples differ in nothing else, so every schema accepts both or neither and every filter matches both or neither.
var pairForms = []string{
	"document:%s#viewer@user:u0",
	"document:%s#viewer@user:u1",
	"document:%s#viewer@user:*",
	"document:%s#viewer@team:t0#member",
	"document:%s#banned@user:u0",
	"team:%s#member@user:u0",
}

var contractFilters = []string{
	"document#viewer",
	"document#viewer@user",
	"document#viewer@user:*",
	"document#viewer@team#member",
	"document#banned",
	"team#member",
}

// contractSchema is the schema the writers move between: each flag keeps one thing a pair can use.
func contractSchema(wildcard, teams, bans bool) string {
	viewer, banned, view := "user", "", "viewer"

	if wildcard {
		viewer += " | user:*"
	}

	if teams {
		viewer += " | team#member"
	}

	if bans {
		banned, view = "\n\t\trelation banned: user", "viewer - banned"
	}

	return `
	definition user {}

	definition team {
		relation member: user
	}

	definition document {
		relation viewer: ` + viewer + banned + `
		permission view = ` + view + `
	}
`
}

// runContract is the contract itself, apart from how a store is opened: open returns a fresh one for each seed.
func runContract(t *testing.T, open func(*testing.T) *Store) {
	for seed := range contractSeeds {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()

			s := open(t)

			// Two readers start before any schema is written.
			first := follow(t, s)  // follows all along
			asleep := follow(t, s) // sleeps until the writers stop, then catches up in one step

			if err := s.WriteSchema(t.Context(), contractSchema(true, true, true)); err != nil {
				t.Fatalf("WriteSchema: %v", err)
			}

			var (
				done     tally
				progress atomic.Int64
				writers  sync.WaitGroup
				readers  sync.WaitGroup
			)

			stop := make(chan struct{})
			followers := make([]*follower, contractReaders)

			for i := range contractReaders {
				readers.Go(func() {
					f := first

					if i > 0 {
						// The last reader waits for the writers to be halfway.
						for i == contractReaders-1 && progress.Load() < contractWriters*contractOps/2 {
							select {
							case <-stop:
								return
							case <-time.After(time.Millisecond):
							}
						}

						st, err := s.Snapshot(t.Context())
						if err != nil {
							t.Errorf("Snapshot: %v", err)

							return
						}

						f = newFollower(s, st)
					}

					followers[i] = read(t, f, stop)
				})
			}

			for w := range contractWriters {
				writers.Go(func() {
					//nolint:gosec // G404: a failing run's requests have to be reproducible from its seed
					rnd := rand.New(rand.NewPCG(uint64(seed), uint64(w)))

					for range contractOps {
						if err := writeOne(t, s, rnd, &done); err != nil {
							t.Errorf("seed %d, writer %d: %v", seed, w, err)

							return
						}

						progress.Add(1)
					}
				})
			}

			writers.Wait()
			close(stop)
			readers.Wait()

			if t.Failed() {
				return
			}

			final, err := s.Snapshot(t.Context())
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}

			want := newFollower(s, final)
			if v := violation(want); v != "" {
				t.Fatalf("seed %d: the store %s", seed, v)
			}

			for i, f := range append(followers, asleep) {
				if f == nil {
					t.Fatalf("seed %d: reader %d never started", seed, i)
				}

				f.catchUp(t)

				if got, want := f.held(), want.held(); !slices.Equal(got, want) {
					t.Errorf("seed %d: reader %d holds %q; the store holds %q", seed, i, got, want)
				}

				if f.schema != final.Schema {
					t.Errorf("seed %d: reader %d holds schema %q; the store holds %q", seed, i, f.schema, final.Schema)
				}
			}

			t.Logf("seed %d: %s", seed, &done)

			if !done.writes.passed() || !done.large.passed() || !done.deletes.passed() || !done.schemas.passed() {
				t.Errorf("seed %d: the store let through none of one kind of request, so the run tested little", seed)
			}
		})
	}
}

// read follows f's store until stop closes, checking the copy at every revision it reaches.
// It returns f, or nil once it has reported a failure.
func read(t *testing.T, f *follower, stop <-chan struct{}) *follower {
	for {
		if v := violation(f); v != "" {
			t.Errorf("at revision %s, a reader %s", f.rev, v)

			return nil
		}

		select {
		case <-stop:
			return f
		default:
		}

		d, err := f.s.Changes(t.Context(), f.rev)
		if err != nil {
			t.Errorf("Changes: %v", err)

			return nil
		}

		f.advance(d)
	}
}

// violation reports how f's copy breaks the contract at its revision, or "" if it keeps it.
func violation(f *follower) string {
	d := core.NewDictionary()

	compiled, err := schema.Compile("schema", f.schema, d)
	if err != nil {
		return "holds a schema that does not compile: " + err.Error()
	}

	for u := range f.tuples {
		if !accepts(compiled, d, u) {
			return fmt.Sprintf("holds %s, which its schema refuses", u)
		}

		if twin := twinOf(u); !f.tuples[twin] {
			return fmt.Sprintf("holds %s without %s, which was written with it", u, twin)
		}
	}

	return ""
}

// accepts reports whether compiled takes u, asking core directly rather than the store's own checks.
func accepts(compiled *core.Schema, d *core.Dictionary, u Tuple) bool {
	ref := core.RelationRef{Type: d.LookupType(u.ResourceType), Relation: d.LookupRelation(u.Relation)}
	st := core.SubjectType{Type: d.LookupType(u.SubjectType), Wildcard: u.SubjectID == core.WildcardMarker}

	if u.SubjectRelation != "" {
		if st.Relation = d.LookupRelation(u.SubjectRelation); st.Relation == core.NoRelation {
			return false
		}
	}

	return compiled.Allows(ref, st)
}

// twinOf returns the other tuple of u's pair: p0 for q0, and q0 for p0.
func twinOf(u Tuple) Tuple {
	side, n := u.ResourceID[:1], u.ResourceID[1:]
	if side == "p" {
		u.ResourceID = "q" + n
	} else {
		u.ResourceID = "p" + n
	}

	return u
}

// writeOne makes one random request of s, and fails only if s refuses it for a reason the workload cannot cause:
// a schema may have stopped accepting what a request writes or what a filter names, and tuples may remain under what a schema drops.
func writeOne(t *testing.T, s *Store, rnd *rand.Rand, done *tally) error {
	// Of twenty requests: thirteen pair writes, one large write, three filter deletes, three schema changes.
	switch n := rnd.IntN(20); {
	case n < 13:
		return done.writes.record(s.Write(t.Context(), pairUpdates(rnd)), ErrInvalid)

	case n < 14:
		return done.large.record(s.Write(t.Context(), largeUpdates(rnd)), ErrInvalid)

	case n < 17:
		f := filter(contractFilters[rnd.IntN(len(contractFilters))])

		return done.deletes.record(s.DeleteMatching(t.Context(), f), ErrInvalid)

	default:
		wildcard, teams, bans := rnd.IntN(2) == 0, rnd.IntN(2) == 0, rnd.IntN(2) == 0

		// Clear what the schema drops first, though a racing writer may put some back.
		for _, drop := range []struct {
			kept   bool
			filter string
		}{{wildcard, "document#viewer@user:*"}, {teams, "document#viewer@team#member"}, {bans, "document#banned"}} {
			if drop.kept {
				continue
			}

			if err := done.deletes.record(s.DeleteMatching(t.Context(), filter(drop.filter)), ErrInvalid); err != nil {
				return err
			}
		}

		return done.schemas.record(s.WriteSchema(t.Context(), contractSchema(wildcard, teams, bans)), ErrTuplesRemain)
	}
}

// pairUpdates touches or deletes one to three pairs, each as one, so a reader never holds half of one.
func pairUpdates(rnd *rand.Rand) []Update {
	var updates []Update

	picked := make(map[[2]int]bool)

	for range 1 + rnd.IntN(3) {
		pick := [2]int{rnd.IntN(contractPairs), rnd.IntN(len(pairForms))}
		if picked[pick] {
			continue
		}

		picked[pick] = true

		updates = append(updates, pair(randomOp(rnd), pairForms[pick[1]], pick[0])...)
	}

	return updates
}

// largeUpdates touches or deletes contractLargePairs pairs of one form at once:
// more tuples than a store may be willing to lock one by one.
func largeUpdates(rnd *rand.Rand) []Update {
	op, form := randomOp(rnd), pairForms[rnd.IntN(len(pairForms))]

	// One side of every pair, then the other: a request cut short anywhere leaves pairs in half.
	updates := make([]Update, 2*contractLargePairs)
	for n := range contractLargePairs {
		both := pair(op, form, n)
		updates[n], updates[contractLargePairs+n] = both[0], both[1]
	}

	return updates
}

// pair returns op on both tuples of pair n in form.
func pair(op Op, form string, n int) []Update {
	return []Update{
		{op, tuple(fmt.Sprintf(form, "p"+strconv.Itoa(n)))},
		{op, tuple(fmt.Sprintf(form, "q"+strconv.Itoa(n)))},
	}
}

func randomOp(rnd *rand.Rand) Op {
	if rnd.IntN(2) == 0 {
		return Delete
	}

	return Touch
}

// tally counts what the writers sent and what the store refused, so a run shows it put each kind of request to the test.
type tally struct {
	writes, large, deletes, schemas count
}

func (ta *tally) String() string {
	return fmt.Sprintf("writes %s, large writes %s, deletes %s, schemas %s",
		&ta.writes, &ta.large, &ta.deletes, &ta.schemas)
}

type count struct {
	sent, refused atomic.Int64
}

// record counts a request that ended in err, and returns err unless it is the refusal the workload can cause.
func (c *count) record(err, refusal error) error {
	c.sent.Add(1)

	if errors.Is(err, refusal) {
		c.refused.Add(1)

		return nil
	}

	return err
}

// passed reports whether the store let any of the requests through.
func (c *count) passed() bool {
	return c.sent.Load() > c.refused.Load()
}

func (c *count) String() string {
	return fmt.Sprintf("%d (%d refused)", c.sent.Load(), c.refused.Load())
}
