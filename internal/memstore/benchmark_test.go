package memstore

import (
	"fmt"
	"runtime"
	"slices"
	"testing"

	"github.com/fervovita/AuthZ-dev/internal/watch"
)

// BenchmarkCheck reads what a Check costs on a replica, in the shapes core's BenchmarkCheck reads on its test map:
// the difference is what the store and resolving names add.
func BenchmarkCheck(b *testing.B) {
	for _, c := range checkCases() {
		b.Run(c.name, func(b *testing.B) {
			var s Store

			reset(b, &s, c.schema, c.tuples...)

			req := checkOf(b, c.check)
			ctx := b.Context()

			b.ReportAllocs()

			for b.Loop() {
				allowed, err := s.Check(ctx, req)
				if err != nil {
					b.Fatalf("Check: %v", err)
				}

				// Every case is written to grant, so a denial means the shape stopped exercising what it was built for.
				if !allowed {
					b.Fatal("Allowed = false")
				}
			}
		})
	}
}

// BenchmarkCheckParallel runs Checks at once, as a sidecar does: through the store, and on its engine with the names resolved beforehand.
func BenchmarkCheckParallel(b *testing.B) {
	// The cheapest shape, where the locks weigh most, and two that spend longer in the engine.
	picked := []string{"direct", "groups/depth=8", "fanout/width=100"}

	for _, c := range checkCases() {
		if !slices.Contains(picked, c.name) {
			continue
		}

		var s Store

		reset(b, &s, c.schema, c.tuples...)

		req := checkOf(b, c.check)

		resolved, err := s.current.request(req)
		if err != nil {
			b.Fatalf("request: %v", err)
		}

		b.Run(c.name+"/store", func(b *testing.B) {
			ctx := b.Context()

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if allowed, err := s.Check(ctx, req); err != nil || !allowed {
						b.Errorf("Check = %v, %v", allowed, err)

						return
					}
				}
			})
		})

		b.Run(c.name+"/engine", func(b *testing.B) {
			ctx := b.Context()

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if res, err := s.current.engine.Check(ctx, resolved); err != nil || !res.Allowed {
						b.Errorf("engine.Check = %v, %v", res.Allowed, err)

						return
					}
				}
			})
		})
	}
}

type checkCase struct {
	name   string
	schema string
	tuples []string
	check  string
}

const (
	directSchema = `
		definition user {}
		definition document {
			relation viewer: user
		}`

	groupsSchema = `
		definition user {}
		definition team {
			relation member: user | team#member
		}
		definition document {
			relation viewer: team#member
		}`

	arrowSchema = `
		definition user {}
		definition folder {
			relation parent: folder
			relation viewer: user
			permission view = viewer + parent->view
		}
		definition document {
			relation parent: folder
			relation viewer: user
			permission view = viewer + parent->view
		}`

	fanoutSchema = `
		definition user {}
		definition team {
			relation member: user
		}
		definition document {
			relation viewer: team#member
		}`

	exclusionSchema = `
		definition user {}
		definition document {
			relation viewer: user | user:*
			relation owner: user
			relation banned: user
			permission view = (viewer + owner) - banned
		}`

	diamondSchema = `
		definition user {}
		definition team {
			relation member: user | team#member
		}
		definition document {
			relation viewer: team#member
			relation editor: team#member
			permission view = viewer & editor
		}`
)

// checkCases are core's shapes in the language: a direct grant, a chain of groups, a folder hierarchy behind arrows,
// a relation naming many groups, a wildcard under an exclusion, and a cycle that has to settle.
func checkCases() []checkCase {
	cases := []checkCase{{"direct", directSchema, []string{"document:d1#viewer@user:alice"}, "document:d1#viewer@user:alice"}}

	for _, depth := range []int{2, 8, 16} {
		tuples := []string{"document:d1#viewer@team:t1#member"}
		for i := 1; i < depth; i++ {
			tuples = append(tuples, fmt.Sprintf("team:t%d#member@team:t%d#member", i, i+1))
		}

		tuples = append(tuples, fmt.Sprintf("team:t%d#member@user:alice", depth))
		cases = append(cases, checkCase{fmt.Sprintf("groups/depth=%d", depth), groupsSchema, tuples, "document:d1#viewer@user:alice"})
	}

	for _, depth := range []int{2, 8, 16} {
		tuples := []string{"document:d1#parent@folder:f1"}
		for i := 1; i < depth; i++ {
			tuples = append(tuples, fmt.Sprintf("folder:f%d#parent@folder:f%d", i, i+1))
		}

		tuples = append(tuples, fmt.Sprintf("folder:f%d#viewer@user:alice", depth))
		cases = append(cases, checkCase{fmt.Sprintf("arrow/depth=%d", depth), arrowSchema, tuples, "document:d1#view@user:alice"})
	}

	// The groups are interned in order, so the one holding alice is last: every other group is descended into first.
	for _, width := range []int{10, 100, 1000} {
		var tuples []string
		for i := 1; i <= width; i++ {
			tuples = append(tuples, fmt.Sprintf("document:d1#viewer@team:t%d#member", i))
		}

		tuples = append(tuples, fmt.Sprintf("team:t%d#member@user:alice", width))
		cases = append(cases, checkCase{fmt.Sprintf("fanout/width=%d", width), fanoutSchema, tuples, "document:d1#viewer@user:alice"})
	}

	return append(cases,
		checkCase{
			"wildcard-exclusion", exclusionSchema,
			[]string{"document:d1#viewer@user:*", "document:d1#banned@user:bob"},
			"document:d1#view@user:alice",
		},
		// eng and alice hold each other, and eng reaches bob through a third group.
		checkCase{
			"diamond", diamondSchema,
			[]string{
				"document:d1#viewer@team:eng#member",
				"document:d1#editor@team:alice#member",
				"team:eng#member@team:alice#member",
				"team:eng#member@team:d1#member",
				"team:alice#member@team:eng#member",
				"team:d1#member@user:bob",
			},
			"document:d1#view@user:bob",
		},
	)
}

// shapes are how a replica's tuples sit on its keys: a document each, as most documents are shared with a few,
// or all on one, as a large group's members are. IDs are 32 bytes, as the central store's benchmarks' are.
var shapes = []struct {
	name  string
	tuple func(i int) watch.Tuple
}{
	{"spread", func(i int) watch.Tuple {
		return watch.Tuple{
			ResourceType: "document", ResourceID: fmt.Sprintf("d%031d", i), Relation: "viewer",
			SubjectType: "user", SubjectID: fmt.Sprintf("u%031d", i%1000),
		}
	}},
	{"wide", func(i int) watch.Tuple {
		return watch.Tuple{
			ResourceType: "document", ResourceID: "d1", Relation: "viewer",
			SubjectType: "user", SubjectID: fmt.Sprintf("u%031d", i),
		}
	}},
}

func tuplesShaped(tuple func(int) watch.Tuple, first, n int) []watch.Tuple {
	tuples := make([]watch.Tuple, n)
	for i := range tuples {
		tuples[i] = tuple(first + i)
	}

	return tuples
}

// BenchmarkReset reads what a bootstrap costs a sidecar, by the tuples it receives: the time to build the replica,
// and B/tuple, the memory the built replica keeps, names included.
func BenchmarkReset(b *testing.B) {
	for _, shape := range shapes {
		for _, n := range []int{10_000, 100_000} {
			b.Run(fmt.Sprintf("%s/tuples=%d", shape.name, n), func(b *testing.B) {
				tuples := tuplesShaped(shape.tuple, 0, n)

				b.ReportAllocs()

				for b.Loop() {
					var s Store

					if err := s.Reset(watch.Revision("r1"), directSchema, tuples); err != nil {
						b.Fatalf("Reset: %v", err)
					}
				}

				// After the loop: its first round clears what was reported before it.
				b.ReportMetric(retained(b, func() *Store {
					var s Store

					if err := s.Reset(watch.Revision("r1"), directSchema, tuplesShaped(shape.tuple, 0, n)); err != nil {
						b.Fatalf("Reset: %v", err)
					}

					return &s
				})/float64(n), "B/tuple")
			})
		}
	}
}

// retained reports the bytes what build returns keeps live, measured between two collections.
// The tuples are made inside build, so the names the dictionary holds on to are counted.
func retained(tb testing.TB, build func() *Store) float64 {
	tb.Helper()

	var before, after runtime.MemStats

	runtime.GC() //nolint:revive // call-to-gc: the live heap is only known right after a collection
	runtime.ReadMemStats(&before)

	s := build()

	runtime.GC() //nolint:revive // call-to-gc: as above
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(s)

	return float64(after.HeapAlloc) - float64(before.HeapAlloc)
}

// BenchmarkApply reads how long a change keeps Checks waiting, on a replica of 100,000 tuples: each round adds
// changes tuples and the next takes them back. A wide replica rebuilds its one key's list for any change to it.
func BenchmarkApply(b *testing.B) {
	const stored = 100_000

	for _, shape := range shapes {
		for _, n := range []int{1, 100, 10_000} {
			b.Run(fmt.Sprintf("%s/changes=%d", shape.name, n), func(b *testing.B) {
				var s Store

				if err := s.Reset(watch.Revision("r0"), directSchema, tuplesShaped(shape.tuple, 0, stored)); err != nil {
					b.Fatalf("Reset: %v", err)
				}

				added, taken := make([]watch.Change, n), make([]watch.Change, n)
				for i, t := range tuplesShaped(shape.tuple, stored, n) {
					added[i], taken[i] = watch.Change{Tuple: t, Present: true}, watch.Change{Tuple: t}
				}

				b.ReportAllocs()

				round := 0

				for b.Loop() {
					changes := added
					if round%2 == 1 {
						changes = taken
					}

					round++

					if err := s.Apply(watch.Revision("r1"), nil, changes); err != nil {
						b.Fatalf("Apply: %v", err)
					}
				}
			})
		}
	}
}
