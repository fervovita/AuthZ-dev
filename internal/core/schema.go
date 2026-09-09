package core

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// Op is a rewrite operator.
type Op uint8

// Op values.
const (
	OpThis            Op = iota + 1 // the tuples stored on the relation being evaluated
	OpComputedUserset               // another relation on the same object
	OpTupleToUserset                // follow Tupleset, then Relation on each result
	OpUnion                         // OR
	OpIntersection                  // AND
	OpExclusion                     // BUT NOT
)

func (o Op) String() string {
	switch o {
	case OpThis:
		return "this"
	case OpComputedUserset:
		return "computed_userset"
	case OpTupleToUserset:
		return "tuple_to_userset"
	case OpUnion:
		return "union"
	case OpIntersection:
		return "intersection"
	case OpExclusion:
		return "exclusion"
	default:
		return "op(" + strconv.Itoa(int(o)) + ")"
	}
}

// Rewrite is one node of a relation's definition.
type Rewrite struct {
	Op       Op
	Relation RelationID // OpComputedUserset, OpTupleToUserset
	Tupleset RelationID // OpTupleToUserset
	Children []Rewrite  // OpUnion, OpIntersection, OpExclusion
}

// This returns the rewrite reading the relation's own stored tuples.
func This() Rewrite {
	return Rewrite{Op: OpThis}
}

// ComputedUserset returns the rewrite reading another relation on the same object.
func ComputedUserset(rel RelationID) Rewrite {
	return Rewrite{Op: OpComputedUserset, Relation: rel}
}

// TupleToUserset returns the arrow rewrite.
// Build rejects it until the source can enumerate a tupleset's subjects.
func TupleToUserset(tupleset, rel RelationID) Rewrite {
	return Rewrite{Op: OpTupleToUserset, Tupleset: tupleset, Relation: rel}
}

// Union returns the rewrite satisfied when any child is.
func Union(children ...Rewrite) Rewrite {
	return Rewrite{Op: OpUnion, Children: children}
}

// Intersection returns the rewrite satisfied when every child is.
func Intersection(children ...Rewrite) Rewrite {
	return Rewrite{Op: OpIntersection, Children: children}
}

// Exclusion returns the rewrite satisfied when base is and subtract is not.
func Exclusion(base, subtract Rewrite) Rewrite {
	return Rewrite{Op: OpExclusion, Children: []Rewrite{base, subtract}}
}

// RelationRef names a relation on an object type.
type RelationRef struct {
	Type     TypeID
	Relation RelationID
}

var (
	// ErrSchemaInvalid reports that a schema could not be built.
	ErrSchemaInvalid = errors.New("core: invalid schema")

	// ErrUnsupportedOp reports a rewrite operator this build cannot evaluate.
	ErrUnsupportedOp = errors.New("core: unsupported rewrite operator")
)

// Schema holds every relation's rewrite.
// Build is the only way to make one, so a schema in hand has already passed validation.
type Schema struct {
	rewrites  map[RelationRef]Rewrite
	reachable []RelationRef
}

// Rewrite returns the definition of r.
func (s *Schema) Rewrite(r RelationRef) (Rewrite, bool) {
	rw, ok := s.rewrites[r]

	return rw, ok
}

// ExclusionReachable returns every relation reachable from the subtract side of an exclusion, sorted.
func (s *Schema) ExclusionReachable() []RelationRef {
	return slices.Clone(s.reachable)
}

// SchemaBuilder collects relation definitions. Build validates them together.
type SchemaBuilder struct {
	rewrites map[RelationRef]Rewrite
	errs     []error
}

// NewSchemaBuilder returns an empty builder.
func NewSchemaBuilder() *SchemaBuilder {
	return &SchemaBuilder{rewrites: make(map[RelationRef]Rewrite)}
}

// Define records the rewrite for r.
// Defining the same relation twice is an error reported by Build.
func (b *SchemaBuilder) Define(r RelationRef, rw Rewrite) *SchemaBuilder {
	if _, ok := b.rewrites[r]; ok {
		b.errs = append(b.errs, fmt.Errorf("%w: relation %+v defined twice", ErrSchemaInvalid, r))

		return b
	}

	b.rewrites[r] = rw

	return b
}

// Build validates and freezes the schema.
func (b *SchemaBuilder) Build() (*Schema, error) {
	errs := slices.Clone(b.errs)

	var edges []edge

	for _, ref := range sortedRefs(b.rewrites) {
		errs = append(errs, b.checkShape(ref, b.rewrites[ref])...)
		collectEdges(ref, b.rewrites[ref], false, &edges)
	}

	errs = append(errs, checkStratified(edges)...)

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	rewrites := make(map[RelationRef]Rewrite, len(b.rewrites))
	for ref, rw := range b.rewrites {
		rewrites[ref] = cloneRewrite(rw)
	}

	return &Schema{
		rewrites:  rewrites,
		reachable: exclusionReachable(edges),
	}, nil
}

// cloneRewrite copies a rewrite tree so a built schema cannot be reached through the
// slice its caller passed to Define.
func cloneRewrite(rw Rewrite) Rewrite {
	if rw.Children == nil {
		return rw
	}

	children := make([]Rewrite, len(rw.Children))
	for i, child := range rw.Children {
		children[i] = cloneRewrite(child)
	}

	rw.Children = children

	return rw
}

// checkShape validates one rewrite tree's structure and its references.
func (b *SchemaBuilder) checkShape(ref RelationRef, rw Rewrite) []error {
	var errs []error

	// A field the operator does not read is a mistaken intention, not spare capacity:
	// the evaluator would drop it without a word.
	switch rw.Op {
	case OpThis:
		if len(rw.Children) != 0 || rw.Relation != NoRelation || rw.Tupleset != NoRelation {
			errs = append(errs, fmt.Errorf("%w: %+v: this takes no operands", ErrSchemaInvalid, ref))
		}

	case OpComputedUserset:
		if len(rw.Children) != 0 || rw.Tupleset != NoRelation {
			errs = append(errs, fmt.Errorf("%w: %+v: computed_userset takes only a relation", ErrSchemaInvalid, ref))
		}

		if rw.Relation == NoRelation {
			errs = append(errs, fmt.Errorf("%w: %+v: computed_userset needs a relation", ErrSchemaInvalid, ref))

			break
		}

		target := RelationRef{Type: ref.Type, Relation: rw.Relation}
		if _, ok := b.rewrites[target]; !ok {
			errs = append(errs, fmt.Errorf("%w: %+v refers to undefined %+v", ErrSchemaInvalid, ref, target))
		}

	case OpTupleToUserset:
		errs = append(errs, fmt.Errorf("%w: %+v: %s", ErrUnsupportedOp, ref, rw.Op))

	case OpUnion, OpIntersection:
		if rw.Relation != NoRelation || rw.Tupleset != NoRelation {
			errs = append(errs, fmt.Errorf("%w: %+v: %s takes only operands", ErrSchemaInvalid, ref, rw.Op))
		}

		if len(rw.Children) < 2 {
			errs = append(errs, fmt.Errorf("%w: %+v: %s needs two or more operands", ErrSchemaInvalid, ref, rw.Op))
		}

	case OpExclusion:
		if rw.Relation != NoRelation || rw.Tupleset != NoRelation {
			errs = append(errs, fmt.Errorf("%w: %+v: exclusion takes only operands", ErrSchemaInvalid, ref))
		}

		if len(rw.Children) != 2 {
			errs = append(errs, fmt.Errorf("%w: %+v: exclusion needs exactly two operands", ErrSchemaInvalid, ref))
		}

	default:
		errs = append(errs, fmt.Errorf("%w: %+v: %s", ErrSchemaInvalid, ref, rw.Op))
	}

	for _, child := range rw.Children {
		errs = append(errs, b.checkShape(ref, child)...)
	}

	return errs
}

// edge is a dependency from one relation to another.
// negated marks a dependency that passes through the subtract side of an exclusion.
type edge struct {
	from, to RelationRef
	negated  bool
}

// collectEdges walks a rewrite, recording which relations it depends on and whether
// the dependency crosses an exclusion's subtract side.
func collectEdges(from RelationRef, rw Rewrite, negated bool, out *[]edge) {
	switch rw.Op {
	case OpThis:
		// Only worth an edge under a subtract, where from depends negatively on itself.
		if negated {
			*out = append(*out, edge{from: from, to: from, negated: true})
		}

	case OpComputedUserset:
		*out = append(*out, edge{
			from:    from,
			to:      RelationRef{Type: from.Type, Relation: rw.Relation},
			negated: negated,
		})

	case OpExclusion:
		if len(rw.Children) == 2 {
			collectEdges(from, rw.Children[0], negated, out)
			collectEdges(from, rw.Children[1], true, out)
		}

	case OpUnion, OpIntersection:
		for _, child := range rw.Children {
			collectEdges(from, child, negated, out)
		}

	case OpTupleToUserset:
		// Rejected by checkShape; no edge to draw without the target's type.

	default:
	}
}

// checkStratified rejects recursion that passes through an exclusion.
// Without it the "already computing means false" cycle rule has no least fixed point to
// converge on, so a denial could be an artefact of traversal order.
func checkStratified(edges []edge) []error {
	var errs []error

	for _, e := range edges {
		if !e.negated {
			continue
		}

		if reaches(edges, e.to, e.from) {
			errs = append(errs, fmt.Errorf(
				"%w: %+v excludes %+v, which reaches back to it",
				ErrSchemaInvalid, e.from, e.to))
		}
	}

	return errs
}

// reaches reports whether to is reachable from start, following every edge.
func reaches(edges []edge, start, to RelationRef) bool {
	seen := map[RelationRef]bool{start: true}
	queue := []RelationRef{start}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if cur == to {
			return true
		}

		for _, e := range edges {
			if e.from == cur && !seen[e.to] {
				seen[e.to] = true
				queue = append(queue, e.to)
			}
		}
	}

	return false
}

// exclusionReachable closes over every relation a subtract side can reach.
func exclusionReachable(edges []edge) []RelationRef {
	seen := make(map[RelationRef]bool)

	var queue []RelationRef

	for _, e := range edges {
		if e.negated && !seen[e.to] {
			seen[e.to] = true
			queue = append(queue, e.to)
		}
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		for _, e := range edges {
			if e.from == cur && !seen[e.to] {
				seen[e.to] = true
				queue = append(queue, e.to)
			}
		}
	}

	out := make([]RelationRef, 0, len(seen))
	for ref := range seen {
		out = append(out, ref)
	}

	sortRefs(out)

	return out
}

func sortedRefs(m map[RelationRef]Rewrite) []RelationRef {
	out := make([]RelationRef, 0, len(m))
	for ref := range m {
		out = append(out, ref)
	}

	sortRefs(out)

	return out
}

func sortRefs(refs []RelationRef) {
	slices.SortFunc(refs, func(a, b RelationRef) int {
		if c := cmp.Compare(a.Type, b.Type); c != 0 {
			return c
		}

		return cmp.Compare(a.Relation, b.Relation)
	})
}
