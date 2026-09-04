package core

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// stubSource keeps the interface honest: if the shape stops being implementable
// without copying a whole subject set, this stops compiling comfortably.
type stubSource struct {
	subjects map[stubKey][]SubjectRef

	// Reused between answers, as the contract allows, so a caller that forgets to copy inside yield sees the reuse.
	// A real source needs one buffer per call.
	scratch []SubjectRef
}

type stubKey struct {
	Object   ObjectRef
	Relation RelationID
}

var _ TupleSource = (*stubSource)(nil)

func (s *stubSource) Lookup(ctx context.Context, reqs []LookupRequest, yield func(int, LookupResult) bool) error {
	for i, req := range reqs {
		if err := ctx.Err(); err != nil {
			return err
		}

		if !yield(i, s.classify(req)) {
			return nil
		}
	}

	return nil
}

// classify never reports a wildcard; that question belongs to HasWildcard.
func (s *stubSource) classify(req LookupRequest) LookupResult {
	var res LookupResult

	s.scratch = s.scratch[:0]

	for _, subj := range s.subjects[stubKey{req.Object, req.Relation}] {
		switch {
		// Equality first: a userset subject is answered by the tuple naming it, not by descending into it.
		// Stored wildcards match neither case.
		case !subj.Wildcard && subj == req.Subject:
			res.Direct = true
		case subj.Relation != NoRelation:
			s.scratch = append(s.scratch, subj)
		}
	}

	res.Usersets = s.scratch

	return res
}

func (s *stubSource) HasWildcard(ctx context.Context, reqs []WildcardRequest, out []bool) error {
	if len(out) < len(reqs) {
		return ErrShortBuffer
	}

	for i, req := range reqs {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Written once per request, never conditionally, which is both the
		// overwrite contract and the form gosec can prove in range.
		out[i] = s.covered(req)
	}

	return nil
}

func (s *stubSource) covered(req WildcardRequest) bool {
	for _, subj := range s.subjects[stubKey{req.Object, req.Relation}] {
		if subj.Wildcard && subj.Type == req.SubjectType {
			return true
		}
	}

	return false
}

// document:d1#viewer holds alice, user:*, and team:eng#member.
func newStub() (*stubSource, ObjectRef, RelationID, SubjectRef) {
	const (
		userType   TypeID     = 1
		teamType   TypeID     = 2
		docType    TypeID     = 3
		viewer     RelationID = 1
		member     RelationID = 2
		aliceID    ObjectID   = 1
		engID      ObjectID   = 2
		documentID ObjectID   = 3
	)

	doc := ObjectRef{Type: docType, ID: documentID}
	alice := SubjectRef{Type: userType, ID: aliceID}

	return &stubSource{
		subjects: map[stubKey][]SubjectRef{
			{doc, viewer}: {
				alice,
				WildcardSubject(userType),
				{Type: teamType, ID: engID, Relation: member},
			},
		},
	}, doc, viewer, alice
}

// collect copies inside yield, the way a caller must.
func collect(t *testing.T, src TupleSource, reqs []LookupRequest) []LookupResult {
	t.Helper()

	out := make([]LookupResult, len(reqs))

	err := src.Lookup(t.Context(), reqs, func(i int, res LookupResult) bool {
		res.Usersets = slices.Clone(res.Usersets)
		out[i] = res

		return true
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	return out
}

func TestLookupClassifiesSubjects(t *testing.T) {
	t.Parallel()

	src, doc, viewer, alice := newStub()

	res := collect(t, src, []LookupRequest{{doc, viewer, alice}})[0]

	if !res.Direct {
		t.Error("Direct = false; alice is named outright")
	}

	if len(res.Usersets) != 1 {
		t.Fatalf("got %d usersets; want 1", len(res.Usersets))
	}

	if res.Usersets[0].Relation == NoRelation {
		t.Error("a userset came back without a relation")
	}

	for _, subj := range res.Usersets {
		if subj.Wildcard {
			t.Error("a wildcard leaked into Usersets")
		}
	}
}

// Ordering the userset case first would make this direct hit unreachable, and
// the evaluator would hunt team:eng#member among team:eng's members.
func TestLookupUsersetSubjectIsDirect(t *testing.T) {
	t.Parallel()

	src, doc, viewer, _ := newStub()

	engMember := SubjectRef{Type: 2, ID: 2, Relation: 2} // team:eng#member, the stored subject

	res := collect(t, src, []LookupRequest{{doc, viewer, engMember}})[0]

	if !res.Direct {
		t.Error("Direct = false; the request names exactly the stored subject")
	}
}

// Without the buffer reset, usersets pile onto later requests and the evaluator
// descends into groups the tuple never named.
func TestLookupUsersetsDoNotAccumulate(t *testing.T) {
	t.Parallel()

	src, doc, viewer, alice := newStub()

	reqs := []LookupRequest{{doc, viewer, alice}, {doc, viewer, alice}, {doc, viewer, alice}}

	err := src.Lookup(t.Context(), reqs, func(i int, res LookupResult) bool {
		if len(res.Usersets) != 1 {
			t.Errorf("request %d: got %d usersets; want 1", i, len(res.Usersets))
		}

		return true
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
}

// A forbidden wildcard subject must answer "not found", not a direct hit off the stored user:*.
func TestLookupWildcardSubjectIsNotDirect(t *testing.T) {
	t.Parallel()

	src, doc, viewer, alice := newStub()

	res := collect(t, src, []LookupRequest{{doc, viewer, WildcardSubject(alice.Type)}})[0]

	if res.Direct {
		t.Error("Direct = true; the stored wildcard must not answer a wildcard request here")
	}
}

// Results carry their request's index, because a source may answer out of order.
func TestLookupResultsAlignByIndex(t *testing.T) {
	t.Parallel()

	src, doc, viewer, alice := newStub()

	missing := ObjectRef{Type: doc.Type, ID: 999}
	reqs := []LookupRequest{
		{missing, viewer, alice},
		{doc, viewer, alice},
		{missing, viewer, alice},
	}

	out := collect(t, src, reqs)

	if out[0].Direct || out[2].Direct {
		t.Error("a request for an unknown object came back found")
	}

	if !out[1].Direct {
		t.Error("the middle request lost its answer")
	}
}

// Without this a union node waits for its slowest sibling.
func TestLookupYieldStopsRound(t *testing.T) {
	t.Parallel()

	src, doc, viewer, alice := newStub()

	reqs := []LookupRequest{{doc, viewer, alice}, {doc, viewer, alice}, {doc, viewer, alice}}

	answered := 0

	err := src.Lookup(t.Context(), reqs, func(int, LookupResult) bool {
		answered++

		return false
	})
	if err != nil {
		t.Fatalf("Lookup: %v; stopping early is not a failure", err)
	}

	if answered != 1 {
		t.Errorf("yield was called %d times; want 1, the round should stop on false", answered)
	}
}

func TestLookupHonoursCancellation(t *testing.T) {
	t.Parallel()

	src, doc, viewer, alice := newStub()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := src.Lookup(ctx, []LookupRequest{{doc, viewer, alice}}, func(int, LookupResult) bool {
		t.Error("yield ran on a cancelled context")

		return false
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want context.Canceled", err)
	}
}

func TestHasWildcardIsTypeScoped(t *testing.T) {
	t.Parallel()

	src, doc, viewer, alice := newStub()

	reqs := []WildcardRequest{
		{doc, viewer, alice.Type},                      // user:* is stored
		{doc, viewer, 99},                              // no wildcard for this type
		{ObjectRef{doc.Type, 999}, viewer, alice.Type}, // unknown object
	}

	out := make([]bool, len(reqs))
	if err := src.HasWildcard(t.Context(), reqs, out); err != nil {
		t.Fatalf("HasWildcard: %v", err)
	}

	if !out[0] {
		t.Error("out[0] = false; user:* covers alice's type")
	}

	if out[1] {
		t.Error("out[1] = true; user:* must not cover another type")
	}

	if out[2] {
		t.Error("out[2] = true; the object holds no tuples at all")
	}
}

// An implementation writing only the true entries would leave a stale true in a reused buffer, which reads as a grant.
func TestHasWildcardOverwritesBuffer(t *testing.T) {
	t.Parallel()

	src, doc, viewer, _ := newStub()

	out := []bool{true}

	if err := src.HasWildcard(t.Context(), []WildcardRequest{{doc, viewer, 99}}, out); err != nil {
		t.Fatalf("HasWildcard: %v", err)
	}

	if out[0] {
		t.Error("out[0] = true; a stale entry survived a request with no wildcard")
	}
}

func TestHasWildcardShortBuffer(t *testing.T) {
	t.Parallel()

	src, doc, viewer, alice := newStub()

	reqs := []WildcardRequest{{doc, viewer, alice.Type}, {doc, viewer, alice.Type}}

	err := src.HasWildcard(t.Context(), reqs, make([]bool, 1))
	if !errors.Is(err, ErrShortBuffer) {
		t.Errorf("err = %v; want ErrShortBuffer", err)
	}
}
