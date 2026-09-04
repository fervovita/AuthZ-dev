package core

import "testing"

func TestObjectRefValid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ref  ObjectRef
		want bool
	}{
		{"both set", ObjectRef{Type: 1, ID: 2}, true},
		{"zero type", ObjectRef{Type: 0, ID: 2}, false},
		{"zero id", ObjectRef{Type: 1, ID: 0}, false},
		{"zero value", ObjectRef{}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := c.ref.Valid(); got != c.want {
				t.Errorf("Valid() = %v; want %v", got, c.want)
			}
		})
	}
}

func TestSubjectRefValid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ref  SubjectRef
		want bool
	}{
		{"direct", SubjectRef{Type: 1, ID: 2}, true},
		{"userset", SubjectRef{Type: 1, ID: 2, Relation: 3}, true},
		{"wildcard", SubjectRef{Type: 1, Wildcard: true}, true},
		{"zero type", SubjectRef{ID: 2}, false},
		{"zero id", SubjectRef{Type: 1}, false},
		{"wildcard with id", SubjectRef{Type: 1, ID: 2, Wildcard: true}, false},
		{"wildcard with relation", SubjectRef{Type: 1, Relation: 3, Wildcard: true}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := c.ref.Valid(); got != c.want {
				t.Errorf("Valid() = %v; want %v", got, c.want)
			}
		})
	}
}

func TestTupleValid(t *testing.T) {
	t.Parallel()

	obj := ObjectRef{Type: 1, ID: 2}
	subj := SubjectRef{Type: 3, ID: 4}

	cases := []struct {
		name  string
		tuple Tuple
		want  bool
	}{
		{"complete", Tuple{Object: obj, Relation: 5, Subject: subj}, true},
		{"no relation", Tuple{Object: obj, Relation: NoRelation, Subject: subj}, false},
		{"bad object", Tuple{Object: ObjectRef{}, Relation: 5, Subject: subj}, false},
		{"bad subject", Tuple{Object: obj, Relation: 5, Subject: SubjectRef{}}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := c.tuple.Valid(); got != c.want {
				t.Errorf("Valid() = %v; want %v", got, c.want)
			}
		})
	}
}

func TestWildcardSubject(t *testing.T) {
	t.Parallel()

	s := WildcardSubject(7)

	if !s.Wildcard {
		t.Error("Wildcard = false; want true")
	}

	if s.ID != 0 || s.Relation != NoRelation {
		t.Errorf("got ID %d relation %d; a wildcard carries neither", s.ID, s.Relation)
	}

	if !s.Valid() {
		t.Error("Valid() = false; want true")
	}
}
