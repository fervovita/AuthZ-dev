//go:build integration

package postgres

import (
	"fmt"
	"strings"
	"testing"
)

// storeSizes are the tuple counts the benchmarks seed: three points show what one more stored tuple costs.
var storeSizes = []int{1_000, 10_000, 100_000}

// BenchmarkWrite reads what one more update costs a request, and one more definition in the schema it compiles.
func BenchmarkWrite(b *testing.B) {
	// maxTupleLocks and one more sit either side of the switch from tuple locks to the relation's.
	for _, n := range []int{1, maxTupleLocks, maxTupleLocks + 1, 1000} {
		b.Run(fmt.Sprintf("updates=%d", n), func(b *testing.B) {
			benchmarkWrite(b, schemaStore(b), n)
		})
	}

	// The baseline for these is the one-tuple request, which runs under testSchema before any padding.
	for _, n := range []int{10, 100} {
		b.Run(fmt.Sprintf("definitions=%d", n), func(b *testing.B) {
			s := schemaStore(b)

			if err := s.WriteSchema(b.Context(), wideSchema(n)); err != nil {
				b.Fatalf("WriteSchema: %v", err)
			}

			benchmarkWrite(b, s, 1)
		})
	}
}

// benchmarkWrite times requests that each store n tuples s does not hold.
func benchmarkWrite(b *testing.B, s *Store, n int) {
	ctx := b.Context()
	updates := make([]Update, n)
	next := 0

	b.ReportAllocs()

	for b.Loop() {
		b.StopTimer()

		for i := range updates {
			updates[i] = Update{Touch, benchTuple(next)}
			next++
		}

		b.StartTimer()

		if err := s.Write(ctx, updates); err != nil {
			b.Fatalf("Write: %v", err)
		}
	}
}

// BenchmarkDeleteMatching reads how long a filter keeps writes out of its relation, by the tuples it deletes.
func BenchmarkDeleteMatching(b *testing.B) {
	for _, n := range storeSizes {
		b.Run(fmt.Sprintf("tuples=%d", n), func(b *testing.B) {
			s := schemaStore(b)
			ctx := b.Context()
			f := filter("document#viewer")

			b.ReportAllocs()

			for b.Loop() {
				b.StopTimer()
				exec(b, s, `TRUNCATE tuples, change_log`)
				seed(b, s, 0, n)
				b.StartTimer()

				if err := s.DeleteMatching(ctx, f); err != nil {
					b.Fatalf("DeleteMatching: %v", err)
				}
			}

			// A filter that no longer matched the seed would time a delete of nothing.
			if st, err := s.Snapshot(ctx); err != nil || len(st.Tuples) != 0 {
				b.Fatalf("Snapshot after DeleteMatching: %d tuples, %v; want none", len(st.Tuples), err)
			}
		})
	}
}

// BenchmarkWriteSchema reads how long dropping a subject type keeps every write out, by the tuples in its relation.
// No seeded tuple is of the dropped type, so the check reads them all before it lets the schema in.
func BenchmarkWriteSchema(b *testing.B) {
	narrowed := edit(b, [2]string{"viewer: user | user:* | team | team#member", "viewer: user | team | team#member"})

	for _, n := range storeSizes {
		b.Run(fmt.Sprintf("tuples=%d", n), func(b *testing.B) {
			s := schemaStore(b)
			ctx := b.Context()

			seed(b, s, 0, n)
			b.ReportAllocs()

			for b.Loop() {
				if err := s.WriteSchema(ctx, narrowed); err != nil {
					b.Fatalf("WriteSchema: %v", err)
				}

				b.StopTimer()

				if err := s.WriteSchema(ctx, testSchema); err != nil {
					b.Fatalf("WriteSchema: %v", err)
				}

				b.StartTimer()
			}
		})
	}
}

// BenchmarkSnapshot reads what a bootstrap costs the store in time and memory, by the tuples stored.
func BenchmarkSnapshot(b *testing.B) {
	for _, n := range storeSizes {
		b.Run(fmt.Sprintf("tuples=%d", n), func(b *testing.B) {
			s := schemaStore(b)
			ctx := b.Context()

			seed(b, s, 0, n)
			b.ReportAllocs()

			for b.Loop() {
				st, err := s.Snapshot(ctx)
				if err != nil {
					b.Fatalf("Snapshot: %v", err)
				}

				if len(st.Tuples) != n {
					b.Fatalf("Snapshot read %d tuples; want %d", len(st.Tuples), n)
				}
			}
		})
	}
}

// BenchmarkChanges reads what catching up costs, by the tuples changed since the reader's revision.
// The store beneath is empty or already holds and has logged many, which shows what its history adds.
func BenchmarkChanges(b *testing.B) {
	for _, stored := range []int{0, 100_000} {
		for _, changed := range []int{1, 1000, 100_000} {
			b.Run(fmt.Sprintf("tuples=%d/changed=%d", stored, changed), func(b *testing.B) {
				s := schemaStore(b)
				ctx := b.Context()

				seed(b, s, 0, stored)

				st, err := s.Snapshot(ctx)
				if err != nil {
					b.Fatalf("Snapshot: %v", err)
				}

				since := st.Revision

				seed(b, s, stored, changed)
				b.ReportAllocs()

				for b.Loop() {
					d, err := s.Changes(ctx, since)
					if err != nil {
						b.Fatalf("Changes: %v", err)
					}

					if len(d.Changes) != changed {
						b.Fatalf("Changes read %d tuples; want %d", len(d.Changes), changed)
					}
				}
			})
		}
	}
}

// seedSQL stores and logs $2 tuples, starting at tuple $1.
const seedSQL = `WITH changed AS (
		INSERT INTO tuples (` + columns + `)
		SELECT 'document', lpad(i::text, 32, '0'), 'viewer', 'user', lpad(i::text, 32, '0'), ''
		FROM generate_series($1::int, $1::int + $2::int - 1) AS i
		RETURNING ` + columns + `)
	INSERT INTO change_log (` + columns + `) SELECT ` + columns + ` FROM changed`

// seed stores the n tuples from first, then vacuums and analyzes as autovacuum would have.
func seed(tb testing.TB, s *Store, first, n int) {
	tb.Helper()

	exec(tb, s, seedSQL, first, n)
	exec(tb, s, `VACUUM (ANALYZE) tuples, change_log`)
}

func exec(tb testing.TB, s *Store, sql string, args ...any) {
	tb.Helper()

	if _, err := s.pool.Exec(tb.Context(), sql, args...); err != nil {
		tb.Fatalf("%s: %v", sql, err)
	}
}

// benchTuple returns tuple i of document#viewer.
func benchTuple(i int) Tuple {
	id := fmt.Sprintf("%032d", i)

	return Tuple{"document", id, "viewer", "user", id, ""}
}

// folderDefinition is a definition a document service could hold.
const folderDefinition = `
	definition folder%d {
		relation parent: document
		relation owner: user | team#member
		relation editor: user | team#member
		relation viewer: user | user:* | team#member
		relation banned: user
		permission edit = owner + editor
		permission view = (viewer + edit + parent->view) - banned
	}
`

// wideSchema returns testSchema padded to n definitions with copies of folderDefinition.
func wideSchema(n int) string {
	var sb strings.Builder

	sb.WriteString(testSchema)

	for i := strings.Count(testSchema, "definition "); i < n; i++ {
		fmt.Fprintf(&sb, folderDefinition, i)
	}

	return sb.String()
}
