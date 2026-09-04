package core

import (
	"context"
	"errors"
)

// ErrShortBuffer reports that the out slice passed to HasWildcard is shorter than reqs.
var ErrShortBuffer = errors.New("core: result buffer shorter than request slice")

// LookupRequest asks what a stored relation says about one concrete subject.
type LookupRequest struct {
	Object   ObjectRef
	Relation RelationID
	Subject  SubjectRef
}

// LookupResult answers a LookupRequest.
type LookupResult struct {
	Direct   bool         // the subject appears as itself
	Usersets []SubjectRef // entries to descend into, such as team:eng#member
}

// WildcardRequest asks whether a relation is granted to a whole subject type.
type WildcardRequest struct {
	Object      ObjectRef
	Relation    RelationID
	SubjectType TypeID
}

// TupleSource is the evaluator's view of stored tuples.
// Implementations must be safe for concurrent use, so a buffer reused between answers needs one per call.
type TupleSource interface {
	// Lookup answers reqs, calling yield with each request's index and answer, in
	// any order; returning false ends the round and Lookup returns nil.
	// Usersets is valid only until yield returns, and yield may run under the source's locks:
	// copy what you keep, never call back in.
	Lookup(ctx context.Context, reqs []LookupRequest, yield func(i int, res LookupResult) bool) error

	// HasWildcard reports, per request by index, whether a wildcard tuple grants the relation to the whole of SubjectType.
	// out must be at least as long as reqs, and every entry is overwritten so a reused buffer shows no stale true.
	HasWildcard(ctx context.Context, reqs []WildcardRequest, out []bool) error
}
