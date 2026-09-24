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

// TupleToUserset returns the arrow rewrite, tupleset->rel.
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

// SchemaError is one reason Build refused a schema. It matches ErrSchemaInvalid.
// The reason keeps the ids it mentions as values, so a caller that knows the names can say it in them.
type SchemaError struct {
	Ref RelationRef // the relation at fault

	msg    string
	format string
	args   []any
}

// schemaError reports a problem with ref. The format names whatever it names, ref included.
// Writing the reason here, rather than in Error, is also what shows vet the format: it checks
// the calls below only because this one hands a format and its arguments to fmt.
func schemaError(ref RelationRef, format string, args ...any) *SchemaError {
	return &SchemaError{Ref: ref, msg: fmt.Sprintf(format, args...), format: format, args: args}
}

func (e *SchemaError) Error() string {
	return ErrSchemaInvalid.Error() + ": " + e.msg
}

// Unwrap lets errors.Is match ErrSchemaInvalid.
func (e *SchemaError) Unwrap() error { return ErrSchemaInvalid }

// Explain gives the reason with the names d holds in place of ids:
// document#view for a relation, and user, user:* or team#member for a subject type.
func (e *SchemaError) Explain(d *Dictionary) string {
	args := make([]any, len(e.args))
	for i, a := range e.args {
		args[i] = d.describe(a)
	}

	return fmt.Sprintf(e.format, args...)
}

// described prints as itself under any verb, so a format written for ids takes names unchanged.
type described string

func (s described) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(s))
}

// describe names an id-bearing value, falling back to the number for an id d never interned.
func (d *Dictionary) describe(a any) any {
	typeName := func(id TypeID) string {
		if name, ok := d.TypeName(id); ok {
			return name
		}

		return strconv.FormatUint(uint64(id), 10)
	}

	relName := func(id RelationID) string {
		if name, ok := d.RelationName(id); ok {
			return name
		}

		return strconv.FormatUint(uint64(id), 10)
	}

	switch v := a.(type) {
	case RelationRef:
		return described(typeName(v.Type) + "#" + relName(v.Relation))
	case SubjectType:
		switch {
		case v.Wildcard:
			return described(typeName(v.Type) + ":" + WildcardMarker)
		case v.Relation != NoRelation:
			return described(typeName(v.Type) + "#" + relName(v.Relation))
		default:
			return described(typeName(v.Type))
		}
	case RelationID:
		return described(relName(v))
	default:
		return a
	}
}

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
		b.errs = append(b.errs, schemaError(r, "relation %+v defined twice", r))

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

		b.collectEdges(ref, rw, false, &edges)
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
			errs = append(errs, schemaError(ref,
				"%+v: this is a whole relation, not an operand of a permission", ref))
		}

		if len(rw.Children) != 0 || rw.Relation != NoRelation || rw.Tupleset != NoRelation {
			errs = append(errs, schemaError(ref, "%+v: this takes no operands", ref))
		}

	case OpComputedUserset:
		if len(rw.Children) != 0 || rw.Tupleset != NoRelation {
			errs = append(errs, schemaError(ref, "%+v: computed_userset takes only a relation", ref))
		}

		if rw.Relation == NoRelation {
			errs = append(errs, schemaError(ref, "%+v: computed_userset needs a relation", ref))

			break
		}

		target := RelationRef{Type: ref.Type, Relation: rw.Relation}
		if _, ok := b.rewrites[target]; !ok {
			errs = append(errs, schemaError(ref, "%+v refers to undefined %+v", ref, target))
		}

	case OpTupleToUserset:
		errs = append(errs, b.checkArrow(ref, rw)...)

	case OpUnion, OpIntersection:
		if rw.Relation != NoRelation || rw.Tupleset != NoRelation {
			errs = append(errs, schemaError(ref, "%+v: %s takes only operands", ref, rw.Op))
		}

		if len(rw.Children) < 2 {
			errs = append(errs, schemaError(ref, "%+v: %s needs two or more operands", ref, rw.Op))
		}

	case OpExclusion:
		if rw.Relation != NoRelation || rw.Tupleset != NoRelation {
			errs = append(errs, schemaError(ref, "%+v: exclusion takes only operands", ref))
		}

		if len(rw.Children) != 2 {
			errs = append(errs, schemaError(ref, "%+v: exclusion needs exactly two operands", ref))
		}

	default:
		errs = append(errs, schemaError(ref, "%+v: %s", ref, rw.Op))
	}

	for _, child := range rw.Children {
		errs = append(errs, b.checkShape(ref, child, false)...)
	}

	return errs
}

// checkArrow validates an arrow: its tupleset is a relation naming objects, and one of their types defines the target.
func (b *SchemaBuilder) checkArrow(ref RelationRef, rw Rewrite) []error {
	var errs []error

	if len(rw.Children) != 0 {
		errs = append(errs, schemaError(ref, "%+v: tuple_to_userset takes only a tupleset and a relation", ref))
	}

	if rw.Tupleset == NoRelation || rw.Relation == NoRelation {
		return append(errs, schemaError(ref, "%+v: tuple_to_userset needs a tupleset and a relation", ref))
	}

	tupleset := RelationRef{Type: ref.Type, Relation: rw.Tupleset}

	ts, ok := b.rewrites[tupleset]
	if !ok {
		return append(errs, schemaError(ref, "%+v refers to undefined %+v", ref, tupleset))
	}

	if ts.Op != OpThis {
		return append(errs, schemaError(ref, "%+v: tupleset %+v is a permission and stores nothing to follow",
			ref, tupleset))
	}

	// A userset or a wildcard names no single object to follow.
	for _, st := range b.allowed[tupleset] {
		if st.Relation != NoRelation || st.Wildcard {
			errs = append(errs, schemaError(ref, "%+v: tupleset %+v accepts %+v, but an arrow follows objects only",
				ref, tupleset, st))
		}
	}

	if len(b.arrowTargets(tupleset, rw.Relation)) == 0 {
		errs = append(errs, schemaError(ref, "%+v: relation %d is not defined on any type %+v accepts",
			ref, rw.Relation, tupleset))
	}

	return errs
}

// arrowTargets lists rel on each type the tupleset accepts that defines it.
func (b *SchemaBuilder) arrowTargets(tupleset RelationRef, rel RelationID) []RelationRef {
	var out []RelationRef

	for _, st := range b.allowed[tupleset] {
		target := RelationRef{Type: st.Type, Relation: rel}
		if _, ok := b.rewrites[target]; ok {
			out = append(out, target)
		}
	}

	return out
}

// checkAllowed validates a relation's accepted subject types.
func (b *SchemaBuilder) checkAllowed(ref RelationRef, rw Rewrite) []error {
	if rw.Op != OpThis {
		return nil
	}

	allowed := b.allowed[ref]
	if len(allowed) == 0 {
		return []error{schemaError(ref,
			"%+v accepts no subject type, so no tuple on it could grant", ref)}
	}

	var errs []error

	for i, st := range allowed {
		if st.Type == 0 {
			errs = append(errs, schemaError(ref, "%+v: a subject type did not resolve", ref))

			continue
		}

		// A wildcard names every object of a type, and a userset is a set rather than one of them.
		if st.Wildcard && st.Relation != NoRelation {
			errs = append(errs, schemaError(ref, "%+v: a wildcard cannot carry a relation", ref))

			continue
		}

		if slices.Contains(allowed[:i], st) {
			errs = append(errs, schemaError(ref, "%+v: subject type %+v listed twice", ref, st))

			continue
		}

		if st.Relation == NoRelation {
			continue
		}

		// Stratification draws an edge to this relation, so it has to exist to be drawn to.
		target := RelationRef{Type: st.Type, Relation: st.Relation}
		if _, ok := b.rewrites[target]; !ok {
			errs = append(errs, schemaError(ref, "%+v accepts undefined %+v", ref, target))

			continue
		}

		if w := b.reachesWildcard(target, map[RelationRef]bool{}); w != nil {
			errs = append(errs, schemaError(ref, "%+v accepts %+v, which reaches %+v at %+v", ref, target, w.st, w.at))
		}
	}

	return errs
}

// wildcardSite is a relation that accepts a wildcard, and the wildcard it accepts.
type wildcardSite struct {
	at RelationRef
	st SubjectType
}

// reachesWildcard reports where ref picks up every subject of a type: a wildcard it accepts, or one
// the relations a permission's expression reads accept.
// The kind of operator does not matter: a wildcard in any operand counts.
// Userset subject types are not followed: each is refused where it is declared.
func (b *SchemaBuilder) reachesWildcard(ref RelationRef, seen map[RelationRef]bool) *wildcardSite {
	rw, ok := b.rewrites[ref]
	if !ok || seen[ref] {
		return nil
	}

	seen[ref] = true

	if rw.Op != OpThis {
		return b.rewriteReachesWildcard(ref.Type, rw, seen)
	}

	for _, st := range b.allowed[ref] {
		if st.Wildcard {
			return &wildcardSite{at: ref, st: st}
		}
	}

	return nil
}

// rewriteReachesWildcard walks a permission's expression for reachesWildcard.
func (b *SchemaBuilder) rewriteReachesWildcard(typ TypeID, rw Rewrite, seen map[RelationRef]bool) *wildcardSite {
	switch rw.Op {
	case OpThis:
		// A permission's expression cannot hold "this"; checkShape refuses that.

	case OpComputedUserset:
		return b.reachesWildcard(RelationRef{Type: typ, Relation: rw.Relation}, seen)

	case OpTupleToUserset:
		// An arrow yields the subjects of Relation on whatever the tupleset names.
		for _, target := range b.arrowTargets(RelationRef{Type: typ, Relation: rw.Tupleset}, rw.Relation) {
			if w := b.reachesWildcard(target, seen); w != nil {
				return w
			}
		}

	case OpUnion, OpIntersection, OpExclusion:
		// Every operand counts the same, including the one an exclusion subtracts.
		for _, child := range rw.Children {
			if w := b.rewriteReachesWildcard(typ, child, seen); w != nil {
				return w
			}
		}
	}

	return nil
}

// edge is a dependency from one relation to another.
// negated marks a dependency that passes through the subtract side of an exclusion.
type edge struct {
	from, to RelationRef
	negated  bool
}

// collectEdges walks a rewrite, recording which relations it depends on and whether
// the dependency crosses an exclusion's subtract side.
func (b *SchemaBuilder) collectEdges(from RelationRef, rw Rewrite, negated bool, out *[]edge) {
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
			b.collectEdges(from, rw.Children[0], negated, out)
			b.collectEdges(from, rw.Children[1], true, out)
		}

	case OpUnion, OpIntersection:
		for _, child := range rw.Children {
			b.collectEdges(from, child, negated, out)
		}

	case OpTupleToUserset:
		// The tupleset is read as well, so under a subtract it matters as much as where it leads.
		tupleset := RelationRef{Type: from.Type, Relation: rw.Tupleset}
		*out = append(*out, edge{from: from, to: tupleset, negated: negated})

		for _, target := range b.arrowTargets(tupleset, rw.Relation) {
			*out = append(*out, edge{from: from, to: target, negated: negated})
		}

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
			errs = append(errs, schemaError(e.from,
				"%+v excludes %+v, which reaches back to it",
				e.from, e.to))
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
