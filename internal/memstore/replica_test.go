package memstore

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/fervovita/AuthZ-dev/internal/core"
	"github.com/fervovita/AuthZ-dev/internal/watch"
)

// Every key a tuple of the test can land on, and every subject it can name.
var (
	modelKeys     = []string{"document:d1#viewer", "document:d2#viewer", "document:d1#member", "team:t1#member", "team:t2#member"}
	modelSubjects = []string{
		"user:u1", "user:u2", "user:u3", "user:*",
		"team:t1", "team:t2", "team:t1#member", "team:t1#viewer", "team:t2#member", "team:*",
	}
)

// After any run of resets and changes, the replica answers each question the evaluator asks as the plain set of
// tuples would, and holds a key only while a tuple is on it.
func TestReplicaMatchesASetOfTuples(t *testing.T) {
	t.Parallel()

	var universe []watch.Tuple

	for _, k := range modelKeys {
		for _, s := range modelSubjects {
			universe = append(universe, tupleOf(t, k+"@"+s))
		}
	}

	for seed := range 20 {
		//nolint:gosec // G404: a failing run has to be reproducible from its seed
		rnd := rand.New(rand.NewPCG(uint64(seed), 0))
		held := make(map[watch.Tuple]bool)

		var start []watch.Tuple

		for _, tp := range universe {
			if rnd.IntN(3) == 0 {
				held[tp] = true
				start = append(start, tp)
			}
		}

		var s Store

		if err := s.Reset(watch.Revision("r0"), "", start); err != nil {
			t.Fatalf("seed %d: Reset: %v", seed, err)
		}

		assertHolds(t, fmt.Sprintf("seed %d, reset", seed), s.current, held)

		for step := range 30 {
			var changes []watch.Change

			// Each tuple at most once, as a change list carries it, and as often absent as present.
			for _, i := range rnd.Perm(len(universe))[:1+rnd.IntN(8)] {
				c := watch.Change{Tuple: universe[i], Present: rnd.IntN(2) == 0}
				changes = append(changes, c)

				if c.Present {
					held[c.Tuple] = true
				} else {
					delete(held, c.Tuple)
				}
			}

			if err := s.Apply(watch.Revision(fmt.Sprint(step)), nil, changes); err != nil {
				t.Fatalf("seed %d, step %d: Apply: %v", seed, step, err)
			}

			assertHolds(t, fmt.Sprintf("seed %d, step %d", seed, step), s.current, held)
		}
	}
}

// assertHolds asks r every question about every key and subject of the universe, and compares the answers with held.
func assertHolds(t *testing.T, at string, r *replica, held map[watch.Tuple]bool) {
	t.Helper()

	ctx := t.Context()
	keys := 0

	for _, k := range modelKeys {
		var wantSubjects, wantUsersets []string

		wantWildcards := make(map[string]bool)

		for _, s := range modelSubjects {
			tp := tupleOf(t, k+"@"+s)

			switch {
			case !held[tp]:
			case tp.SubjectID == core.WildcardMarker:
				wantWildcards[tp.SubjectType] = true
			case tp.SubjectRelation != "":
				wantUsersets = append(wantUsersets, s)
				wantSubjects = append(wantSubjects, s)
			default:
				wantSubjects = append(wantSubjects, s)
			}
		}

		if len(wantSubjects) > 0 || len(wantWildcards) > 0 {
			keys++
		}

		probe := tupleOf(t, k+"@user:u1")
		obj := r.names.LookupObjectRef(probe.ResourceType, probe.ResourceID)
		rel := r.names.LookupRelation(probe.Relation)

		if !obj.Valid() || rel == core.NoRelation {
			if len(wantSubjects) > 0 || len(wantWildcards) > 0 {
				t.Errorf("%s: %s holds tuples but its names were never interned", at, k)
			}

			continue
		}

		var gotSubjects []string

		if err := r.Subjects(ctx, []core.SubjectsRequest{{Object: obj, Relation: rel}}, func(_ int, subjects []core.SubjectRef) bool {
			for _, s := range subjects {
				gotSubjects = append(gotSubjects, r.names.FormatSubjectRef(s))
			}

			return true
		}); err != nil {
			t.Fatalf("%s: Subjects: %v", at, err)
		}

		if got, want := sorted(gotSubjects), sorted(wantSubjects); !slices.Equal(got, want) {
			t.Errorf("%s: Subjects(%s) = %q; want %q", at, k, got, want)
		}

		for _, typ := range []string{"user", "team"} {
			out := make([]bool, 1)

			req := core.WildcardRequest{Object: obj, Relation: rel, SubjectType: r.names.LookupType(typ)}
			if err := r.HasWildcard(ctx, []core.WildcardRequest{req}, out); err != nil {
				t.Fatalf("%s: HasWildcard: %v", at, err)
			}

			if out[0] != wantWildcards[typ] {
				t.Errorf("%s: HasWildcard(%s, %s) = %v; want %v", at, k, typ, out[0], wantWildcards[typ])
			}
		}

		for _, s := range modelSubjects {
			tp := tupleOf(t, k+"@"+s)
			subj := r.names.LookupSubjectRef(tp.SubjectType, tp.SubjectID, tp.SubjectRelation)

			var (
				direct   bool
				usersets []string
			)

			if err := r.Lookup(ctx, []core.LookupRequest{{Object: obj, Relation: rel, Subject: subj}}, func(_ int, res core.LookupResult) bool {
				direct = res.Direct

				for _, us := range res.Usersets {
					usersets = append(usersets, r.names.FormatSubjectRef(us))
				}

				return true
			}); err != nil {
				t.Fatalf("%s: Lookup: %v", at, err)
			}

			// A wildcard is never held directly: HasWildcard answers for it.
			if want := held[tp] && tp.SubjectID != core.WildcardMarker; direct != want {
				t.Errorf("%s: Lookup(%s@%s).Direct = %v; want %v", at, k, s, direct, want)
			}

			// Sorted by name here: the replica sorts by interned ID, which only its own searches rely on.
			if got, want := sorted(usersets), sorted(wantUsersets); !slices.Equal(got, want) {
				t.Errorf("%s: Lookup(%s@%s).Usersets = %q; want %q", at, k, s, got, want)
			}
		}
	}

	if len(r.entries) != keys {
		t.Errorf("%s: the replica keeps %d keys; %d hold a tuple", at, len(r.entries), keys)
	}
}

func sorted(s []string) []string {
	return slices.Sorted(slices.Values(s))
}
