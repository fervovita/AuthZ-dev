package core

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func fixture(t *testing.T) (*Dictionary, Tuple) {
	t.Helper()

	d := NewDictionary()

	obj, err := d.InternObjectRef("document", "d1")
	if err != nil {
		t.Fatalf("InternObjectRef: %v", err)
	}

	rel, err := d.InternRelation("viewer")
	if err != nil {
		t.Fatalf("InternRelation: %v", err)
	}

	subj, err := d.InternSubjectRef("user", "alice", "")
	if err != nil {
		t.Fatalf("InternSubjectRef: %v", err)
	}

	return d, Tuple{Object: obj, Relation: rel, Subject: subj}
}

func TestInternRefRoundTrip(t *testing.T) {
	t.Parallel()

	d, tup := fixture(t)

	if !tup.Valid() {
		t.Fatal("interned tuple is not Valid")
	}

	if got, want := d.FormatTuple(tup), "document:d1#viewer@user:alice"; got != want {
		t.Errorf("FormatTuple = %q; want %q", got, want)
	}
}

// A userset subject is the form the whole model turns on, and its interning path is separate from the direct one.
func TestInternSubjectRefUserset(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	subj, err := d.InternSubjectRef("team", "eng", "member")
	if err != nil {
		t.Fatalf("InternSubjectRef: %v", err)
	}

	if subj.Relation == NoRelation {
		t.Fatal("Relation = NoRelation; the relation argument was dropped")
	}

	if subj.Wildcard {
		t.Error("Wildcard = true; a userset is not a wildcard")
	}

	if !subj.Valid() {
		t.Error("Valid() = false")
	}

	if got, want := d.FormatSubjectRef(subj), "team:eng#member"; got != want {
		t.Errorf("FormatSubjectRef = %q; want %q", got, want)
	}

	// The relation must land in the relation space, not the object space.
	if got := d.LookupRelation("member"); got != subj.Relation {
		t.Errorf("LookupRelation = %d; want %d", got, subj.Relation)
	}

	// A direct subject interned afterwards must not pick the relation up.
	direct, err := d.InternSubjectRef("user", "alice", "")
	if err != nil {
		t.Fatalf("InternSubjectRef: %v", err)
	}

	if direct.Relation != NoRelation {
		t.Errorf("direct subject got relation %d; want NoRelation", direct.Relation)
	}
}

// A sidecar refusing what central accepted would silently drop a permission, so
// ValidSubject is the guard and the central write path applies it.
func TestInternRefDoesNotValidate(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	const odd = "alice@example.com" // '@' would make the string form unparseable

	if ValidSubject("user", odd, "") {
		t.Fatalf("ValidSubject with %q = true; this test needs an id the rules reject", odd)
	}

	subj, err := d.InternSubjectRef("user", odd, "")
	if err != nil {
		t.Fatalf("InternSubjectRef rejected an invalid id: %v", err)
	}

	if !subj.Valid() {
		t.Error("Valid() = false; an odd id still interns to a usable reference")
	}
}

// Left to the caller, a forgotten branch built a subject whose id was the literal
// "*": it printed as user:*, passed Valid, and matched nobody.
func TestInternSubjectRefReadsWildcardMarker(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	subj, err := d.InternSubjectRef("user", WildcardMarker, "")
	if err != nil {
		t.Fatalf("InternSubjectRef: %v", err)
	}

	if !subj.Wildcard {
		t.Fatalf("got %+v; the marker must produce a wildcard, not an ordinary subject", subj)
	}

	if subj.ID != 0 {
		t.Errorf("ID = %d; a wildcard carries no object id", subj.ID)
	}

	if !subj.Valid() {
		t.Error("Valid() = false")
	}

	if got, want := d.FormatSubjectRef(subj), "user:"+WildcardMarker; got != want {
		t.Errorf("FormatSubjectRef = %q; want %q", got, want)
	}

	// Equality-based matching must not tell the two construction paths apart.
	if typ := d.LookupType("user"); subj != WildcardSubject(typ) {
		t.Errorf("%+v != WildcardSubject(%d)", subj, typ)
	}

	// The marker must not have taken an object id of its own.
	if got := d.LookupObject(WildcardMarker); got != 0 {
		t.Errorf("LookupObject(%q) = %d; the marker is not an object", WildcardMarker, got)
	}
}

// a query for user:* resolves to a reference no loader ever stored.
func TestLookupSubjectRefReadsWildcardMarker(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	interned, err := d.InternSubjectRef("user", WildcardMarker, "")
	if err != nil {
		t.Fatalf("InternSubjectRef: %v", err)
	}

	if got := d.LookupSubjectRef("user", WildcardMarker, ""); got != interned {
		t.Errorf("LookupSubjectRef = %+v; want %+v", got, interned)
	}
}

// Interning keeps both so Valid can call the combination unusable, rather than
// dropping the relation silently.
func TestInternSubjectRefWildcardWithRelation(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	subj, err := d.InternSubjectRef("user", WildcardMarker, "member")
	if err != nil {
		t.Fatalf("InternSubjectRef: %v", err)
	}

	if !subj.Wildcard || subj.Relation == NoRelation {
		t.Fatalf("got %+v; both the wildcard and the relation must survive", subj)
	}

	if subj.Valid() {
		t.Error("Valid() = true; a wildcard with a relation is not well-formed")
	}
}

// Valid only tests for nonzero ids, so it protects nothing unless "" keeps resolving to zero.
func TestInternRefEmptyComponentsAreNotValid(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	obj, err := d.InternObjectRef("", "")
	if err != nil {
		t.Fatalf("InternObjectRef: %v", err)
	}

	if obj.Valid() {
		t.Errorf("ObjectRef%+v reports Valid; it formats as %q", obj, d.FormatObjectRef(obj))
	}

	subj, err := d.InternSubjectRef("", "", "")
	if err != nil {
		t.Fatalf("InternSubjectRef: %v", err)
	}

	if subj.Valid() {
		t.Errorf("SubjectRef%+v reports Valid; it formats as %q", subj, d.FormatSubjectRef(subj))
	}
}

// A full ID space has to be reported from whichever leg hits it.
func TestInternRefReportsIDSpaceExhausted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// fill takes the one available slot in the space that should overflow.
		fill   func(*Dictionary) error
		intern func(*Dictionary) error
	}{
		{
			"object ref, type leg",
			func(d *Dictionary) error { _, err := d.InternType("taken"); return err },
			func(d *Dictionary) error { _, err := d.InternObjectRef("overflow", "o"); return err },
		},
		{
			"object ref, object leg",
			func(d *Dictionary) error { _, err := d.InternObject("taken"); return err },
			func(d *Dictionary) error { _, err := d.InternObjectRef("t", "overflow"); return err },
		},
		{
			"subject ref, type leg",
			func(d *Dictionary) error { _, err := d.InternType("taken"); return err },
			func(d *Dictionary) error { _, err := d.InternSubjectRef("overflow", "o", "r"); return err },
		},
		{
			"subject ref, object leg",
			func(d *Dictionary) error { _, err := d.InternObject("taken"); return err },
			func(d *Dictionary) error { _, err := d.InternSubjectRef("t", "overflow", "r"); return err },
		},
		{
			"subject ref, relation leg",
			func(d *Dictionary) error { _, err := d.InternRelation("taken"); return err },
			func(d *Dictionary) error { _, err := d.InternSubjectRef("t", "o", "overflow"); return err },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			d := newDictionary(1) // room for exactly one entry

			if err := c.fill(d); err != nil {
				t.Fatalf("filling the space: %v", err)
			}

			if err := c.intern(d); !errors.Is(err, ErrIDSpaceExhausted) {
				t.Errorf("err = %v; want ErrIDSpaceExhausted", err)
			}
		})
	}
}

func TestLookupRefMatchesIntern(t *testing.T) {
	t.Parallel()

	d, tup := fixture(t)

	if got := d.LookupObjectRef("document", "d1"); got != tup.Object {
		t.Errorf("LookupObjectRef = %+v; want %+v", got, tup.Object)
	}

	if got := d.LookupSubjectRef("user", "alice", ""); got != tup.Subject {
		t.Errorf("LookupSubjectRef = %+v; want %+v", got, tup.Subject)
	}
}

// Why Wildcard is its own field: an unresolvable name must match nothing, never everything.
func TestLookupUnknownIsNotWildcard(t *testing.T) {
	t.Parallel()

	d, _ := fixture(t)

	s := d.LookupSubjectRef("user", "nobody", "")

	if s.Wildcard {
		t.Fatal("an unknown subject came back as a wildcard")
	}

	if s.ID != 0 {
		t.Errorf("ID = %d; want 0 for an unknown name", s.ID)
	}

	if s.Valid() {
		t.Error("Valid() = true; an unresolvable subject must not be valid")
	}

	if s.Type == 0 {
		t.Error("Type = 0; the type was interned and should resolve")
	}
}

func TestLookupRefDoesNotGrow(t *testing.T) {
	t.Parallel()

	// One entry already used by "real"; one slot left.
	d := newDictionary(2)

	if _, err := d.InternObject("real"); err != nil {
		t.Fatalf("InternObject: %v", err)
	}

	for i := range 500 {
		d.LookupObjectRef("attacker-t"+strconv.Itoa(i), "attacker-o"+strconv.Itoa(i))
		d.LookupSubjectRef("attacker-t"+strconv.Itoa(i), "attacker-o"+strconv.Itoa(i), "attacker-r")
	}

	if _, err := d.InternObject("also-real"); err != nil {
		t.Fatalf("lookups consumed the interning budget: %v", err)
	}
}

func TestFormat(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	team, _ := d.InternType("team")
	user, _ := d.InternType("user")
	eng, _ := d.InternObject("eng")
	member, _ := d.InternRelation("member")

	cases := []struct {
		name string
		ref  SubjectRef
		want string
	}{
		{"direct", SubjectRef{Type: user, ID: eng}, "user:eng"},
		{"userset", SubjectRef{Type: team, ID: eng, Relation: member}, "team:eng#member"},
		{"wildcard", WildcardSubject(user), "user:*"},
		{"unresolved object", SubjectRef{Type: user, ID: 0}, "user:?"},
		{"dangling", SubjectRef{Type: 9999, ID: 9999}, "!dangling(9999):!dangling(9999)"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := d.FormatSubjectRef(c.ref); got != c.want {
				t.Errorf("FormatSubjectRef = %q; want %q", got, c.want)
			}
		})
	}
}

// Proof trees and error paths format unresolvable references; it must not panic.
func TestFormatDanglingTuple(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	tup := Tuple{
		Object:   ObjectRef{Type: 1, ID: 2},
		Relation: 3,
		Subject:  SubjectRef{Type: 4, ID: 5},
	}

	want := "!dangling(1):!dangling(2)#!dangling(3)@!dangling(4):!dangling(5)"
	if got := d.FormatTuple(tup); got != want {
		t.Errorf("FormatTuple = %q; want %q", got, want)
	}
}

// An unseen name is not a dangling id.
func TestFormatZeroIsNotDangling(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	if _, err := d.InternType("document"); err != nil {
		t.Fatalf("InternType: %v", err)
	}

	ref := d.LookupObjectRef("document", "no-such-object")

	got := d.FormatObjectRef(ref)
	if got != "document:?" {
		t.Errorf("FormatObjectRef = %q; want %q", got, "document:?")
	}

	if strings.Contains(got, danglingName) {
		t.Errorf("FormatObjectRef = %q; an unseen name is not a dangling id", got)
	}
}

// Replication may be writing while a query resolves.
func TestRefConcurrentAccess(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	const workers, iterations = 4, 200

	var wg sync.WaitGroup

	for w := range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			prefix := "w" + strconv.Itoa(w) + "-"

			for i := range iterations {
				key := prefix + strconv.Itoa(i)

				if _, err := d.InternObjectRef(key, key); err != nil {
					t.Errorf("InternObjectRef: %v", err)
					return
				}

				if _, err := d.InternSubjectRef(key, key, key); err != nil {
					t.Errorf("InternSubjectRef: %v", err)
					return
				}
			}
		}()
	}

	for range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range iterations {
				key := "w0-" + strconv.Itoa(i)

				obj := d.LookupObjectRef(key, key)
				subj := d.LookupSubjectRef(key, key, key)

				d.FormatObjectRef(obj)
				d.FormatSubjectRef(subj)
			}
		}()
	}

	wg.Wait()
}

// ObjectRef has no Wildcard field to hold one, so the marker is an ordinary object id here.
// The asymmetry with InternSubjectRef is deliberate; this pins it.
func TestInternObjectRefDoesNotReadWildcardMarker(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	obj, err := d.InternObjectRef("document", WildcardMarker)
	if err != nil {
		t.Fatalf("InternObjectRef: %v", err)
	}

	if obj.ID == 0 {
		t.Fatal("ID = 0; the marker must intern as an ordinary object id here")
	}

	// The loader's gate is what rejects it, not the dictionary.
	if ValidObject("document", WildcardMarker) {
		t.Error("ValidObject accepts the marker; a resource has no wildcard")
	}
}

func TestFormatShowsWildcardRelation(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	malformed, err := d.InternSubjectRef("user", WildcardMarker, "member")
	if err != nil {
		t.Fatalf("InternSubjectRef: %v", err)
	}

	plain, err := d.InternSubjectRef("user", WildcardMarker, "")
	if err != nil {
		t.Fatalf("InternSubjectRef: %v", err)
	}

	if malformed.Valid() {
		t.Fatal("Valid() = true; a wildcard with a relation is not well-formed")
	}

	if got, want := d.FormatSubjectRef(malformed), "user:*#member"; got != want {
		t.Errorf("FormatSubjectRef = %q; want %q", got, want)
	}

	if got := d.FormatSubjectRef(plain); got == d.FormatSubjectRef(malformed) {
		t.Errorf("both render as %q; the malformed one has to look malformed", got)
	}
}
