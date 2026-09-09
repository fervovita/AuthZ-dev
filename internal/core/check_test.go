package core

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
)

// Interned ids the evaluator tests share.
const (
	tDoc  TypeID = 1
	tTeam TypeID = 2
	tUser TypeID = 3

	rViewer RelationID = 1
	rOwner  RelationID = 2
	rBanned RelationID = 3
	rMember RelationID = 4
	rView   RelationID = 5
	rEditor RelationID = 6

	oD1    ObjectID = 1
	oEng   ObjectID = 2
	oAlice ObjectID = 3
	oBob   ObjectID = 4
)

func docRel(rel RelationID) RelationRef  { return RelationRef{Type: tDoc, Relation: rel} }
func teamRel(rel RelationID) RelationRef { return RelationRef{Type: tTeam, Relation: rel} }

// document: viewer, owner and banned are this.
// view = (viewer | owner) - banned.
// team: member = this.
func validSchema(t *testing.T) *Schema {
	t.Helper()

	s, err := NewSchemaBuilder().
		Define(docRel(rViewer), This()).
		Define(docRel(rOwner), This()).
		Define(docRel(rBanned), This()).
		Define(docRel(rView), Exclusion(
			Union(ComputedUserset(rViewer), ComputedUserset(rOwner)),
			ComputedUserset(rBanned),
		)).
		Define(teamRel(rMember), This()).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return s
}

func doc(id ObjectID) ObjectRef         { return ObjectRef{Type: tDoc, ID: id} }
func team(id ObjectID) ObjectRef        { return ObjectRef{Type: tTeam, ID: id} }
func user(id ObjectID) SubjectRef       { return SubjectRef{Type: tUser, ID: id} }
func teamMember(id ObjectID) SubjectRef { return SubjectRef{Type: tTeam, ID: id, Relation: rMember} }
func anyUser() SubjectRef               { return WildcardSubject(tUser) }

// storeBuilder assembles the map stubSource reads.
type storeBuilder struct{ m map[stubKey][]SubjectRef }

func store() *storeBuilder { return &storeBuilder{m: make(map[stubKey][]SubjectRef)} }

func (b *storeBuilder) add(obj ObjectRef, rel RelationID, subj SubjectRef) *storeBuilder {
	k := stubKey{Object: obj, Relation: rel}
	b.m[k] = append(b.m[k], subj)

	return b
}

func (b *storeBuilder) source() *stubSource { return &stubSource{subjects: b.m} }

func engine(t *testing.T, s *Schema, src TupleSource, opts ...Option) *Engine {
	t.Helper()

	e, err := NewEngine(s, src, opts...)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	return e
}

func check(t *testing.T, e *Engine, obj ObjectRef, rel RelationID, subj SubjectRef) CheckResult {
	t.Helper()

	res, err := e.Check(t.Context(), CheckRequest{
		Object: obj, Relation: rel, Subject: subj, Proof: ProofJustify,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	return res
}

func TestCheckDirectAndDenied(t *testing.T) {
	t.Parallel()

	e := engine(t, validSchema(t), store().add(doc(oD1), rViewer, user(oAlice)).source())

	if !check(t, e, doc(oD1), rViewer, user(oAlice)).Allowed {
		t.Error("alice is stored as a viewer")
	}

	if check(t, e, doc(oD1), rViewer, user(oBob)).Allowed {
		t.Error("bob is not stored anywhere")
	}
}

// A wildcard grants to every subject of the type, and only that type.
func TestCheckWildcard(t *testing.T) {
	t.Parallel()

	e := engine(t, validSchema(t), store().add(doc(oD1), rViewer, anyUser()).source())

	if !check(t, e, doc(oD1), rViewer, user(oBob)).Allowed {
		t.Error("user:* covers every user")
	}

	other := SubjectRef{Type: tTeam, ID: oEng}
	if check(t, e, doc(oD1), rViewer, other).Allowed {
		t.Error("user:* must not cover a team subject")
	}
}

// A public resource is answered without ever reaching the tuple lookup.
func TestCheckAsksWildcardBeforeLookup(t *testing.T) {
	t.Parallel()

	log := &callLog{stubSource: store().add(doc(oD1), rViewer, anyUser()).source()}
	e := engine(t, validSchema(t), log)

	if !check(t, e, doc(oD1), rViewer, user(oAlice)).Allowed {
		t.Fatal("user:* covers alice")
	}

	if len(log.calls) != 1 || log.calls[0] != "wildcard" {
		t.Errorf("calls = %v; want the wildcard question alone", log.calls)
	}
}

func TestCheckUserset(t *testing.T) {
	t.Parallel()

	src := store().
		add(doc(oD1), rViewer, teamMember(oEng)).
		add(team(oEng), rMember, user(oAlice)).
		source()

	e := engine(t, validSchema(t), src)

	if !check(t, e, doc(oD1), rViewer, user(oAlice)).Allowed {
		t.Error("alice is a member of the team the document names")
	}

	if check(t, e, doc(oD1), rViewer, user(oBob)).Allowed {
		t.Error("bob is not a member")
	}
}

func TestCheckUnionAndComputedUserset(t *testing.T) {
	t.Parallel()

	// view is (viewer | owner) - banned; alice is only an owner.
	src := store().add(doc(oD1), rOwner, user(oAlice)).source()
	e := engine(t, validSchema(t), src)

	if !check(t, e, doc(oD1), rView, user(oAlice)).Allowed {
		t.Error("the owner branch of the union should satisfy view")
	}
}

func TestCheckExclusion(t *testing.T) {
	t.Parallel()

	s := validSchema(t)

	t.Run("subtract matches", func(t *testing.T) {
		t.Parallel()

		src := store().
			add(doc(oD1), rViewer, user(oAlice)).
			add(doc(oD1), rBanned, user(oAlice)).
			source()

		if check(t, engine(t, s, src), doc(oD1), rView, user(oAlice)).Allowed {
			t.Error("a banned viewer must not hold view")
		}
	})

	t.Run("base fails", func(t *testing.T) {
		t.Parallel()

		src := store().add(doc(oD1), rBanned, user(oAlice)).source()

		if check(t, engine(t, s, src), doc(oD1), rView, user(oAlice)).Allowed {
			t.Error("no positive grant, so view cannot hold")
		}
	})
}

func TestCheckIntersection(t *testing.T) {
	t.Parallel()

	s, err := NewSchemaBuilder().
		Define(docRel(rViewer), This()).
		Define(docRel(rOwner), This()).
		Define(docRel(rView), Intersection(ComputedUserset(rViewer), ComputedUserset(rOwner))).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	both := store().add(doc(oD1), rViewer, user(oAlice)).add(doc(oD1), rOwner, user(oAlice)).source()
	if !check(t, engine(t, s, both), doc(oD1), rView, user(oAlice)).Allowed {
		t.Error("alice satisfies both operands")
	}

	one := store().add(doc(oD1), rViewer, user(oAlice)).source()
	if check(t, engine(t, s, one), doc(oD1), rView, user(oAlice)).Allowed {
		t.Error("one operand is not enough for an intersection")
	}
}

// Mutually nested groups must terminate.
// The answer is false, and the proof says it came from a cycle rather than from missing data.
func TestCheckCycleTerminates(t *testing.T) {
	t.Parallel()

	src := store().
		add(team(oEng), rMember, teamMember(oAlice)).
		add(team(oAlice), rMember, teamMember(oEng)).
		source()

	res := check(t, engine(t, validSchema(t), src), team(oEng), rMember, user(oBob))

	if res.Allowed {
		t.Fatal("bob is in neither group")
	}

	var sawCycle bool

	for _, n := range res.Proof.Nodes {
		if n.Outcome == OutcomeCycle {
			sawCycle = true
		}
	}

	if !sawCycle {
		t.Errorf("no OutcomeCycle in %d nodes; the cycle should be visible", len(res.Proof.Nodes))
	}
}

// A denial reached while an outer relation is still being computed says "not yet", not "no".
// Publishing it made the engine call both operands of an intersection true and the intersection false.
func TestCheckAnswersARecursiveComponentConsistently(t *testing.T) {
	t.Parallel()

	// view = viewer AND editor
	s, err := NewSchemaBuilder().
		Define(docRel(rViewer), This()).
		Define(docRel(rEditor), This()).
		Define(docRel(rView), Intersection(ComputedUserset(rViewer), ComputedUserset(rEditor))).
		Define(teamRel(rMember), This()).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	const (
		oA ObjectID = 10
		oB ObjectID = 11
		oC ObjectID = 12
	)

	// a = b | c, b = a, c = {alice}.
	// Least fixed point: alice is in all three.
	// Reaching a through b first cuts the cycle, so b's denial is the provisional one.
	src := func() TupleSource {
		return store().
			add(doc(oD1), rViewer, teamMember(oA)).
			add(doc(oD1), rEditor, teamMember(oB)).
			add(team(oA), rMember, teamMember(oB)).
			add(team(oA), rMember, teamMember(oC)).
			add(team(oB), rMember, teamMember(oA)).
			add(team(oC), rMember, user(oAlice)).
			source()
	}

	for _, c := range []struct {
		name string
		obj  ObjectRef
		rel  RelationID
	}{
		{"team:b#member", team(oB), rMember},
		{"document:d1#viewer", doc(oD1), rViewer},
		{"document:d1#editor", doc(oD1), rEditor},
		{"document:d1#view", doc(oD1), rView},
	} {
		if !check(t, engine(t, s, src()), c.obj, c.rel, user(oAlice)).Allowed {
			t.Errorf("%s@alice = false; alice is in a, b and c", c.name)
		}
	}
}

// Settling publishes a component's answers, so a second branch reaching the same groups
// reads them instead of walking them again.
func TestCheckReusesASettledComponent(t *testing.T) {
	t.Parallel()

	s, err := NewSchemaBuilder().
		Define(docRel(rViewer), This()).
		Define(docRel(rOwner), This()).
		Define(docRel(rView), Union(ComputedUserset(rViewer), ComputedUserset(rOwner))).
		Define(teamRel(rMember), This()).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Both branches of the union reach the same pair of groups, which hold each other and, through a third group, bob.
	// The grant is what makes the pair worth settling.
	log := &callLog{stubSource: store().
		add(doc(oD1), rViewer, teamMember(oEng)).
		add(doc(oD1), rOwner, teamMember(oEng)).
		add(team(oEng), rMember, teamMember(oAlice)).
		add(team(oEng), rMember, teamMember(oD1)).
		add(team(oAlice), rMember, teamMember(oEng)).
		add(team(oD1), rMember, user(oBob)).
		source()}

	if !check(t, engine(t, s, log), doc(oD1), rView, user(oBob)).Allowed {
		t.Fatal("bob reaches the pair through the third group")
	}

	// viewer, the three groups, and one settling round over team:alice.
	// owner then reads the settled answers; re-walking the pair for it would make 8.
	if got := countCalls(log.calls, "lookup"); got != 5 {
		t.Errorf("%d lookups; want 5, the settled component should not be walked again", got)
	}
}

// A component that grants nobody is already at its least fixed point, so there is nothing
// for a round to raise and the rounds are skipped outright.
func TestCheckDoesNotSettleAComponentThatGrantsNothing(t *testing.T) {
	t.Parallel()

	s, err := NewSchemaBuilder().Define(teamRel(rMember), This()).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	log := &callLog{stubSource: store().
		add(team(oEng), rMember, teamMember(oAlice)).
		add(team(oAlice), rMember, teamMember(oEng)).
		source()}

	if check(t, engine(t, s, log), team(oEng), rMember, user(oBob)).Allowed {
		t.Fatal("bob is in neither group")
	}

	// One lookup per group and no rounds at all. Settling them anyway would make 4.
	if got := countCalls(log.calls, "lookup"); got != 2 {
		t.Errorf("%d lookups; want 2, a component that grants nothing needs no rounds", got)
	}
}

// Memoising inside a cycle is the one part of the evaluator whose correctness is not
// visible by reading it, so it is checked against the definition instead.
func TestCheckAgreesWithTheFixedPoint(t *testing.T) {
	t.Parallel()

	rels := []RelationID{rViewer, rOwner, rBanned, rMember}
	objs := []ObjectRef{team(1), team(2), team(3), team(4)}
	subj := user(oAlice)

	var pairs []memoKey

	for _, o := range objs {
		for _, rel := range rels {
			pairs = append(pairs, memoKey{Object: o, Relation: rel})
		}
	}

	for seed := range 3000 {
		//nolint:gosec // G404: a failing case has to be reproducible from its seed
		rnd := rand.New(rand.NewPCG(uint64(seed), 0x5eed))

		s := randomSchema(t, rnd, rels)
		tuples := randomTuples(rnd, objs, rels, subj)
		want := fixedPoint(t, s, tuples, subj, pairs)

		// A key on the stack is already being computed, so the descent cannot outrun them.
		e := engine(t, s, &stubSource{subjects: tuples}, WithMaxDepth(len(pairs)+1))

		for _, p := range pairs {
			if got := check(t, e, p.Object, p.Relation, subj); got.Allowed != want[p] {
				t.Fatalf("seed %d: %+v = %v; the fixed point says %v", seed, p, got.Allowed, want[p])
			}
		}
	}
}

func randomSchema(t *testing.T, rnd *rand.Rand, rels []RelationID) *Schema {
	t.Helper()

	b := NewSchemaBuilder()
	for _, rel := range rels {
		b.Define(teamRel(rel), randomRewrite(rnd, rels, 2))
	}

	s, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return s
}

// Exclusion is left out: stratification already forbids it inside a cycle, and a naive
// iteration is only a fixed point while every operator is monotone.
func randomRewrite(rnd *rand.Rand, rels []RelationID, budget int) Rewrite {
	if budget == 0 || rnd.IntN(3) == 0 {
		if rnd.IntN(2) == 0 {
			return This()
		}

		return ComputedUserset(rels[rnd.IntN(len(rels))])
	}

	children := []Rewrite{
		randomRewrite(rnd, rels, budget-1),
		randomRewrite(rnd, rels, budget-1),
	}

	if rnd.IntN(2) == 0 {
		return Union(children...)
	}

	return Intersection(children...)
}

func randomTuples(rnd *rand.Rand, objs []ObjectRef, rels []RelationID, subj SubjectRef) map[stubKey][]SubjectRef {
	tuples := make(map[stubKey][]SubjectRef)

	for _, o := range objs {
		for _, rel := range rels {
			k := stubKey{Object: o, Relation: rel}

			for range rnd.IntN(3) {
				switch rnd.IntN(5) {
				case 0:
					tuples[k] = append(tuples[k], subj)
				case 1:
					tuples[k] = append(tuples[k], WildcardSubject(subj.Type))
				default:
					other := objs[rnd.IntN(len(objs))]
					tuples[k] = append(tuples[k], SubjectRef{
						Type: other.Type, ID: other.ID, Relation: rels[rnd.IntN(len(rels))],
					})
				}
			}
		}
	}

	return tuples
}

// fixedPoint holds every key the schema entails for one subject, by iterating from nothing held until a round changes nothing.
func fixedPoint(t *testing.T, s *Schema, tuples map[stubKey][]SubjectRef, subj SubjectRef, pairs []memoKey) map[memoKey]bool {
	t.Helper()

	held := make(map[memoKey]bool, len(pairs))

	for changed := true; changed; {
		changed = false

		for _, p := range pairs {
			rw, ok := s.Rewrite(RelationRef{Type: p.Object.Type, Relation: p.Relation})
			if !ok {
				continue
			}

			if v := entails(t, tuples, subj, held, rw, p.Object, p.Relation); v != held[p] {
				held[p] = v
				changed = true
			}
		}
	}

	return held
}

func entails(t *testing.T, tuples map[stubKey][]SubjectRef, subj SubjectRef,
	held map[memoKey]bool, rw Rewrite, obj ObjectRef, rel RelationID,
) bool {
	t.Helper()

	switch rw.Op {
	case OpThis:
		return storedEntails(tuples[stubKey{obj, rel}], subj, held)

	case OpComputedUserset:
		return held[memoKey{Object: obj, Relation: rw.Relation}]

	case OpUnion:
		for _, c := range rw.Children {
			if entails(t, tuples, subj, held, c, obj, rel) {
				return true
			}
		}

		return false

	case OpIntersection:
		for _, c := range rw.Children {
			if !entails(t, tuples, subj, held, c, obj, rel) {
				return false
			}
		}

		return true

	case OpExclusion, OpTupleToUserset:
		t.Fatalf("the reference does not model %s", rw.Op)
	}

	t.Fatalf("the reference does not model %s", rw.Op)

	return false
}

func storedEntails(stored []SubjectRef, subj SubjectRef, held map[memoKey]bool) bool {
	for _, s := range stored {
		switch {
		case s.Wildcard:
			if subj.Relation == NoRelation && s.Type == subj.Type {
				return true
			}
		case s == subj:
			return true
		case s.Relation != NoRelation:
			if held[memoKey{Object: ObjectRef{Type: s.Type, ID: s.ID}, Relation: s.Relation}] {
				return true
			}
		}
	}

	return false
}

// A bound that answered "no" would invent denials, so it errors, and still returns what it managed to record.
func TestCheckMaxDepthErrorsWithProof(t *testing.T) {
	t.Parallel()

	b := store()
	for i := ObjectID(1); i < 8; i++ {
		b.add(team(i), rMember, teamMember(i+1))
	}

	e := engine(t, validSchema(t), b.source(), WithMaxDepth(3))

	res, err := e.Check(t.Context(), CheckRequest{
		Object: team(1), Relation: rMember, Subject: user(oAlice), Proof: ProofJustify,
	})

	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("err = %v; want ErrMaxDepthExceeded", err)
	}

	if len(res.Proof.Nodes) == 0 {
		t.Fatal("the proof is empty; a depth bound should still say where it stopped")
	}

	// Saying where it stopped means the path to that point is walkable from the root.
	if n := len(reachableNodes(res.Proof)); n != len(res.Proof.Nodes) {
		t.Errorf("%d of %d nodes reachable from the root:\n%s",
			n, len(res.Proof.Nodes), formatProof(res.Proof))
	}

	last := res.Proof.Nodes[len(res.Proof.Nodes)-1]
	if last.Outcome != OutcomeIncomplete {
		t.Errorf("the deepest node = %s; want incomplete", last.Outcome)
	}
}

// A relation reached twice in one check is evaluated once; the second is marked.
func TestCheckMemoizesRepeatedRelation(t *testing.T) {
	t.Parallel()

	// view = viewer | owner, and owner is defined as viewer, so viewer is reached twice.
	s, err := NewSchemaBuilder().
		Define(docRel(rViewer), This()).
		Define(docRel(rOwner), ComputedUserset(rViewer)).
		Define(docRel(rView), Union(ComputedUserset(rViewer), ComputedUserset(rOwner))).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	log := &callLog{stubSource: store().source()}
	res := check(t, engine(t, s, log), doc(oD1), rView, user(oAlice))

	if res.Allowed {
		t.Fatal("nothing is stored")
	}

	if got := countCalls(log.calls, "lookup"); got != 1 {
		t.Errorf("%d lookups; viewer should be resolved once and reused", got)
	}

	var memoized int

	for _, n := range res.Proof.Nodes {
		if n.Memoized {
			memoized++
		}
	}

	if memoized == 0 {
		t.Error("no node marked Memoized; a reused answer should say so")
	}
}

func TestCheckProofNoneRecordsNothing(t *testing.T) {
	t.Parallel()

	e := engine(t, validSchema(t), store().add(doc(oD1), rViewer, user(oAlice)).source())

	res, err := e.Check(t.Context(), CheckRequest{
		Object: doc(oD1), Relation: rViewer, Subject: user(oAlice), Proof: ProofNone,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if !res.Allowed {
		t.Error("the decision must not depend on whether a proof was asked for")
	}

	if len(res.Proof.Nodes) != 0 || res.Proof.Truncated {
		t.Errorf("recorded %d nodes with ProofNone", len(res.Proof.Nodes))
	}
}

// The proof records every branch that was tried, including the ones that failed,
// so a reader can see what was considered and not just what won.
func TestCheckProofRecordsTriedBranches(t *testing.T) {
	t.Parallel()

	s, err := NewSchemaBuilder().
		Define(docRel(rViewer), This()).
		Define(docRel(rOwner), This()).
		Define(docRel(rView), Union(ComputedUserset(rViewer), ComputedUserset(rOwner))).
		Define(teamRel(rMember), This()).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// viewer descends into a team that does not hold alice; owner names her directly.
	src := store().
		add(doc(oD1), rViewer, teamMember(oEng)).
		add(team(oEng), rMember, user(oBob)).
		add(doc(oD1), rOwner, user(oAlice)).
		source()

	res := check(t, engine(t, s, src), doc(oD1), rView, user(oAlice))
	if !res.Allowed {
		t.Fatal("alice is an owner")
	}

	// union, computed(viewer), this(viewer), this(team member), computed(owner), this(owner).
	if len(res.Proof.Nodes) != 6 {
		t.Fatalf("got %d nodes; want 6:\n%s", len(res.Proof.Nodes), formatProof(res.Proof))
	}

	if got := res.Proof.Nodes[0].Outcome; got != OutcomeAllowed {
		t.Errorf("root outcome = %s; want allowed", got)
	}

	if got := res.Proof.Nodes[1].Outcome; got != OutcomeDenied {
		t.Errorf("the viewer branch = %s; want denied, and recorded", got)
	}

	if res.Proof.Subject != user(oAlice) {
		t.Errorf("Proof.Subject = %+v; the subject is constant for a check", res.Proof.Subject)
	}
}

func TestCheckRejectsMalformedRequests(t *testing.T) {
	t.Parallel()

	e := engine(t, validSchema(t), store().source())

	cases := []struct {
		name string
		req  CheckRequest
	}{
		{"zero object", CheckRequest{Relation: rViewer, Subject: user(oAlice)}},
		{"zero relation", CheckRequest{Object: doc(oD1), Subject: user(oAlice)}},
		{"zero subject", CheckRequest{Object: doc(oD1), Relation: rViewer}},
		{"wildcard subject", CheckRequest{Object: doc(oD1), Relation: rViewer, Subject: anyUser()}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if _, err := e.Check(t.Context(), c.req); !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err = %v; want ErrInvalidRequest", err)
			}
		})
	}
}

func TestCheckUndefinedRelation(t *testing.T) {
	t.Parallel()

	e := engine(t, validSchema(t), store().source())

	_, err := e.Check(t.Context(), CheckRequest{
		Object: doc(oD1), Relation: rEditor, Subject: user(oAlice),
	})

	if !errors.Is(err, ErrRelationUndefined) {
		t.Errorf("err = %v; want ErrRelationUndefined", err)
	}
}

// A source failure is an error, never a denial.
func TestCheckPropagatesSourceError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("source is unreachable")

	for _, onWildcard := range []bool{true, false} {
		src := &failingSource{
			stubSource: store().source(),
			err:        sentinel,
			onWildcard: onWildcard,
		}

		res, err := engine(t, validSchema(t), src).Check(t.Context(), CheckRequest{
			Object: doc(oD1), Relation: rViewer, Subject: user(oAlice),
		})

		if !errors.Is(err, sentinel) {
			t.Errorf("onWildcard=%v: err = %v; want the source error", onWildcard, err)
		}

		if res.Allowed {
			t.Errorf("onWildcard=%v: Allowed = true on a failed lookup", onWildcard)
		}
	}
}

// The TupleSource contract does not require watching ctx, so the engine must.
func TestCheckHonoursCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res, err := engine(t, validSchema(t), ctxBlindSource{}).
		Check(ctx, CheckRequest{Object: doc(oD1), Relation: rViewer, Subject: user(oAlice)})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want context.Canceled", err)
	}

	if res.Allowed {
		t.Error("Allowed = true on a cancelled check")
	}
}

// A wildcard names every object of a type, which a userset subject is not one of.
func TestCheckWildcardDoesNotCoverAUsersetSubject(t *testing.T) {
	t.Parallel()

	src := store().add(doc(oD1), rViewer, WildcardSubject(tTeam)).source()

	if check(t, engine(t, validSchema(t), src), doc(oD1), rViewer, teamMember(oEng)).Allowed {
		t.Error("team:* covered team:eng#member")
	}

	// The same wildcard still covers the concrete object.
	if !check(t, engine(t, validSchema(t), src), doc(oD1), rViewer, SubjectRef{Type: tTeam, ID: oEng}).Allowed {
		t.Error("team:* should cover team:eng itself")
	}
}

// Erroring on a dangling userset would turn every check on the object into a failure instead of a denial.
func TestCheckSkipsUsersetsTheSchemaDoesNotDefine(t *testing.T) {
	t.Parallel()

	// The schema has no team#member at all, but a tuple still points at one.
	s, err := NewSchemaBuilder().Define(docRel(rViewer), This()).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	src := store().
		add(doc(oD1), rViewer, teamMember(oEng)).
		add(team(oEng), rMember, user(oAlice)).
		source()

	res, err := engine(t, s, src).Check(t.Context(), CheckRequest{
		Object: doc(oD1), Relation: rViewer, Subject: user(oAlice),
	})
	if err != nil {
		t.Fatalf("Check: %v; a dangling userset is a dead branch, not a failure", err)
	}

	if res.Allowed {
		t.Error("an undefined relation granted; it can hold nobody")
	}
}

// The relation the caller asked about is a different matter: that one is their mistake.
func TestCheckUndefinedRootRelationStillErrors(t *testing.T) {
	t.Parallel()

	s, err := NewSchemaBuilder().Define(docRel(rViewer), This()).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	_, err = engine(t, s, store().source()).Check(t.Context(), CheckRequest{
		Object: team(oEng), Relation: rMember, Subject: user(oAlice),
	})
	if !errors.Is(err, ErrRelationUndefined) {
		t.Errorf("err = %v; want ErrRelationUndefined", err)
	}
}

// Build rejects these arities. Reaching one means a Schema was assembled some other way,
// where indexing blind would panic and an empty intersection would grant out of nothing.
func TestCheckRefusesMalformedCombinators(t *testing.T) {
	t.Parallel()

	for name, rw := range map[string]Rewrite{
		"exclusion with one operand":   {Op: OpExclusion, Children: []Rewrite{This()}},
		"exclusion with three":         {Op: OpExclusion, Children: []Rewrite{This(), This(), This()}},
		"intersection with no operand": {Op: OpIntersection},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := &Schema{rewrites: map[RelationRef]Rewrite{docRel(rViewer): rw}}
			src := store().add(doc(oD1), rViewer, user(oAlice)).source()

			res, err := engine(t, s, src).Check(t.Context(), CheckRequest{
				Object: doc(oD1), Relation: rViewer, Subject: user(oAlice),
			})

			if !errors.Is(err, ErrSchemaInvalid) {
				t.Errorf("err = %v; want ErrSchemaInvalid", err)
			}

			if res.Allowed {
				t.Error("a malformed operator granted")
			}
		})
	}
}

func TestNewEngineRejectsBadArguments(t *testing.T) {
	t.Parallel()

	s := validSchema(t)
	src := store().source()

	if _, err := NewEngine(nil, src); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("nil schema: err = %v", err)
	}

	if _, err := NewEngine(s, nil); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("nil source: err = %v", err)
	}

	if _, err := NewEngine(s, src, WithMaxDepth(0)); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("zero depth: err = %v", err)
	}
}

// A sidecar answers Checks from one Engine at once. Nothing in an Engine is written after
// NewEngine and every check builds its own run, which under -race is what this says.
func TestCheckIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	s, err := NewSchemaBuilder().
		Define(docRel(rViewer), This()).
		Define(docRel(rOwner), This()).
		Define(docRel(rView), Union(ComputedUserset(rViewer), ComputedUserset(rOwner))).
		Define(teamRel(rMember), This()).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A component worth settling, so the memo, the stack and the recorder are all in play.
	src := store().
		add(doc(oD1), rViewer, teamMember(oEng)).
		add(team(oEng), rMember, teamMember(oAlice)).
		add(team(oEng), rMember, teamMember(oD1)).
		add(team(oAlice), rMember, teamMember(oEng)).
		add(team(oD1), rMember, user(oBob)).
		source()

	e := engine(t, s, src)

	var wg sync.WaitGroup

	for range 32 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			res, err := e.Check(t.Context(), CheckRequest{
				Object: doc(oD1), Relation: rView, Subject: user(oBob), Proof: ProofJustify,
			})
			if err != nil {
				t.Errorf("Check: %v", err)

				return
			}

			if !res.Allowed {
				t.Error("bob reaches the pair through the third group")
			}
		}()
	}

	wg.Wait()
}

// callLog records which source questions were asked, in order.
type callLog struct {
	*stubSource

	calls []string
}

func (c *callLog) Lookup(ctx context.Context, reqs []LookupRequest, yield func(int, LookupResult) bool) error {
	c.calls = append(c.calls, "lookup")

	return c.stubSource.Lookup(ctx, reqs, yield)
}

func (c *callLog) HasWildcard(ctx context.Context, reqs []WildcardRequest, out []bool) error {
	c.calls = append(c.calls, "wildcard")

	return c.stubSource.HasWildcard(ctx, reqs, out)
}

func countCalls(calls []string, want string) int {
	n := 0

	for _, c := range calls {
		if c == want {
			n++
		}
	}

	return n
}

// ctxBlindSource never looks at ctx, which the TupleSource contract permits, and grants
// everything, so an engine that kept going would answer allowed rather than error.
type ctxBlindSource struct{}

func (ctxBlindSource) Lookup(_ context.Context, reqs []LookupRequest, yield func(int, LookupResult) bool) error {
	for i := range reqs {
		if !yield(i, LookupResult{Direct: true}) {
			return nil
		}
	}

	return nil
}

func (ctxBlindSource) HasWildcard(_ context.Context, reqs []WildcardRequest, out []bool) error {
	for i := range reqs {
		out[i] = true
	}

	return nil
}

// failingSource fails one of the two questions so both error paths are reachable.
type failingSource struct {
	*stubSource

	err        error
	onWildcard bool
}

func (f *failingSource) Lookup(ctx context.Context, reqs []LookupRequest, yield func(int, LookupResult) bool) error {
	if f.onWildcard {
		return f.stubSource.Lookup(ctx, reqs, yield)
	}

	return f.err
}

func (f *failingSource) HasWildcard(ctx context.Context, reqs []WildcardRequest, out []bool) error {
	if !f.onWildcard {
		return f.stubSource.HasWildcard(ctx, reqs, out)
	}

	return f.err
}

// An answer reused within a check is marked, whether it was an allow or a denial.
func TestCheckMemoizesAnAllow(t *testing.T) {
	t.Parallel()

	// view needs both operands, and owner is defined as viewer, so viewer is evaluated once and reused.
	s, err := NewSchemaBuilder().
		Define(docRel(rViewer), This()).
		Define(docRel(rOwner), ComputedUserset(rViewer)).
		Define(docRel(rView), Intersection(ComputedUserset(rViewer), ComputedUserset(rOwner))).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	src := store().add(doc(oD1), rViewer, user(oAlice)).source()

	res := check(t, engine(t, s, src), doc(oD1), rView, user(oAlice))
	if !res.Allowed {
		t.Fatal("alice satisfies both operands")
	}

	var memoized int

	for _, n := range res.Proof.Nodes {
		if n.Memoized && n.Outcome == OutcomeAllowed {
			memoized++
		}
	}

	if memoized == 0 {
		t.Errorf("no reused allow marked Memoized:\n%s", formatProof(res.Proof))
	}
}

// Build rejects these, so reaching them means a Schema was assembled some other way.
// The evaluator refuses rather than silently denying.
func TestCheckRefusesOperatorsBuildWouldReject(t *testing.T) {
	t.Parallel()

	for name, rw := range map[string]Rewrite{
		"tuple_to_userset": TupleToUserset(rOwner, rViewer),
		"unknown":          {Op: Op(99)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := &Schema{rewrites: map[RelationRef]Rewrite{docRel(rViewer): rw}}

			_, err := engine(t, s, store().source()).Check(t.Context(), CheckRequest{
				Object: doc(oD1), Relation: rViewer, Subject: user(oAlice),
			})

			if !errors.Is(err, ErrUnsupportedOp) {
				t.Errorf("err = %v; want ErrUnsupportedOp", err)
			}
		})
	}
}

// An entry without a relation, or a wildcard, is not a userset and is skipped.
func TestCheckSkipsNonUsersetEntries(t *testing.T) {
	t.Parallel()

	src := &oddUsersetSource{stubSource: store().source()}

	if check(t, engine(t, validSchema(t), src), doc(oD1), rViewer, user(oAlice)).Allowed {
		t.Error("neither entry is a userset, so nothing grants")
	}
}

// A source failure has to surface from inside every combinator, not just from a bare leaf.
func TestCheckPropagatesSourceErrorFromCombinators(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("source is unreachable")

	t.Run("union and exclusion base", func(t *testing.T) {
		t.Parallel()

		src := &relFailSource{stubSource: store().source(), failOn: rViewer, err: sentinel}

		_, err := engine(t, validSchema(t), src).Check(t.Context(), CheckRequest{
			Object: doc(oD1), Relation: rView, Subject: user(oAlice),
		})
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v; want the source error", err)
		}
	})

	t.Run("exclusion subtract", func(t *testing.T) {
		t.Parallel()

		// The base succeeds, so evaluation reaches the subtract before failing.
		src := &relFailSource{
			stubSource: store().add(doc(oD1), rViewer, user(oAlice)).source(),
			failOn:     rBanned,
			err:        sentinel,
		}

		_, err := engine(t, validSchema(t), src).Check(t.Context(), CheckRequest{
			Object: doc(oD1), Relation: rView, Subject: user(oAlice),
		})
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v; want the source error", err)
		}
	})

	t.Run("intersection", func(t *testing.T) {
		t.Parallel()

		s, err := NewSchemaBuilder().
			Define(docRel(rViewer), This()).
			Define(docRel(rOwner), This()).
			Define(docRel(rView), Intersection(ComputedUserset(rViewer), ComputedUserset(rOwner))).
			Build()
		if err != nil {
			t.Fatalf("Build: %v", err)
		}

		src := &relFailSource{stubSource: store().source(), failOn: rViewer, err: sentinel}

		_, err = engine(t, s, src).Check(t.Context(), CheckRequest{
			Object: doc(oD1), Relation: rView, Subject: user(oAlice),
		})
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v; want the source error", err)
		}
	})
}

// Settling re-walks from no node in particular, so recording it would litter the proof with nodes nothing points at.
func TestCheckProofStaysAWholeTreeAcrossAComponent(t *testing.T) {
	t.Parallel()

	s, err := NewSchemaBuilder().
		Define(docRel(rViewer), This()).
		Define(docRel(rEditor), This()).
		Define(docRel(rView), Intersection(ComputedUserset(rViewer), ComputedUserset(rEditor))).
		Define(teamRel(rMember), This()).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	const (
		oA ObjectID = 10
		oB ObjectID = 11
		oC ObjectID = 12
	)

	// a = b | c, b = a, c = {alice}: reaching a through b cuts the cycle, so the component
	// has to be settled before the intersection can be answered.
	src := store().
		add(doc(oD1), rViewer, teamMember(oA)).
		add(doc(oD1), rEditor, teamMember(oB)).
		add(team(oA), rMember, teamMember(oB)).
		add(team(oA), rMember, teamMember(oC)).
		add(team(oB), rMember, teamMember(oA)).
		add(team(oC), rMember, user(oAlice)).
		source()

	res := check(t, engine(t, s, src), doc(oD1), rView, user(oAlice))

	if n := len(reachableNodes(res.Proof)); n != len(res.Proof.Nodes) {
		t.Errorf("%d of %d nodes reachable from the root:\n%s",
			n, len(res.Proof.Nodes), formatProof(res.Proof))
	}

	// The answer and the node it is read from cannot disagree.
	if !res.Allowed || res.Proof.Nodes[0].Outcome != OutcomeAllowed {
		t.Errorf("Allowed = %v but the root node says %s", res.Allowed, res.Proof.Nodes[0].Outcome)
	}
}

// A settling round reads the source again, and a failure there is still an error.
func TestCheckPropagatesSourceErrorFromSettling(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("source is unreachable")

	s, err := NewSchemaBuilder().
		Define(docRel(rViewer), This()).
		Define(teamRel(rMember), This()).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Two groups holding each other, one of them granting through a third: the traversal
	// reads four relations, and the round that settles the pair reads the fifth.
	src := &countdownSource{
		stubSource: store().
			add(doc(oD1), rViewer, teamMember(oEng)).
			add(team(oEng), rMember, teamMember(oAlice)).
			add(team(oEng), rMember, teamMember(oD1)).
			add(team(oAlice), rMember, teamMember(oEng)).
			add(team(oD1), rMember, user(oBob)).
			source(),
		ok:  4,
		err: sentinel,
	}

	res, err := engine(t, s, src).Check(t.Context(), CheckRequest{
		Object: doc(oD1), Relation: rViewer, Subject: user(oBob),
	})

	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v; want the source error", err)
	}

	if res.Allowed {
		t.Error("Allowed = true on a failed lookup")
	}
}

// countdownSource answers ok lookups and fails after, which puts the failure in a
// settling round rather than in the traversal.
type countdownSource struct {
	*stubSource

	ok  int
	err error
}

func (c *countdownSource) Lookup(ctx context.Context, reqs []LookupRequest, yield func(int, LookupResult) bool) error {
	if c.ok == 0 {
		return c.err
	}

	c.ok--

	return c.stubSource.Lookup(ctx, reqs, yield)
}

// oddUsersetSource returns entries the contract says are not usersets.
type oddUsersetSource struct{ *stubSource }

func (o *oddUsersetSource) Lookup(_ context.Context, reqs []LookupRequest, yield func(int, LookupResult) bool) error {
	for i := range reqs {
		res := LookupResult{Usersets: []SubjectRef{
			{Type: tUser, ID: oBob}, // a plain object, not a userset
			WildcardSubject(tUser),
		}}

		if !yield(i, res) {
			return nil
		}
	}

	return nil
}

// relFailSource fails only the questions about one relation.
type relFailSource struct {
	*stubSource

	failOn RelationID
	err    error
}

func (f *relFailSource) Lookup(ctx context.Context, reqs []LookupRequest, yield func(int, LookupResult) bool) error {
	for _, req := range reqs {
		if req.Relation == f.failOn {
			return f.err
		}
	}

	return f.stubSource.Lookup(ctx, reqs, yield)
}

func (f *relFailSource) HasWildcard(ctx context.Context, reqs []WildcardRequest, out []bool) error {
	for _, req := range reqs {
		if req.Relation == f.failOn {
			return f.err
		}
	}

	return f.stubSource.HasWildcard(ctx, reqs, out)
}
