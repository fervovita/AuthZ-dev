package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// DefaultMaxDepth bounds how far one check descends.
const DefaultMaxDepth = 50

var (
	// ErrMaxDepthExceeded reports that a check ran deeper than the engine allows.
	ErrMaxDepthExceeded = errors.New("core: maximum evaluation depth exceeded")

	// ErrRelationUndefined reports a relation the schema does not define.
	ErrRelationUndefined = errors.New("core: relation not defined")

	// ErrInvalidRequest reports a malformed CheckRequest.
	ErrInvalidRequest = errors.New("core: invalid check request")
)

// Engine answers checks against one schema and one tuple source.
type Engine struct {
	schema   *Schema
	source   TupleSource
	maxDepth int
}

// Option configures an Engine.
type Option func(*Engine)

// WithMaxDepth sets the descent bound.
func WithMaxDepth(n int) Option {
	return func(e *Engine) {
		e.maxDepth = n
	}
}

// NewEngine returns an Engine reading source under schema.
func NewEngine(schema *Schema, source TupleSource, opts ...Option) (*Engine, error) {
	if schema == nil || source == nil {
		return nil, fmt.Errorf("%w: schema and source are required", ErrInvalidRequest)
	}

	e := &Engine{schema: schema, source: source, maxDepth: DefaultMaxDepth}
	for _, opt := range opts {
		opt(e)
	}

	if e.maxDepth < 1 {
		return nil, fmt.Errorf("%w: max depth must be positive", ErrInvalidRequest)
	}

	return e, nil
}

// CheckRequest asks whether Subject holds Relation on Object.
type CheckRequest struct {
	Object   ObjectRef
	Relation RelationID
	Subject  SubjectRef // concrete; a whole type is not a question Check answers
	Proof    ProofMode
}

// CheckResult is the decision and, when asked for, why.
type CheckResult struct {
	Allowed bool
	Proof   Proof
}

// Check answers req: allowed or denied, never "unknown" for data that may be stale.
// A proof comes back with an error too, so a depth bound still says where it stopped.
func (e *Engine) Check(ctx context.Context, req CheckRequest) (CheckResult, error) {
	if !req.Object.Valid() || req.Relation == NoRelation {
		return CheckResult{}, fmt.Errorf("%w: object and relation must resolve", ErrInvalidRequest)
	}

	if !req.Subject.Valid() || req.Subject.Wildcard {
		return CheckResult{}, fmt.Errorf("%w: subject must be concrete", ErrInvalidRequest)
	}

	r := &run{
		ctx:     ctx,
		engine:  e,
		subject: req.Subject,
		memo:    make(map[memoKey]memoEntry),
		rec:     recorder{mode: req.Proof, subject: req.Subject},
	}

	allowed, _, err := r.relation(req.Object, req.Relation)

	res := CheckResult{Allowed: allowed, Proof: r.rec.proof()}
	if err != nil {
		return CheckResult{Proof: res.Proof}, err
	}

	return res, nil
}

type memoKey struct {
	Object   ObjectRef
	Relation RelationID
}

type memoState uint8

const (
	memoComputing memoState = iota + 1
	memoTrue
	memoFalse
)

// memoEntry is what a relation concluded, and whether that answer can still move.
type memoEntry struct {
	state memoState

	// index is Tarjan's index and onStack in one field: where this node fell in visit order
	// while it is on the stack, and zero once its component has settled and been popped.
	// Visit order, not stack depth, which sibling subtrees reuse.
	index int
}

func stateOf(allowed bool) memoState {
	if allowed {
		return memoTrue
	}

	return memoFalse
}

// run is the state of one check.
// The subject never changes during a check, so the memo is keyed by object and relation alone.
type run struct {
	ctx     context.Context
	engine  *Engine
	subject SubjectRef
	memo    map[memoKey]memoEntry
	rec     recorder
	depth   int

	// index is Tarjan's counter, handing each relation a number as it is entered.
	index int

	// lowlink is the lowest index the subtree being evaluated can reach.
	lowlink int

	// stack is Tarjan's, not the call stack: a frame that returns without being its
	// component's root stays on it, so slicing from a root's position gives its component.
	stack []memoKey

	// Scratch for the one-request source calls.
	// Refilled before each call and read before any recursion, so nested frames reusing it is safe.
	wildcardReq [1]WildcardRequest
	wildcardOut [1]bool
	lookupReq   [1]LookupRequest
	subjectsReq [1]SubjectsRequest
}

// relation evaluates one relation on one object, memoized for the whole check.
func (r *run) relation(obj ObjectRef, rel RelationID) (bool, int, error) {
	// Every recursion passes through here, so cancellation does not rest on the source.
	if err := r.ctx.Err(); err != nil {
		return false, noNode, err
	}

	rw, ok := r.engine.schema.Rewrite(RelationRef{Type: obj.Type, Relation: rel})
	if !ok {
		return false, noNode, fmt.Errorf("%w: type %d relation %d", ErrRelationUndefined, obj.Type, rel)
	}

	key := memoKey{Object: obj, Relation: rel}

	e := r.memo[key]

	if e.index != 0 {
		r.lowlink = min(r.lowlink, e.index)
	}

	// Re-entering a node still being computed is a cycle; false is the least fixed point.
	switch e.state {
	case memoComputing:
		return false, r.rec.leaf(obj, rel, rw.Op, OutcomeCycle, false), nil
	case memoTrue:
		return true, r.rec.leaf(obj, rel, rw.Op, OutcomeAllowed, true), nil
	case memoFalse:
		return false, r.rec.leaf(obj, rel, rw.Op, OutcomeDenied, true), nil
	}

	if r.depth >= r.engine.maxDepth {
		return false, noNode, fmt.Errorf("%w: %d", ErrMaxDepthExceeded, r.engine.maxDepth)
	}

	r.depth++
	r.index++
	index := r.index

	pos := len(r.stack)
	r.stack = append(r.stack, key)
	r.memo[key] = memoEntry{state: memoComputing, index: index}

	outer := r.lowlink
	r.lowlink = index

	allowed, idx, err := r.rewrite(rw, obj, rel)
	if err != nil {
		r.depth--

		return false, idx, err
	}

	r.memo[key] = memoEntry{state: stateOf(allowed), index: index}

	// Tarjan's root test: nothing this subtree reached was entered before this node.
	isRoot := r.lowlink == index
	if isRoot {
		if err := r.settle(pos); err != nil {
			r.depth--

			return false, idx, err
		}

		// Settling opens branches the traversal never took, which can reach further out.
		isRoot = r.lowlink == index
	}

	if isRoot {
		for _, k := range r.stack[pos:] {
			r.pop(k)
		}

		r.stack = r.stack[:pos]

		// The fixed point can raise this node's own answer above what the traversal found.
		allowed = r.memo[key].state == memoTrue
		r.rec.close(idx, allowed)
	}

	r.lowlink = min(outer, r.lowlink)
	r.depth--

	return allowed, idx, nil
}

// settle raises a component's answers to their least fixed point.
// The traversal answered each member against whatever the others held then, which is a lower bound, and every
// operator inside a component is monotone, so a round that changes nothing has arrived.
func (r *run) settle(pos int) error {
	if len(r.stack)-pos < 2 || !r.componentGrants(pos) {
		return nil
	}

	r.rec.pause()
	defer r.rec.resume()

	for {
		// Re-sliced each round: a new branch can pull in members the last round never saw.
		members := r.stack[pos:]

		changed := false

		for _, k := range members {
			// An answer at true is already at the top and cannot move again.
			if r.memo[k].state == memoTrue {
				continue
			}

			rw, _ := r.engine.schema.Rewrite(RelationRef{Type: k.Object.Type, Relation: k.Relation})

			allowed, _, err := r.rewrite(rw, k.Object, k.Relation)
			if err != nil {
				return err
			}

			// An answer only ever rises, which is what ends the loop.
			if e := r.memo[k]; allowed && e.state != memoTrue {
				e.state = memoTrue
				r.memo[k] = e
				changed = true
			}
		}

		// A round that pulled in new members has not answered them against settled ones yet.
		if !changed && len(r.stack)-pos == len(members) {
			return nil
		}
	}
}

// componentGrants reports whether any member came out true.
// When none did, the traversal already answered every member against nothing held, which is the least fixed point.
func (r *run) componentGrants(pos int) bool {
	for _, k := range r.stack[pos:] {
		if r.memo[k].state == memoTrue {
			return true
		}
	}

	return false
}

// pop takes a key off the stack: its component has settled, so its answer is final and
// reading it no longer draws anyone into that component.
func (r *run) pop(key memoKey) {
	e := r.memo[key]
	e.index = 0
	r.memo[key] = e
}

// rewrite evaluates one rewrite node and records it.
func (r *run) rewrite(rw Rewrite, obj ObjectRef, rel RelationID) (bool, int, error) {
	idx := r.rec.open(obj, rel, rw.Op)

	var (
		allowed bool
		err     error
	)

	switch rw.Op {
	case OpThis:
		allowed, err = r.this(obj, rel, idx)
	case OpComputedUserset:
		var child int

		allowed, child, err = r.relation(obj, rw.Relation)
		r.rec.addChild(idx, child)
	case OpUnion:
		allowed, err = r.union(rw, obj, rel, idx)
	case OpIntersection:
		allowed, err = r.intersection(rw, obj, rel, idx)
	case OpExclusion:
		allowed, err = r.exclusion(rw, obj, rel, idx)
	case OpTupleToUserset:
		allowed, err = r.arrow(rw, obj, idx)
	default:
		err = fmt.Errorf("%w: %s", ErrUnsupportedOp, rw.Op)
	}

	if err != nil {
		return false, idx, err
	}

	r.rec.close(idx, allowed)

	return allowed, idx, nil
}

// allows reports whether the relation accepts st as a subject.
// A tuple outside the set grants nobody rather than erroring, which would fail every check on the object.
func (r *run) allows(obj ObjectRef, rel RelationID, st SubjectType) bool {
	return r.engine.schema.Allows(RelationRef{Type: obj.Type, Relation: rel}, st)
}

// this reads the relation's own tuples: the wildcard bucket first, then the subject itself,
// then the usersets it would have to belong to.
func (r *run) this(obj ObjectRef, rel RelationID, idx int) (bool, error) {
	granted, err := r.wildcardGrants(obj, rel)
	if err != nil {
		return false, err
	}

	if granted {
		return true, nil
	}

	var usersets []SubjectRef

	direct := false

	r.lookupReq[0] = LookupRequest{Object: obj, Relation: rel, Subject: r.subject}

	err = r.engine.source.Lookup(r.ctx, r.lookupReq[:], func(_ int, res LookupResult) bool {
		direct = res.Direct
		// Usersets dies with the yield, and the contract forbids calling back in.
		usersets = slices.Clone(res.Usersets)

		return true
	})
	if err != nil {
		return false, err
	}

	// A rejected direct hit does not end the search: a userset may still grant.
	if direct && r.allows(obj, rel, SubjectType{Type: r.subject.Type, Relation: r.subject.Relation}) {
		return true, nil
	}

	for _, us := range usersets {
		if !r.followable(obj, rel, us) {
			continue
		}

		allowed, child, err := r.relation(ObjectRef{Type: us.Type, ID: us.ID}, us.Relation)
		r.rec.addChild(idx, child)

		if err != nil {
			return false, err
		}

		if allowed {
			return true, nil
		}
	}

	return false, nil
}

// wildcardGrants reports whether a wildcard tuple covers the subject.
// A wildcard names every object of a type, which a userset subject is not one of.
func (r *run) wildcardGrants(obj ObjectRef, rel RelationID) (bool, error) {
	if r.subject.Relation != NoRelation {
		return false, nil
	}

	if !r.allows(obj, rel, WildcardType(r.subject.Type)) {
		return false, nil
	}

	// The bucket is always local, so asking first answers a public resource off-network.
	r.wildcardReq[0] = WildcardRequest{Object: obj, Relation: rel, SubjectType: r.subject.Type}
	if err := r.engine.source.HasWildcard(r.ctx, r.wildcardReq[:], r.wildcardOut[:]); err != nil {
		return false, err
	}

	return r.wildcardOut[0], nil
}

// followable reports whether an entry from Usersets names a userset to descend into.
// Build proves an accepted userset type names a defined relation, so accepting it is the whole test.
func (r *run) followable(obj ObjectRef, rel RelationID, us SubjectRef) bool {
	if us.Relation == NoRelation || us.Wildcard {
		return false
	}

	return r.allows(obj, rel, UsersetType(us.Type, us.Relation))
}

// arrow asks for rw.Relation on each object the tupleset names.
// Build proves only that some accepted type defines the relation, so a type that does not is skipped, not an error.
func (r *run) arrow(rw Rewrite, obj ObjectRef, idx int) (bool, error) {
	var subjects []SubjectRef

	r.subjectsReq[0] = SubjectsRequest{Object: obj, Relation: rw.Tupleset}

	err := r.engine.source.Subjects(r.ctx, r.subjectsReq[:], func(_ int, res []SubjectRef) bool {
		subjects = slices.Clone(res)

		return true
	})
	if err != nil {
		return false, err
	}

	for _, s := range subjects {
		if !r.allows(obj, rw.Tupleset, SubjectType{Type: s.Type, Relation: s.Relation, Wildcard: s.Wildcard}) {
			continue
		}

		if _, ok := r.engine.schema.Rewrite(RelationRef{Type: s.Type, Relation: rw.Relation}); !ok {
			continue
		}

		allowed, child, err := r.relation(ObjectRef{Type: s.Type, ID: s.ID}, rw.Relation)
		r.rec.addChild(idx, child)

		if err != nil {
			return false, err
		}

		if allowed {
			return true, nil
		}
	}

	return false, nil
}

func (r *run) union(rw Rewrite, obj ObjectRef, rel RelationID, idx int) (bool, error) {
	for _, child := range rw.Children {
		allowed, cidx, err := r.rewrite(child, obj, rel)
		r.rec.addChild(idx, cidx)

		if err != nil {
			return false, err
		}

		if allowed {
			return true, nil
		}
	}

	return false, nil
}

func (r *run) intersection(rw Rewrite, obj ObjectRef, rel RelationID, idx int) (bool, error) {
	// An empty intersection is vacuously true, so a malformed one would grant out of nothing.
	if len(rw.Children) < 2 {
		return false, fmt.Errorf("%w: intersection needs two or more operands", ErrSchemaInvalid)
	}

	for _, child := range rw.Children {
		allowed, cidx, err := r.rewrite(child, obj, rel)
		r.rec.addChild(idx, cidx)

		if err != nil {
			return false, err
		}

		if !allowed {
			return false, nil
		}
	}

	return true, nil
}

func (r *run) exclusion(rw Rewrite, obj ObjectRef, rel RelationID, idx int) (bool, error) {
	// Indexing a malformed exclusion would panic and take the sidecar down with it.
	if len(rw.Children) != 2 {
		return false, fmt.Errorf("%w: exclusion needs exactly two operands", ErrSchemaInvalid)
	}

	base, bidx, err := r.rewrite(rw.Children[0], obj, rel)
	r.rec.addChild(idx, bidx)

	if err != nil {
		return false, err
	}

	if !base {
		return false, nil
	}

	sub, sidx, err := r.rewrite(rw.Children[1], obj, rel)
	r.rec.addChild(idx, sidx)

	if err != nil {
		return false, err
	}

	return !sub, nil
}
