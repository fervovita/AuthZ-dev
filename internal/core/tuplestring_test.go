package core

import (
	"strings"
	"testing"
)

func loadTuples(t *testing.T, d *Dictionary, lines []string) *stubSource {
	t.Helper()

	b := store()

	for _, line := range lines {
		obj, rel, subj := parseTuple(t, d, line)
		b.add(obj, rel, subj)
	}

	return b.source()
}

// parseTuple reads "type:id#relation@subject", the form FormatTuple writes.
// Identifiers exclude ':' '#' and '@', so the first occurrence of each is the separator.
func parseTuple(t *testing.T, d *Dictionary, s string) (ObjectRef, RelationID, SubjectRef) {
	t.Helper()

	at := strings.Index(s, "@")
	if at < 0 {
		t.Fatalf("%q has no subject", s)
	}

	left, right := s[:at], s[at+1:]

	hash := strings.Index(left, "#")
	if hash < 0 {
		t.Fatalf("%q has no relation", s)
	}

	objType, objID := splitRef(t, left[:hash])
	relName := left[hash+1:]

	subjType, subjID, subjRel := splitSubject(t, right)

	if !ValidObject(objType, objID) {
		t.Fatalf("%q: %s:%s is not a legal resource", s, objType, objID)
	}

	if !ValidRelation(relName) {
		t.Fatalf("%q: %s is not a legal relation", s, relName)
	}

	if !ValidSubject(subjType, subjID, subjRel) {
		t.Fatalf("%q: the subject is not legal", s)
	}

	obj, err := d.InternObjectRef(objType, objID)
	if err != nil {
		t.Fatalf("%q: %v", s, err)
	}

	rel, err := d.InternRelation(relName)
	if err != nil {
		t.Fatalf("%q: %v", s, err)
	}

	subj, err := d.InternSubjectRef(subjType, subjID, subjRel)
	if err != nil {
		t.Fatalf("%q: %v", s, err)
	}

	return obj, rel, subj
}

func splitRef(t *testing.T, s string) (typ, id string) {
	t.Helper()

	colon := strings.Index(s, ":")
	if colon < 0 {
		t.Fatalf("%q is not type:id", s)
	}

	return s[:colon], s[colon+1:]
}

func splitSubject(t *testing.T, s string) (typ, id, rel string) {
	t.Helper()

	if before, after, ok := strings.Cut(s, "#"); ok {
		typ, id = splitRef(t, before)

		return typ, id, after
	}

	typ, id = splitRef(t, s)

	return typ, id, ""
}

// The case format writes tuples the way FormatTuple does, so a round trip has to
// come back unchanged or the cases are describing something else.
func TestYAMLTupleFormRoundTrips(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	for _, want := range []string{
		"document:d1#viewer@user:alice",
		"document:d1#viewer@team:eng#member",
		"document:d1#viewer@user:*",
	} {
		obj, rel, subj := parseTuple(t, d, want)

		if got := d.FormatTuple(Tuple{Object: obj, Relation: rel, Subject: subj}); got != want {
			t.Errorf("round trip = %q; want %q", got, want)
		}
	}
}
