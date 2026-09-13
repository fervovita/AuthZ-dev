package core

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestSchemaBuildAndLookup(t *testing.T) {
	t.Parallel()

	s := validSchema(t)

	rw, ok := s.Rewrite(docRel(rView))
	if !ok {
		t.Fatal("view is not defined")
	}

	if rw.Op != OpExclusion {
		t.Errorf("Op = %s; want exclusion", rw.Op)
	}

	if _, ok := s.Rewrite(docRel(rMember)); ok {
		t.Error("document:member resolved; relations are per type")
	}
}

func TestSchemaBuildRejectsShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		def  func(*SchemaBuilder) *SchemaBuilder
		want error
	}{
		{
			"exclusion with one operand",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Permission(docRel(rView), Rewrite{
					Op: OpExclusion, Children: []Rewrite{ComputedUserset(rViewer)},
				})
			},
			ErrSchemaInvalid,
		},
		{
			"union with one operand",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Permission(docRel(rView), Union(ComputedUserset(rViewer)))
			},
			ErrSchemaInvalid,
		},
		{
			"this with operands",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Permission(docRel(rView), Rewrite{Op: OpThis, Children: []Rewrite{This()}})
			},
			ErrSchemaInvalid,
		},
		{
			"this as an operand of a permission",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Permission(docRel(rView), Union(ComputedUserset(rViewer), This()))
			},
			ErrSchemaInvalid,
		},
		{
			"computed_userset without a relation",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Permission(docRel(rView), Rewrite{Op: OpComputedUserset})
			},
			ErrSchemaInvalid,
		},
		{
			"reference to an undefined relation",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Permission(docRel(rView), ComputedUserset(rEditor))
			},
			ErrSchemaInvalid,
		},
		{
			"unknown operator",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Permission(docRel(rView), Rewrite{Op: Op(99)})
			},
			ErrSchemaInvalid,
		},
		{
			"tuple_to_userset",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Permission(docRel(rView), TupleToUserset(rOwner, rViewer))
			},
			ErrUnsupportedOp,
		},
		{
			"computed_userset with operands",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Permission(docRel(rView), Rewrite{
					Op: OpComputedUserset, Relation: rViewer, Children: []Rewrite{ComputedUserset(rOwner)},
				})
			},
			ErrSchemaInvalid,
		},
		{
			"union with a relation",
			func(b *SchemaBuilder) *SchemaBuilder {
				rw := Union(ComputedUserset(rViewer), ComputedUserset(rOwner))
				rw.Relation = rOwner

				return b.Permission(docRel(rView), rw)
			},
			ErrSchemaInvalid,
		},
		{
			"exclusion with a tupleset",
			func(b *SchemaBuilder) *SchemaBuilder {
				rw := Exclusion(ComputedUserset(rViewer), ComputedUserset(rOwner))
				rw.Tupleset = rOwner

				return b.Permission(docRel(rView), rw)
			},
			ErrSchemaInvalid,
		},
		{
			"this with a tupleset",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Permission(docRel(rView), Rewrite{Op: OpThis, Tupleset: rOwner})
			},
			ErrSchemaInvalid,
		},
		{
			"relation accepting nothing",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Relation(docRel(rView))
			},
			ErrSchemaInvalid,
		},
		{
			"subject type that did not resolve",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Relation(docRel(rView), SubjectType{})
			},
			ErrSchemaInvalid,
		},
		{
			"wildcard carrying a relation",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Relation(docRel(rView), SubjectType{Type: tDoc, Relation: rViewer, Wildcard: true})
			},
			ErrSchemaInvalid,
		},
		{
			"subject type listed twice",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Relation(docRel(rView), DirectType(tUser), DirectType(tUser))
			},
			ErrSchemaInvalid,
		},
		{
			"accepting a userset on an undefined relation",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Relation(docRel(rView), UsersetType(tTeam, rMember))
			},
			ErrSchemaInvalid,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			b := NewSchemaBuilder().
				Relation(docRel(rViewer), DirectType(tUser)).
				Relation(docRel(rOwner), DirectType(tUser))

			if _, err := c.def(b).Build(); !errors.Is(err, c.want) {
				t.Errorf("err = %v; want %v", err, c.want)
			}
		})
	}
}

func TestSchemaBuildRejectsDuplicateDefinition(t *testing.T) {
	t.Parallel()

	t.Run("twice as a relation", func(t *testing.T) {
		t.Parallel()

		_, err := NewSchemaBuilder().
			Relation(docRel(rViewer), DirectType(tUser)).
			Relation(docRel(rViewer), DirectType(tUser)).
			Build()

		if !errors.Is(err, ErrSchemaInvalid) {
			t.Errorf("err = %v; want ErrSchemaInvalid", err)
		}
	})

	// The second call must not leave the first's accepted types attached to a rewrite that
	// no longer reads them.
	t.Run("as a relation then a permission", func(t *testing.T) {
		t.Parallel()

		_, err := NewSchemaBuilder().
			Relation(docRel(rViewer), DirectType(tUser)).
			Permission(docRel(rViewer), ComputedUserset(rViewer)).
			Build()

		if !errors.Is(err, ErrSchemaInvalid) {
			t.Errorf("err = %v; want ErrSchemaInvalid", err)
		}
	})
}

func TestSchemaRejectsRecursionThroughExclusion(t *testing.T) {
	t.Parallel()

	t.Run("mutual through subtract", func(t *testing.T) {
		t.Parallel()

		// view = viewer - banned.
		// banned = view.
		_, err := NewSchemaBuilder().
			Relation(docRel(rViewer), DirectType(tUser)).
			Permission(docRel(rView), Exclusion(ComputedUserset(rViewer), ComputedUserset(rBanned))).
			Permission(docRel(rBanned), ComputedUserset(rView)).
			Build()

		if !errors.Is(err, ErrSchemaInvalid) {
			t.Fatalf("err = %v; want ErrSchemaInvalid", err)
		}

		if !strings.Contains(err.Error(), "reaches back") {
			t.Errorf("err = %q; want it to name the cycle", err)
		}
	})

	// The loop the rewrites do not show: banned accepts a userset on view, so a stored tuple
	// can make banned depend on the permission that subtracts it, leaving view = NOT view.
	t.Run("through a relation's own accepted types", func(t *testing.T) {
		t.Parallel()

		_, err := NewSchemaBuilder().
			Relation(docRel(rViewer), DirectType(tUser)).
			Relation(docRel(rBanned), DirectType(tUser), UsersetType(tDoc, rView)).
			Permission(docRel(rView), Exclusion(ComputedUserset(rViewer), ComputedUserset(rBanned))).
			Build()

		if !errors.Is(err, ErrSchemaInvalid) {
			t.Fatalf("err = %v; want ErrSchemaInvalid", err)
		}

		if !strings.Contains(err.Error(), "reaches back") {
			t.Errorf("err = %q; want it to name the cycle", err)
		}
	})
}

// Recursion itself is fine; only recursion through a subtract is not.
func TestSchemaAcceptsPositiveRecursion(t *testing.T) {
	t.Parallel()

	_, err := NewSchemaBuilder().
		Relation(docRel(rViewer), DirectType(tUser), UsersetType(tDoc, rEditor)).
		Relation(docRel(rEditor), DirectType(tUser), UsersetType(tDoc, rViewer)).
		Build()
	if err != nil {
		t.Errorf("Build: %v; positive recursion is legal", err)
	}
}

// Placement forces these local and ties their lag to readiness, so the set has to
// close over everything the subtract side can reach, not just its first hop.
func TestSchemaExclusionReachableIsTransitive(t *testing.T) {
	t.Parallel()

	// view = viewer - banned, and banned = owner.
	s, err := NewSchemaBuilder().
		Relation(docRel(rViewer), DirectType(tUser)).
		Relation(docRel(rOwner), DirectType(tUser)).
		Permission(docRel(rBanned), ComputedUserset(rOwner)).
		Permission(docRel(rView), Exclusion(ComputedUserset(rViewer), ComputedUserset(rBanned))).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got := s.ExclusionReachable()
	want := []RelationRef{docRel(rOwner), docRel(rBanned)}

	if len(got) != len(want) {
		t.Fatalf("ExclusionReachable = %+v; want %+v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ExclusionReachable = %+v; want %+v", got, want)

			break
		}
	}

	// The positive side is not in the set.
	for _, ref := range got {
		if ref == docRel(rViewer) {
			t.Error("viewer is on the positive side and must not be listed")
		}
	}
}

// The subtract side also reaches through what a relation's tuples may name, not only rewrites.
func TestSchemaExclusionReachableFollowsAcceptedTypes(t *testing.T) {
	t.Parallel()

	// view = viewer - banned; a banned tuple may name a group, whose members name another.
	s, err := NewSchemaBuilder().
		Relation(docRel(rViewer), DirectType(tUser)).
		Relation(docRel(rBanned), DirectType(tUser), UsersetType(tTeam, rMember)).
		Relation(teamRel(rMember), DirectType(tUser), UsersetType(tTeam, rOwner)).
		Relation(teamRel(rOwner), DirectType(tUser)).
		Permission(docRel(rView), Exclusion(ComputedUserset(rViewer), ComputedUserset(rBanned))).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	want := []RelationRef{docRel(rBanned), teamRel(rOwner), teamRel(rMember)}
	if got := s.ExclusionReachable(); !slices.Equal(got, want) {
		t.Errorf("ExclusionReachable = %+v; want %+v", got, want)
	}
}

func TestSchemaExclusionReachableIsACopy(t *testing.T) {
	t.Parallel()

	s := validSchema(t)

	got := s.ExclusionReachable()
	if len(got) == 0 {
		t.Fatal("no exclusion reachable relations to test with")
	}

	got[0] = RelationRef{Type: 99, Relation: 99}

	if s.ExclusionReachable()[0] == got[0] {
		t.Error("the caller mutated the schema's own slice")
	}
}

// Build freezes the schema, so the tree a caller passed to Permission is not a way back in.
func TestSchemaBuildCopiesTheRewriteTree(t *testing.T) {
	t.Parallel()

	rw := Union(ComputedUserset(rViewer), ComputedUserset(rOwner))

	s, err := NewSchemaBuilder().
		Relation(docRel(rViewer), DirectType(tUser)).
		Relation(docRel(rOwner), DirectType(tUser)).
		Permission(docRel(rView), rw).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	rw.Children[0] = ComputedUserset(rBanned)

	got, ok := s.Rewrite(docRel(rView))
	if !ok {
		t.Fatal("view is not defined")
	}

	if got.Children[0].Relation != rViewer {
		t.Errorf("the schema's first operand became %d; the caller reached in", got.Children[0].Relation)
	}
}

// A relation accepts what it was given; a permission accepts nothing.
func TestSchemaAllows(t *testing.T) {
	t.Parallel()

	s := validSchema(t)

	for _, c := range []struct {
		name string
		ref  RelationRef
		st   SubjectType
		want bool
	}{
		{"listed direct type", docRel(rViewer), DirectType(tUser), true},
		{"listed wildcard", docRel(rViewer), WildcardType(tUser), true},
		{"listed userset", docRel(rViewer), UsersetType(tTeam, rMember), true},
		{"wildcard of a type only accepted directly", docRel(rOwner), WildcardType(tUser), false},
		{"userset where only the object type is accepted", docRel(rOwner), UsersetType(tUser, rMember), false},
		{"another relation's list", teamRel(rMember), DirectType(tTeam), false},
		{"a permission", docRel(rView), DirectType(tUser), false},
		{"an undefined relation", docRel(rEditor), DirectType(tUser), false},
	} {
		if got := s.Allows(c.ref, c.st); got != c.want {
			t.Errorf("%s: Allows(%+v, %+v) = %v; want %v", c.name, c.ref, c.st, got, c.want)
		}
	}
}

// Build freezes the schema, so the slice a caller passed to Relation is not a way back in.
func TestSchemaRelationCopiesTheAcceptedTypes(t *testing.T) {
	t.Parallel()

	accepted := []SubjectType{DirectType(tUser), UsersetType(tTeam, rMember)}

	s, err := NewSchemaBuilder().
		Relation(docRel(rViewer), accepted...).
		Relation(teamRel(rMember), DirectType(tUser)).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	accepted[0] = DirectType(tTeam)

	if !s.Allows(docRel(rViewer), DirectType(tUser)) {
		t.Error("the caller reached in and replaced an accepted type")
	}

	if s.Allows(docRel(rViewer), DirectType(tTeam)) {
		t.Error("the caller reached in and added an accepted type")
	}
}

func TestOpString(t *testing.T) {
	t.Parallel()

	cases := map[Op]string{
		OpThis:            "this",
		OpComputedUserset: "computed_userset",
		OpTupleToUserset:  "tuple_to_userset",
		OpUnion:           "union",
		OpIntersection:    "intersection",
		OpExclusion:       "exclusion",
		Op(99):            "op(99)",
	}

	for op, want := range cases {
		if got := op.String(); got != want {
			t.Errorf("Op(%d).String() = %q; want %q", op, got, want)
		}
	}
}
