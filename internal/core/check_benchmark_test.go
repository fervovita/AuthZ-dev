package core

import (
	"fmt"
	"testing"
)

// benchCase is one schema shape with a request that exercises it.
type benchCase struct {
	name    string
	engine  *Engine
	object  ObjectRef
	rel     RelationID
	subject SubjectRef
}

// BenchmarkCheck reads the evaluator alone: the source is the test map, so what it reports is
// what a sidecar pays per Check on top of whatever a store charges.
func BenchmarkCheck(b *testing.B) {
	modes := []struct {
		name string
		mode ProofMode
	}{
		{"proof=none", ProofNone},
		{"proof=justify", ProofJustify},
	}

	for _, c := range benchCases(b) {
		for _, m := range modes {
			b.Run(c.name+"/"+m.name, func(b *testing.B) {
				req := CheckRequest{Object: c.object, Relation: c.rel, Subject: c.subject, Proof: m.mode}
				ctx := b.Context()

				b.ReportAllocs()

				for b.Loop() {
					res, err := c.engine.Check(ctx, req)
					if err != nil {
						b.Fatalf("Check: %v", err)
					}

					// Every case is written to grant, so a denial means the shape stopped exercising what it was built for.
					if !res.Allowed {
						b.Fatal("Allowed = false")
					}
				}
			})
		}
	}
}

// benchCases covers one shape per way the evaluator spends time: a direct grant, a chain of groups,
// a folder hierarchy behind arrows, a relation naming many groups, a wildcard under an exclusion,
// and a cycle that has to settle.
func benchCases(tb testing.TB) []benchCase {
	tb.Helper()

	// Walk lengths rather than a guess at real hierarchies: they show what one more hop costs,
	// and they stay under DefaultMaxDepth so the shipped bound is the one that runs.
	depths := []ObjectID{2, 8, 16}
	cases := []benchCase{directCase(tb)}

	for _, depth := range depths {
		cases = append(cases, groupsCase(tb, depth))
	}

	for _, depth := range depths {
		cases = append(cases, arrowCase(tb, depth))
	}

	for _, width := range []ObjectID{10, 100, 1000} {
		cases = append(cases, fanoutCase(tb, width))
	}

	return append(cases, wildcardExclusionCase(tb), diamondCase(tb))
}

func benchSchema(tb testing.TB, sb *SchemaBuilder) *Schema {
	tb.Helper()

	s, err := sb.Build()
	if err != nil {
		tb.Fatalf("Build: %v", err)
	}

	return s
}

// directCase is the floor: the subject is named on the relation itself.
func directCase(tb testing.TB) benchCase {
	tb.Helper()

	s := benchSchema(tb, NewSchemaBuilder().Relation(docRel(rViewer), DirectType(tUser)))
	src := store().add(doc(oD1), rViewer, user(oAlice)).source()

	return benchCase{
		name:   "direct",
		engine: engine(tb, s, src),
		object: doc(oD1), rel: rViewer, subject: user(oAlice),
	}
}

// groupsCase chains groups that each hold the next, with the subject in the last one,
// so the answer costs one relation per hop.
func groupsCase(tb testing.TB, depth ObjectID) benchCase {
	tb.Helper()

	s := benchSchema(tb, NewSchemaBuilder().
		Relation(docRel(rViewer), UsersetType(tTeam, rMember)).
		Relation(teamRel(rMember), DirectType(tUser), UsersetType(tTeam, rMember)))

	src := store().add(doc(oD1), rViewer, teamMember(1))
	for i := ObjectID(1); i < depth; i++ {
		src.add(team(i), rMember, teamMember(i+1))
	}

	src.add(team(depth), rMember, user(oAlice))

	return benchCase{
		name:   fmt.Sprintf("groups/depth=%d", depth),
		engine: engine(tb, s, src.source()),
		object: doc(oD1), rel: rViewer, subject: user(oAlice),
	}
}

// arrowCase walks a folder hierarchy: every level reads its tupleset and follows it up,
// so a hop costs that read on top of the relation.
func arrowCase(tb testing.TB, depth ObjectID) benchCase {
	tb.Helper()

	src := store().add(doc(oD1), rParent, object(folder(1)))
	for i := ObjectID(1); i < depth; i++ {
		src.add(folder(i), rParent, object(folder(i+1)))
	}

	src.add(folder(depth), rViewer, user(oAlice))

	return benchCase{
		name:   fmt.Sprintf("arrow/depth=%d", depth),
		engine: engine(tb, arrowSchema(tb), src.source()),
		object: doc(oD1), rel: rView, subject: user(oAlice),
	}
}

// fanoutCase shares the document with width groups and puts the subject in the last one.
// The test map answers in insertion order, so every group is descended into before the grant.
func fanoutCase(tb testing.TB, width ObjectID) benchCase {
	tb.Helper()

	s := benchSchema(tb, NewSchemaBuilder().
		Relation(docRel(rViewer), UsersetType(tTeam, rMember)).
		Relation(teamRel(rMember), DirectType(tUser)))

	src := store()
	for i := ObjectID(1); i <= width; i++ {
		src.add(doc(oD1), rViewer, teamMember(i))
	}

	src.add(team(width), rMember, user(oAlice))

	return benchCase{
		name:   fmt.Sprintf("fanout/width=%d", width),
		engine: engine(tb, s, src.source()),
		object: doc(oD1), rel: rViewer, subject: user(oAlice),
	}
}

// wildcardExclusionCase answers from the wildcard bucket, the cheapest grant there is,
// and then still pays for the subtract before the answer holds.
func wildcardExclusionCase(tb testing.TB) benchCase {
	tb.Helper()

	src := store().
		add(doc(oD1), rViewer, anyUser()).
		add(doc(oD1), rBanned, user(oBob)).
		source()

	return benchCase{
		name:   "wildcard-exclusion",
		engine: engine(tb, validSchema(tb), src),
		object: doc(oD1), rel: rView, subject: user(oAlice),
	}
}

// diamondCase is an intersection whose operands both reach a pair of groups that hold each other:
// the first settles the component, the second reads it.
// union would not do this — it stops at the first operand that grants.
func diamondCase(tb testing.TB) benchCase {
	tb.Helper()

	s := benchSchema(tb, NewSchemaBuilder().
		Relation(docRel(rViewer), UsersetType(tTeam, rMember)).
		Relation(docRel(rEditor), UsersetType(tTeam, rMember)).
		Permission(docRel(rView), Intersection(ComputedUserset(rViewer), ComputedUserset(rEditor))).
		Relation(teamRel(rMember), DirectType(tUser), UsersetType(tTeam, rMember)))

	// eng and alice hold each other, and eng reaches bob through a third group.
	// Reaching eng through alice cuts the cycle, so the pair only answers once it is settled.
	src := store().
		add(doc(oD1), rViewer, teamMember(oEng)).
		add(doc(oD1), rEditor, teamMember(oAlice)).
		add(team(oEng), rMember, teamMember(oAlice)).
		add(team(oEng), rMember, teamMember(oD1)).
		add(team(oAlice), rMember, teamMember(oEng)).
		add(team(oD1), rMember, user(oBob)).
		source()

	return benchCase{
		name:   "diamond",
		engine: engine(tb, s, src),
		object: doc(oD1), rel: rView, subject: user(oBob),
	}
}
