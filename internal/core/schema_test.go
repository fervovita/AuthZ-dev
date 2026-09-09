package core

import (
	"errors"
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
				return b.Define(docRel(rView), Rewrite{Op: OpExclusion, Children: []Rewrite{This()}})
			},
			ErrSchemaInvalid,
		},
		{
			"union with one operand",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Define(docRel(rView), Union(This()))
			},
			ErrSchemaInvalid,
		},
		{
			"this with operands",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Define(docRel(rView), Rewrite{Op: OpThis, Children: []Rewrite{This()}})
			},
			ErrSchemaInvalid,
		},
		{
			"computed_userset without a relation",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Define(docRel(rView), Rewrite{Op: OpComputedUserset})
			},
			ErrSchemaInvalid,
		},
		{
			"reference to an undefined relation",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Define(docRel(rView), ComputedUserset(rEditor))
			},
			ErrSchemaInvalid,
		},
		{
			"unknown operator",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Define(docRel(rView), Rewrite{Op: Op(99)})
			},
			ErrSchemaInvalid,
		},
		{
			"tuple_to_userset",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Define(docRel(rView), TupleToUserset(rOwner, rViewer))
			},
			ErrUnsupportedOp,
		},
		{
			"computed_userset with operands",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Define(docRel(rView), Rewrite{
					Op: OpComputedUserset, Relation: rViewer, Children: []Rewrite{This()},
				})
			},
			ErrSchemaInvalid,
		},
		{
			"union with a relation",
			func(b *SchemaBuilder) *SchemaBuilder {
				rw := Union(ComputedUserset(rViewer), ComputedUserset(rOwner))
				rw.Relation = rOwner

				return b.Define(docRel(rView), rw)
			},
			ErrSchemaInvalid,
		},
		{
			"exclusion with a tupleset",
			func(b *SchemaBuilder) *SchemaBuilder {
				rw := Exclusion(ComputedUserset(rViewer), ComputedUserset(rOwner))
				rw.Tupleset = rOwner

				return b.Define(docRel(rView), rw)
			},
			ErrSchemaInvalid,
		},
		{
			"this with a tupleset",
			func(b *SchemaBuilder) *SchemaBuilder {
				return b.Define(docRel(rView), Rewrite{Op: OpThis, Tupleset: rOwner})
			},
			ErrSchemaInvalid,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			b := NewSchemaBuilder().Define(docRel(rViewer), This()).Define(docRel(rOwner), This())

			if _, err := c.def(b).Build(); !errors.Is(err, c.want) {
				t.Errorf("err = %v; want %v", err, c.want)
			}
		})
	}
}

func TestSchemaBuildRejectsDuplicateDefine(t *testing.T) {
	t.Parallel()

	_, err := NewSchemaBuilder().
		Define(docRel(rViewer), This()).
		Define(docRel(rViewer), This()).
		Build()

	if !errors.Is(err, ErrSchemaInvalid) {
		t.Errorf("err = %v; want ErrSchemaInvalid", err)
	}
}

func TestSchemaRejectsRecursionThroughExclusion(t *testing.T) {
	t.Parallel()

	t.Run("mutual through subtract", func(t *testing.T) {
		t.Parallel()

		// view = viewer - banned.
		// banned = view.
		_, err := NewSchemaBuilder().
			Define(docRel(rViewer), This()).
			Define(docRel(rView), Exclusion(ComputedUserset(rViewer), ComputedUserset(rBanned))).
			Define(docRel(rBanned), ComputedUserset(rView)).
			Build()

		if !errors.Is(err, ErrSchemaInvalid) {
			t.Fatalf("err = %v; want ErrSchemaInvalid", err)
		}

		if !strings.Contains(err.Error(), "reaches back") {
			t.Errorf("err = %q; want it to name the cycle", err)
		}
	})

	t.Run("subtracting own tuples", func(t *testing.T) {
		t.Parallel()

		// view = viewer - this  negates view on itself.
		_, err := NewSchemaBuilder().
			Define(docRel(rViewer), This()).
			Define(docRel(rView), Exclusion(ComputedUserset(rViewer), This())).
			Build()

		if !errors.Is(err, ErrSchemaInvalid) {
			t.Errorf("err = %v; want ErrSchemaInvalid", err)
		}
	})
}

// Recursion itself is fine; only recursion through a subtract is not.
func TestSchemaAcceptsPositiveRecursion(t *testing.T) {
	t.Parallel()

	_, err := NewSchemaBuilder().
		Define(docRel(rViewer), Union(This(), ComputedUserset(rEditor))).
		Define(docRel(rEditor), Union(This(), ComputedUserset(rViewer))).
		Build()
	if err != nil {
		t.Errorf("Build: %v; positive recursion is legal", err)
	}
}

// Placement forces these local and ties their lag to readiness, so the set has to
// close over everything the subtract side can reach, not just its first hop.
func TestSchemaExclusionReachableIsTransitive(t *testing.T) {
	t.Parallel()

	// view = viewer - banned.
	// banned = owner.
	// owner = this.
	s, err := NewSchemaBuilder().
		Define(docRel(rViewer), This()).
		Define(docRel(rOwner), This()).
		Define(docRel(rBanned), ComputedUserset(rOwner)).
		Define(docRel(rView), Exclusion(ComputedUserset(rViewer), ComputedUserset(rBanned))).
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

// Build freezes the schema, so the tree a caller passed to Define is not a way back in.
func TestSchemaBuildCopiesTheRewriteTree(t *testing.T) {
	t.Parallel()

	rw := Union(ComputedUserset(rViewer), ComputedUserset(rOwner))

	s, err := NewSchemaBuilder().
		Define(docRel(rViewer), This()).
		Define(docRel(rOwner), This()).
		Define(docRel(rView), rw).
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
