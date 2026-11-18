//go:build integration

package fanout

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fervovita/AuthZ-dev/internal/datastore/postgres"
	"github.com/fervovita/AuthZ-dev/internal/memstore"
	"github.com/fervovita/AuthZ-dev/internal/watch"
)

// BenchmarkCommitToApply reads how long a write takes to reach a sidecar, by the hub's tick: from the call that writes
// a tuple to the first Check its replica allows with it. Writes land at ten phases across the tick, as a service's would.
func BenchmarkCommitToApply(b *testing.B) {
	for _, tick := range []time.Duration{10 * time.Millisecond, 100 * time.Millisecond} {
		b.Run(fmt.Sprintf("tick=%v", tick), func(b *testing.B) {
			st := centralStore(b, database(b), pairsSchema)
			h := New(st, tick)
			runHub(b, h)

			var replica memstore.Store

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)

			go func() { done <- replica.Follow(ctx, h) }()

			b.Cleanup(func() {
				cancel()

				if err := <-done; !errors.Is(err, context.Canceled) {
					b.Errorf("Follow = %v; want context.Canceled", err)
				}
			})

			for replica.Revision() == nil {
				time.Sleep(time.Millisecond)
			}

			for i := 0; b.Loop(); i++ {
				b.StopTimer()
				time.Sleep(time.Duration(i%10) * tick / 10)

				user := fmt.Sprintf("u%d", i)
				tuple := postgres.Tuple{ResourceType: "doc", ResourceID: "d0", Relation: "left", SubjectType: "user", SubjectID: user}
				req := memstore.CheckRequest{ResourceType: "doc", ResourceID: "d0", Permission: "left", SubjectType: "user", SubjectID: user}

				b.StartTimer()

				if err := st.Write(ctx, []postgres.Update{{Op: postgres.Touch, Tuple: tuple}}); err != nil {
					b.Fatalf("Write: %v", err)
				}

				for {
					allowed, err := replica.Check(ctx, req)
					if err != nil {
						b.Fatalf("Check: %v", err)
					}

					if allowed {
						break
					}

					time.Sleep(50 * time.Microsecond)
				}
			}
		})
	}
}

// BenchmarkJoin reads a tick in which n sidecars join a store of 100,000 tuples: the hub reads the store once for all
// of them, and each is handed it in parts. The sidecars only take the parts, so it is the central side alone.
func BenchmarkJoin(b *testing.B) {
	const stored = 100_000

	dsn := database(b)
	st := centralStore(b, dsn, pairsSchema)

	// IDs are 32 bytes, as the store's benchmarks' are: a doc each, shared among 1,000 users.
	for first := 0; first < stored; first += 10_000 {
		updates := make([]postgres.Update, 0, 10_000)
		for i := first; i < first+10_000; i++ {
			updates = append(updates, postgres.Update{Op: postgres.Touch, Tuple: postgres.Tuple{
				ResourceType: "doc", ResourceID: fmt.Sprintf("d%031d", i), Relation: "left",
				SubjectType: "user", SubjectID: fmt.Sprintf("u%031d", i%1000),
			}})
		}

		if err := st.Write(b.Context(), updates); err != nil {
			b.Fatalf("Write: %v", err)
		}
	}

	// As autovacuum and a checkpoint would leave it: without statistics the tick's Changes reads the whole store.
	conn, err := pgx.Connect(b.Context(), dsn)
	if err != nil {
		b.Fatalf("connect: %v", err)
	}

	defer func() { _ = conn.Close(context.Background()) }()

	for _, sql := range []string{"VACUUM (ANALYZE)", "CHECKPOINT"} {
		if _, err := conn.Exec(b.Context(), sql); err != nil {
			b.Fatalf("%s: %v", sql, err)
		}
	}

	// The read the hub shares among all who join in a tick, on its own.
	b.Run("snapshot", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			state, err := st.Snapshot(b.Context())
			if err != nil || len(state.Tuples) != stored {
				b.Fatalf("Snapshot = %d tuples, %v; want %d", len(state.Tuples), err, stored)
			}
		}
	})

	for _, n := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("joiners=%d", n), func(b *testing.B) {
			ctx := b.Context()

			b.ReportAllocs()

			for b.Loop() {
				h := New(st, time.Hour)
				subs := make([]watch.Subscription, n)

				for i := range subs {
					sub, err := h.Subscribe(ctx, nil)
					if err != nil {
						b.Fatalf("Subscribe: %v", err)
					}

					subs[i] = sub
				}

				h.step(ctx)

				for _, sub := range subs {
					if got := drain(b, sub); got != stored {
						b.Fatalf("handed %d tuples; want %d", got, stored)
					}
				}
			}
		})
	}
}

// drain takes a reset's parts from sub and returns how many tuples they held.
func drain(b *testing.B, sub watch.Subscription) int {
	held := 0

	for {
		ev, err := sub.Recv()
		if err != nil || ev.Reset == nil {
			b.Fatalf("Recv = %+v, %v; want a reset", ev, err)
		}

		if held += len(ev.Reset.Tuples); !ev.Reset.More {
			return held
		}
	}
}
