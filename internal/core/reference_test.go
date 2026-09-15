package core

import (
	"math/rand/v2"
	"testing"
)

// Memoizing inside a cycle is the one part of the evaluator whose correctness is not
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

	for seed := range 10000 {
		//nolint:gosec // G404: a failing case has to be reproducible from its seed
		rnd := rand.New(rand.NewPCG(uint64(seed), 0x5eed))

		s := randomSchema(t, rnd, rels)
		tuples := randomTuples(rnd, objs, rels, subj)
		want := fixedPoint(t, s, tuples, subj, pairs)

		// A frame is entered only for a key not seen yet, so nesting cannot exceed the key count.
		e := engine(t, s, &stubSource{subjects: tuples}, WithMaxDepth(len(pairs)+1))

		for _, p := range pairs {
			if got := check(t, e, p.Object, p.Relation, subj); got.Allowed != want[p] {
				t.Fatalf("seed %d: %+v = %v; the fixed point says %v", seed, p, got.Allowed, want[p])
			}
		}
	}
}

// The leading relations store tuples and the rest are permissions over them.
func randomSchema(t *testing.T, rnd *rand.Rand, rels []RelationID) *Schema {
	t.Helper()

	b := NewSchemaBuilder()

	// At least one relation, or nothing stores anything and every answer is false.
	stored := 1 + rnd.IntN(len(rels)-1)

	var tuplesets []RelationID

	for i, rel := range rels {
		if i >= stored {
			b.Permission(teamRel(rel), randomRewrite(rnd, rels, tuplesets, 2))

			continue
		}

		// An arrow needs a tupleset naming objects only, which a random subset of every shape rarely is.
		if rnd.IntN(3) == 0 {
			b.Relation(teamRel(rel), randomObjectTypes(rnd)...)
			tuplesets = append(tuplesets, rel)

			continue
		}

		b.Relation(teamRel(rel), randomTypes(rnd, rels)...)
	}

	s, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return s
}

// randomTypes draws from exactly what randomTuples writes, and takes a subset, so tuples the
// relation does not accept land on the generated path rather than only in a hand-written case.
func randomTypes(rnd *rand.Rand, rels []RelationID) []SubjectType {
	all := []SubjectType{DirectType(tUser), WildcardType(tUser), DirectType(tTeam)}
	for _, rel := range rels {
		all = append(all, UsersetType(tTeam, rel))
	}

	// One is picked outright, because a relation accepting nothing is rejected.
	out := []SubjectType{all[rnd.IntN(len(all))]}

	for _, st := range all {
		if st != out[0] && rnd.IntN(2) == 0 {
			out = append(out, st)
		}
	}

	return out
}

// randomObjectTypes shapes a tupleset: teams, which define every relation an arrow can name,
// and sometimes users, which define none and have to be passed over.
func randomObjectTypes(rnd *rand.Rand) []SubjectType {
	if rnd.IntN(2) == 0 {
		return []SubjectType{DirectType(tTeam), DirectType(tUser)}
	}

	return []SubjectType{DirectType(tTeam)}
}

// Exclusion is left out: stratification already forbids it inside a cycle, and a naive
// iteration is only a fixed point while every operator is monotone.
func randomRewrite(rnd *rand.Rand, rels, tuplesets []RelationID, budget int) Rewrite {
	if budget == 0 || rnd.IntN(3) == 0 {
		if len(tuplesets) > 0 && rnd.IntN(2) == 0 {
			return TupleToUserset(tuplesets[rnd.IntN(len(tuplesets))], rels[rnd.IntN(len(rels))])
		}

		return ComputedUserset(rels[rnd.IntN(len(rels))])
	}

	children := []Rewrite{
		randomRewrite(rnd, rels, tuplesets, budget-1),
		randomRewrite(rnd, rels, tuplesets, budget-1),
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

			for range rnd.IntN(6) {
				switch rnd.IntN(7) {
				case 0:
					tuples[k] = append(tuples[k], subj)
				case 1:
					tuples[k] = append(tuples[k], WildcardSubject(subj.Type))
				case 2, 3:
					tuples[k] = append(tuples[k], object(objs[rnd.IntN(len(objs))]))
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

			if v := entails(t, s, tuples, subj, held, rw, p.Object, p.Relation); v != held[p] {
				held[p] = v
				changed = true
			}
		}
	}

	return held
}

func entails(t *testing.T, s *Schema, tuples map[stubKey][]SubjectRef, subj SubjectRef,
	held map[memoKey]bool, rw Rewrite, obj ObjectRef, rel RelationID,
) bool {
	t.Helper()

	switch rw.Op {
	case OpThis:
		ref := RelationRef{Type: obj.Type, Relation: rel}

		return storedEntails(s, ref, tuples[stubKey{obj, rel}], subj, held)

	case OpComputedUserset:
		return held[memoKey{Object: obj, Relation: rw.Relation}]

	case OpTupleToUserset:
		ref := RelationRef{Type: obj.Type, Relation: rw.Tupleset}

		for _, st := range tuples[stubKey{obj, rw.Tupleset}] {
			if st.Wildcard || st.Relation != NoRelation || !s.Allows(ref, DirectType(st.Type)) {
				continue
			}

			if held[memoKey{Object: ObjectRef{Type: st.Type, ID: st.ID}, Relation: rw.Relation}] {
				return true
			}
		}

		return false

	case OpUnion:
		for _, c := range rw.Children {
			if entails(t, s, tuples, subj, held, c, obj, rel) {
				return true
			}
		}

		return false

	case OpIntersection:
		for _, c := range rw.Children {
			if !entails(t, s, tuples, subj, held, c, obj, rel) {
				return false
			}
		}

		return true

	case OpExclusion:
		t.Fatalf("the reference does not model %s", rw.Op)
	}

	t.Fatalf("the reference does not model %s", rw.Op)

	return false
}

// A tuple the relation does not accept grants nobody, which the reference has to say too or
// it would only agree with the evaluator on schemas the generator happened to write tightly.
func storedEntails(s *Schema, ref RelationRef, stored []SubjectRef,
	subj SubjectRef, held map[memoKey]bool,
) bool {
	for _, st := range stored {
		switch {
		case st.Wildcard:
			if subj.Relation == NoRelation && st.Type == subj.Type && s.Allows(ref, WildcardType(st.Type)) {
				return true
			}
		case st == subj:
			if s.Allows(ref, SubjectType{Type: st.Type, Relation: st.Relation}) {
				return true
			}
		case st.Relation != NoRelation:
			if !s.Allows(ref, UsersetType(st.Type, st.Relation)) {
				continue
			}

			if held[memoKey{Object: ObjectRef{Type: st.Type, ID: st.ID}, Relation: st.Relation}] {
				return true
			}
		}
	}

	return false
}
