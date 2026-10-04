package postgres

import (
	"fmt"
	"slices"
	"testing"
)

// touchMany returns a request that touches relation on n documents.
func touchMany(relation string, n int) []Update {
	out := make([]Update, n)
	for i := range out {
		out[i] = Update{Touch, Tuple{"document", fmt.Sprint("d", i), relation, "user", "alice", ""}}
	}

	return out
}

func TestWriteLocksEachTupleAndItsRelation(t *testing.T) {
	t.Parallel()

	alice := Tuple{"document", "d1", "viewer", "user", "alice", ""}
	eng := Tuple{"team", "eng", "member", "user", "bob", ""}

	got := ordered(writeLocks([]Update{{Touch, alice}, {Delete, eng}}))
	want := ordered([]lock{
		schemaLock(false),
		relationLock("document", "viewer", false), tupleLock(alice),
		relationLock("team", "member", false), tupleLock(eng),
	})

	if !slices.Equal(got, want) {
		t.Errorf("writeLocks = %v; want %v", got, want)
	}
}

// Past maxTupleLocks a request takes one lock per relation, held alone, in place of one per tuple.
func TestWriteLocksWholeRelationsPastTheLimit(t *testing.T) {
	t.Parallel()

	if got := ordered(writeLocks(touchMany("viewer", maxTupleLocks))); len(got) != maxTupleLocks+2 {
		t.Errorf("writeLocks of %d tuples took %d locks; want the schema, the relation and each tuple", maxTupleLocks, len(got))
	}

	got := ordered(writeLocks(touchMany("viewer", maxTupleLocks+1)))
	want := ordered([]lock{schemaLock(false), relationLock("document", "viewer", true)})

	if !slices.Equal(got, want) {
		t.Errorf("writeLocks of %d tuples = %v; want the relation whole", maxTupleLocks+1, got)
	}
}

// A name asked for twice is taken once, exclusively if either asked so, and names come in one order.
func TestOrdered(t *testing.T) {
	t.Parallel()

	got := ordered([]lock{{"c", false}, {"a", true}, {"b", true}, {"a", false}, {"c", false}})
	want := []lock{{"a", true}, {"b", true}, {"c", false}}

	if !slices.Equal(got, want) {
		t.Errorf("ordered = %v; want %v", got, want)
	}
}
