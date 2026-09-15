package core

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// Memoizing inside a cycle is the one part of the evaluator whose correctness is not visible by reading it,
// so it is checked against the definition instead.
// Which schemas Build accepts is part of that definition, so its verdict on each one is checked as well.
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

	for seed := range 20000 {
		//nolint:gosec // G404: a failing case has to be reproducible from its seed
		rnd := rand.New(rand.NewPCG(uint64(seed), 0x5eed))

		defs := randomDefinitions(rnd, rels)
		level, stratified := strata(dependencies(defs))

		s, err := defs.build()
		if (err == nil) != stratified {
			t.Fatalf("seed %d: Build: %v; the definitions stratify: %v", seed, err, stratified)
		}

		if err != nil {
			continue
		}

		tuples := randomTuples(rnd, objs, rels, subj)
		want := fixedPoint(t, defs, tuples, subj, pairs, level)

		// A frame is entered only for a key not seen yet, so nesting cannot exceed the key count.
		e := engine(t, s, &stubSource{subjects: tuples}, WithMaxDepth(len(pairs)+1))

		for _, p := range pairs {
			if got := check(t, e, p.Object, p.Relation, subj); got.Allowed != want[p] {
				t.Fatalf("seed %d: %+v = %v; the fixed point says %v", seed, p, got.Allowed, want[p])
			}
		}
	}
}

// definitions is a generated schema as written, so the reference never reads it through Build.
type definitions struct {
	rewrites map[RelationRef]Rewrite
	allowed  map[RelationRef][]SubjectType
}

func (d definitions) build() (*Schema, error) {
	b := NewSchemaBuilder()

	for ref, rw := range d.rewrites {
		if rw.Op == OpThis {
			b.Relation(ref, d.allowed[ref]...)

			continue
		}

		b.Permission(ref, rw)
	}

	return b.Build()
}

// The leading relations store tuples and the rest are permissions over them.
func randomDefinitions(rnd *rand.Rand, rels []RelationID) definitions {
	d := definitions{
		rewrites: make(map[RelationRef]Rewrite, len(rels)),
		allowed:  make(map[RelationRef][]SubjectType, len(rels)),
	}

	// At least one relation, or nothing stores anything and every answer is false.
	stored := 1 + rnd.IntN(len(rels)-1)

	var tuplesets []RelationID

	for i, rel := range rels {
		ref := teamRel(rel)

		if i >= stored {
			d.rewrites[ref] = randomRewrite(rnd, rels, tuplesets, 2)

			continue
		}

		d.rewrites[ref] = This()

		// An arrow needs a tupleset naming objects only, which a random subset of every shape rarely is.
		if rnd.IntN(3) == 0 {
			d.allowed[ref] = randomObjectTypes(rnd)
			tuplesets = append(tuplesets, rel)

			continue
		}

		d.allowed[ref] = randomTypes(rnd, rels)
	}

	return d
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

// Exclusion is drawn as often as the other combinators, loops through it included:
// Build has to refuse those, and the test checks that it refuses exactly those.
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

	switch rnd.IntN(3) {
	case 0:
		return Union(children...)
	case 1:
		return Intersection(children...)
	default:
		return Exclusion(children[0], children[1])
	}
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

// dependency says a relation's answer reads another's; negated marks a read through a subtract side.
type dependency struct {
	from, to RelationRef
	negated  bool
}

// dependencies lists every read the definitions allow, written out here rather than taken from Build,
// so a read Build forgets is one the reference still sees.
func dependencies(d definitions) []dependency {
	var out []dependency

	var walk func(from RelationRef, rw Rewrite, negated bool)

	walk = func(from RelationRef, rw Rewrite, negated bool) {
		switch rw.Op {
		case OpThis:
			// A stored userset is read for its members; the other subject shapes read nothing.
			for _, st := range d.allowed[from] {
				if st.Relation != NoRelation {
					out = append(out, dependency{from, RelationRef{Type: st.Type, Relation: st.Relation}, negated})
				}
			}

		case OpComputedUserset:
			out = append(out, dependency{from, RelationRef{Type: from.Type, Relation: rw.Relation}, negated})

		case OpTupleToUserset:
			tupleset := RelationRef{Type: from.Type, Relation: rw.Tupleset}
			out = append(out, dependency{from, tupleset, negated})

			for _, st := range d.allowed[tupleset] {
				out = append(out, dependency{from, RelationRef{Type: st.Type, Relation: rw.Relation}, negated})
			}

		case OpUnion, OpIntersection:
			for _, c := range rw.Children {
				walk(from, c, negated)
			}

		case OpExclusion:
			walk(from, rw.Children[0], negated)
			walk(from, rw.Children[1], true)
		}
	}

	for ref, rw := range d.rewrites {
		walk(ref, rw, false)
	}

	return out
}

// strata puts each relation no lower than anything it reads, and above anything it reads through a subtract.
// It reports false when no such placement exists, which is a loop through a subtract.
func strata(deps []dependency) (map[RelationRef]int, bool) {
	level := make(map[RelationRef]int)

	nodes := make(map[RelationRef]bool)
	for _, dep := range deps {
		nodes[dep.from] = true
		nodes[dep.to] = true
	}

	// Each round settles one more hop of every chain, and without such a loop a chain has fewer hops than there are relations.
	for range len(nodes) {
		changed := false

		for _, dep := range deps {
			need := level[dep.to]
			if dep.negated {
				need++
			}

			if level[dep.from] < need {
				level[dep.from] = need
				changed = true
			}
		}

		if !changed {
			return level, true
		}
	}

	return nil, false
}

// fixedPoint holds every key the schema entails for one subject. Each stratum is iterated from nothing held
// until a round changes nothing, over the strata below it, which are final by then and so safe to subtract.
func fixedPoint(t *testing.T, d definitions, tuples map[stubKey][]SubjectRef, subj SubjectRef,
	pairs []memoKey, level map[RelationRef]int,
) map[memoKey]bool {
	t.Helper()

	held := make(map[memoKey]bool, len(pairs))

	top := 0
	for _, l := range level {
		top = max(top, l)
	}

	for stratum := 0; stratum <= top; stratum++ {
		for changed := true; changed; {
			changed = false

			for _, p := range pairs {
				ref := RelationRef{Type: p.Object.Type, Relation: p.Relation}
				if level[ref] != stratum {
					continue
				}

				if !held[p] && entails(t, d, tuples, subj, held, d.rewrites[ref], p.Object, p.Relation) {
					held[p] = true
					changed = true
				}
			}
		}
	}

	return held
}

func entails(t *testing.T, d definitions, tuples map[stubKey][]SubjectRef, subj SubjectRef,
	held map[memoKey]bool, rw Rewrite, obj ObjectRef, rel RelationID,
) bool {
	t.Helper()

	switch rw.Op {
	case OpThis:
		ref := RelationRef{Type: obj.Type, Relation: rel}

		return storedEntails(d.allowed[ref], tuples[stubKey{obj, rel}], subj, held)

	case OpComputedUserset:
		return held[memoKey{Object: obj, Relation: rw.Relation}]

	case OpTupleToUserset:
		accepted := d.allowed[RelationRef{Type: obj.Type, Relation: rw.Tupleset}]

		for _, st := range tuples[stubKey{obj, rw.Tupleset}] {
			if st.Wildcard || st.Relation != NoRelation || !slices.Contains(accepted, DirectType(st.Type)) {
				continue
			}

			if held[memoKey{Object: ObjectRef{Type: st.Type, ID: st.ID}, Relation: rw.Relation}] {
				return true
			}
		}

		return false

	case OpUnion:
		for _, c := range rw.Children {
			if entails(t, d, tuples, subj, held, c, obj, rel) {
				return true
			}
		}

		return false

	case OpIntersection:
		for _, c := range rw.Children {
			if !entails(t, d, tuples, subj, held, c, obj, rel) {
				return false
			}
		}

		return true

	case OpExclusion:
		return entails(t, d, tuples, subj, held, rw.Children[0], obj, rel) &&
			!entails(t, d, tuples, subj, held, rw.Children[1], obj, rel)
	}

	t.Fatalf("the reference does not model %s", rw.Op)

	return false
}

// A tuple the relation does not accept grants nobody, which the reference has to say too or
// it would only agree with the evaluator on schemas the generator happened to write tightly.
func storedEntails(accepted []SubjectType, stored []SubjectRef, subj SubjectRef, held map[memoKey]bool) bool {
	for _, st := range stored {
		switch {
		case st.Wildcard:
			if subj.Relation == NoRelation && st.Type == subj.Type && slices.Contains(accepted, WildcardType(st.Type)) {
				return true
			}
		case st == subj:
			if slices.Contains(accepted, SubjectType{Type: st.Type, Relation: st.Relation}) {
				return true
			}
		case st.Relation != NoRelation:
			if !slices.Contains(accepted, UsersetType(st.Type, st.Relation)) {
				continue
			}

			if held[memoKey{Object: ObjectRef{Type: st.Type, ID: st.ID}, Relation: st.Relation}] {
				return true
			}
		}
	}

	return false
}
