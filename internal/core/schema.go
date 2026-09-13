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

// SubjectType is one entry in a relation's accepted subject list.
type SubjectType struct {
	Type     TypeID
	Relation RelationID // a userset such as team#member; NoRelation names the objects themselves
	Wildcard bool       // type:*, which a userset may not combine with
}

// DirectType accepts the objects of a type as subjects.
func DirectType(typ TypeID) SubjectType {
	return SubjectType{Type: typ}
}

// WildcardType accepts "type:*", which names every object of the type at once.
func WildcardType(typ TypeID) SubjectType {
	return SubjectType{Type: typ, Wildcard: true}
}

// UsersetType accepts a userset such as team#member.
func UsersetType(typ TypeID, rel RelationID) SubjectType {
	return SubjectType{Type: typ, Relation: rel}
}

var (
	// ErrSchemaInvalid reports that a schema could not be built.
	ErrSchemaInvalid = errors.New("core: invalid schema")

	// ErrUnsupportedOp reports a rewrite operator this build cannot evaluate.
	ErrUnsupportedOp = errors.New("core: unsupported rewrite operator")
)

// Schema holds every relation's rewrite and the subjects it accepts.
// Build is the only way to make one, so a schema in hand has already passed validation.
type Schema struct {
	rewrites  map[RelationRef]Rewrite
	allowed   map[RelationRef][]SubjectType
	reachable []RelationRef
}

// Rewrite returns the definition of r.
func (s *Schema) Rewrite(r RelationRef) (Rewrite, bool) {
	rw, ok := s.rewrites[r]

	return rw, ok
}

// Allows reports whether r accepts st as a subject. A permission accepts none.
func (s *Schema) Allows(r RelationRef, st SubjectType) bool {
	return slices.Contains(s.allowed[r], st)
}

// ExclusionReachable returns every relation reachable from the subtract side of an exclusion, sorted.
func (s *Schema) ExclusionReachable() []RelationRef {
	return slices.Clone(s.reachable)
}

// SchemaBuilder collects relation and permission definitions. Build validates them together.
// Defining the same relation twice, by either method, is an error reported by Build.
type SchemaBuilder struct {
	rewrites map[RelationRef]Rewrite
	allowed  map[RelationRef][]SubjectType
	errs     []error
}

// NewSchemaBuilder returns an empty builder.
func NewSchemaBuilder() *SchemaBuilder {
	return &SchemaBuilder{
		rewrites: make(map[RelationRef]Rewrite),
		allowed:  make(map[RelationRef][]SubjectType),
	}
}

// Relation records a relation that stores tuples, and the subject types it accepts.
func (b *SchemaBuilder) Relation(r RelationRef, allowed ...SubjectType) *SchemaBuilder {
	if b.define(r, This()) {
		b.allowed[r] = slices.Clone(allowed)
	}

	return b
}

// Permission records a relation computed from others.
// It stores no tuples, so it accepts no subjects and its rewrite may not read any with "this".
func (b *SchemaBuilder) Permission(r RelationRef, rw Rewrite) *SchemaBuilder {
	b.define(r, rw)

	return b
}

// define records rw for r, reporting whether it was the first definition.
func (b *SchemaBuilder) define(r RelationRef, rw Rewrite) bool {
	if _, ok := b.rewrites[r]; ok {
		b.errs = append(b.errs, fmt.Errorf("%w: relation %+v defined twice", ErrSchemaInvalid, r))

		return false
	}

	b.rewrites[r] = rw

	return true
}

// Build validates and freezes the schema.
func (b *SchemaBuilder) Build() (*Schema, error) {
	errs := slices.Clone(b.errs)

	var edges []edge

	for _, ref := range sortedRefs(b.rewrites) {
		rw := b.rewrites[ref]

		errs = append(errs, b.checkShape(ref, rw, true)...)
		errs = append(errs, b.checkAllowed(ref, rw)...)

		collectEdges(ref, rw, false, &edges)
		collectStoredEdges(ref, b.allowed[ref], &edges)
	}

	errs = append(errs, checkStratified(edges)...)

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	rewrites := make(map[RelationRef]Rewrite, len(b.rewrites))
	for ref, rw := range b.rewrites {
		rewrites[ref] = cloneRewrite(rw)
	}

	// The lists themselves are already the builder's own copies, taken in Relation.
	allowed := make(map[RelationRef][]SubjectType, len(b.allowed))
	for ref, list := range b.allowed {
		allowed[ref] = list
	}

	return &Schema{
		rewrites:  rewrites,
		allowed:   allowed,
		reachable: exclusionReachable(edges),
	}, nil
}

// cloneRewrite copies a rewrite tree so a built schema cannot be reached through the
// slice its caller passed to Permission.
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
// top marks the root of a definition, the only place "this" may stand.
func (b *SchemaBuilder) checkShape(ref RelationRef, rw Rewrite, top bool) []error {
	var errs []error

	// A field the operator does not read is a mistaken intention, not spare capacity:
	// the evaluator would drop it without a word.
	switch rw.Op {
	case OpThis:
		if !top {
			errs = append(errs, fmt.Errorf(
				"%w: %+v: this is a whole relation, not an operand of a permission", ErrSchemaInvalid, ref))
		}

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
		errs = append(errs, b.checkShape(ref, child, false)...)
	}

	return errs
}

// checkAllowed validates a relation's accepted subject types.
func (b *SchemaBuilder) checkAllowed(ref RelationRef, rw Rewrite) []error {
	if rw.Op != OpThis {
		return nil
	}

	allowed := b.allowed[ref]
	if len(allowed) == 0 {
		return []error{fmt.Errorf(
			"%w: %+v accepts no subject type, so no tuple on it could grant", ErrSchemaInvalid, ref)}
	}

	var errs []error

	for i, st := range allowed {
		if st.Type == 0 {
			errs = append(errs, fmt.Errorf("%w: %+v: a subject type did not resolve", ErrSchemaInvalid, ref))

			continue
		}

		// A wildcard names every object of a type, and a userset is a set rather than one of them.
		if st.Wildcard && st.Relation != NoRelation {
			errs = append(errs, fmt.Errorf("%w: %+v: a wildcard cannot carry a relation", ErrSchemaInvalid, ref))

			continue
		}

		if slices.Contains(allowed[:i], st) {
			errs = append(errs, fmt.Errorf("%w: %+v: subject type %+v listed twice", ErrSchemaInvalid, ref, st))
		}

		if st.Relation == NoRelation {
			continue
		}

		// Stratification draws an edge to this relation, so it has to exist to be drawn to.
		target := RelationRef{Type: st.Type, Relation: st.Relation}
		if _, ok := b.rewrites[target]; !ok {
			errs = append(errs, fmt.Errorf("%w: %+v accepts undefined %+v", ErrSchemaInvalid, ref, target))
		}
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
		// A relation's dependencies come from its accepted types, not its rewrite.

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

// collectStoredEdges records what a relation's tuples are allowed to point at, which is a
// dependency no rewrite shows: without it an exclusion could close a loop through data.
func collectStoredEdges(from RelationRef, allowed []SubjectType, out *[]edge) {
	for _, st := range allowed {
		if st.Relation == NoRelation {
			continue
		}

		*out = append(*out, edge{from: from, to: RelationRef{Type: st.Type, Relation: st.Relation}})
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
