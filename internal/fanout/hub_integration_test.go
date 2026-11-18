//go:build integration

package fanout

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fervovita/AuthZ-dev/internal/datastore/postgres"
	"github.com/fervovita/AuthZ-dev/internal/memstore"
	"github.com/fervovita/AuthZ-dev/internal/watch"
)

// pairsSchema is what the writers keep to: every doc's left and right are written together, so torn holds for
// no one at any revision the store reaches. Each write also adds an entry to the ledger, which nothing takes away.
const pairsSchema = `
	definition user {}

	definition doc {
		relation left: user
		relation right: user
		permission torn = (left - right) + (right - left)
	}

	definition ledger {
		relation entry: user
	}
`

// teamSchema adds a definition no tuple uses, so writers can change the schema back and forth.
const teamSchema = pairsSchema + `
	definition team {
		relation member: user
	}
`

const (
	docs    = 4
	users   = 3
	writers = 3
	writes  = 200 // per writer
	tick    = 2 * time.Millisecond

	// keep is far longer than a tick, so the hub stays ahead of the cut, and far shorter than the slow replica
	// falls behind, so it comes back to a reset. The hub itself behind the cut is a unit test's.
	keep = 100 * tick
)

var errDropped = errors.New("connection dropped")

// follower is a sidecar's replica following the hub. It checks each event it takes against the watch contract,
// and before taking one, the revision the last brought it to: Follow takes the next only once the last has landed,
// so that is every revision the replica reaches.
type follower struct {
	t       *testing.T
	name    string
	hub     *Hub
	replica memstore.Store

	dropEvery int // it ends its subscription after this many events, as a lost connection would; 0 never
	pauseAt   int // after this many events it stops taking them until the hub cuts it off; 0 never

	events, resets, prunedResets, drops, cutOff, checks atomic.Int64
}

func (f *follower) Subscribe(ctx context.Context, from watch.Revision) (watch.Subscription, error) {
	sub, err := f.hub.Subscribe(ctx, from)
	if err != nil {
		return nil, err
	}

	return &checked{f: f, sub: sub, c: contract{from: from}}, nil
}

type checked struct {
	f   *follower
	sub watch.Subscription
	c   contract
}

func (s *checked) Recv() (watch.Event, error) {
	f := s.f
	f.assertWhole()

	switch n := f.events.Add(1); {
	case f.dropEvery > 0 && n%int64(f.dropEvery) == 0:
		f.drops.Add(1)

		return watch.Event{}, errDropped
	case n == int64(f.pauseAt):
		select {
		case <-s.sub.(*subscription).done:
		case <-time.After(time.Minute): // the hub's reads stall for seconds when Docker's VM is busy
			f.t.Errorf("%s: the hub never cut off a subscription that took nothing", f.name)
		}
	}

	ev, err := s.sub.Recv()
	if err != nil {
		if errors.Is(err, errSlow) {
			f.cutOff.Add(1)
		}

		return ev, err
	}

	if err := s.c.check(ev); err != nil {
		f.t.Errorf("%s: %v", f.name, err)
	}

	if ev.Reset != nil && len(ev.Reset.Revision) > 0 {
		f.resets.Add(1)

		if len(s.c.from) > 0 {
			f.prunedResets.Add(1)
		}
	}

	return ev, nil
}

// assertWhole fails the test if f's replica holds some doc's pair in half for some user.
func (f *follower) assertWhole() {
	for d := range docs {
		for u := range users {
			torn, err := f.replica.Check(context.Background(), request(d, "torn", u))
			if errors.Is(err, memstore.ErrNotReady) {
				return
			}

			if err != nil {
				f.t.Errorf("%s: Check: %v", f.name, err)

				return
			}

			f.checks.Add(1)

			if torn {
				f.t.Errorf("%s holds doc:d%d's pair in half for user:u%d, at %s", f.name, d, u, f.replica.Revision())
			}
		}
	}
}

// holds reports whether f's replica answers each request in want as want does.
func (f *follower) holds(want map[memstore.CheckRequest]bool) bool {
	for req, allowed := range want {
		if got, err := f.replica.Check(context.Background(), req); err != nil || got != allowed {
			return false
		}
	}

	return true
}

func request(d int, permission string, u int) memstore.CheckRequest {
	return memstore.CheckRequest{
		ResourceType: "doc", ResourceID: "d" + strconv.Itoa(d), Permission: permission,
		SubjectType: "user", SubjectID: "u" + strconv.Itoa(u),
	}
}

// entry is the ledger's nth line.
func entry(n int64) postgres.Tuple {
	return postgres.Tuple{
		ResourceType: "ledger", ResourceID: "l", Relation: "entry",
		SubjectType: "user", SubjectID: "e" + strconv.FormatInt(n, 10),
	}
}

func pair(op postgres.Op, d, u int) []postgres.Update {
	updates := make([]postgres.Update, 0, 2)

	for _, rel := range []string{"left", "right"} {
		updates = append(updates, postgres.Update{Op: op, Tuple: postgres.Tuple{
			ResourceType: "doc", ResourceID: "d" + strconv.Itoa(d), Relation: rel,
			SubjectType: "user", SubjectID: "u" + strconv.Itoa(u),
		}})
	}

	return updates
}

// writeOne writes a pair whole or takes it away whole, with the ledger's next entry, or now and then changes the schema.
func writeOne(ctx context.Context, st *postgres.Store, rnd *rand.Rand, entries *atomic.Int64) error {
	if rnd.IntN(20) == 0 {
		return st.WriteSchema(ctx, []string{pairsSchema, teamSchema}[rnd.IntN(2)])
	}

	op := postgres.Touch
	if rnd.IntN(2) == 0 {
		op = postgres.Delete
	}

	line := postgres.Update{Op: postgres.Touch, Tuple: entry(entries.Add(1) - 1)}

	return st.Write(ctx, append(pair(op, rnd.IntN(docs), rnd.IntN(users)), line))
}

// While writers write pairs whole, the log is pruned and the schema changes, replicas that join at the start and
// halfway, drop their subscription, and fall so far behind the hub cuts them off, each reach only revisions where no
// pair is in half. Once the writers stop, every replica holds what the store does, every ledger entry included:
// a write a replica missed stays missed.
func TestFollowersOnlyEverHoldWholePairs(t *testing.T) {
	t.Parallel()

	st := centralStore(t, database(t), pairsSchema)

	// The ledger starts longer than a part, so every reset comes in parts.
	var entries atomic.Int64

	seed := make([]postgres.Update, partSize+1)
	for i := range seed {
		seed[i] = postgres.Update{Op: postgres.Touch, Tuple: entry(entries.Add(1) - 1)}
	}

	if err := st.Write(t.Context(), seed); err != nil {
		t.Fatalf("Write: %v", err)
	}

	h := New(st, tick)
	runHub(t, h)

	var (
		followers = []*follower{
			{name: "first"},
			{name: "dropping", dropEvery: 40},
			{name: "slow", pauseAt: 50},
			{name: "late"},
		}
		progress atomic.Int64
		running  sync.WaitGroup
		writing  sync.WaitGroup
	)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	for i, f := range followers {
		f.t, f.hub = t, h

		running.Go(func() {
			// The last waits for the writers to be a third of the way.
			for i == len(followers)-1 && progress.Load() < writers*writes/3 && ctx.Err() == nil {
				time.Sleep(time.Millisecond)
			}

			if err := f.replica.Follow(ctx, f); !errors.Is(err, context.Canceled) {
				t.Errorf("%s: Follow = %v; want context.Canceled", f.name, err)
			}
		})
	}

	for w := range writers {
		writing.Go(func() {
			//nolint:gosec // G404: a failing run's requests have to be reproducible from its seed
			rnd := rand.New(rand.NewPCG(uint64(w), 0))

			for range writes {
				if err := writeOne(ctx, st, rnd, &entries); err != nil {
					t.Errorf("writer %d: %v", w, err)

					return
				}

				progress.Add(1)
			}
		})
	}

	pruning := make(chan struct{})

	running.Go(func() {
		for {
			select {
			case <-pruning:
				return
			case <-time.After(20 * time.Millisecond):
			}

			// Not ctx: the followers' end would cut a Prune short.
			if err := st.Prune(t.Context(), keep); err != nil {
				t.Errorf("Prune: %v", err)

				return
			}
		}
	})

	writing.Wait()

	final, err := st.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Every pair and every entry, allowed as the store holds it.
	want := make(map[memstore.CheckRequest]bool)

	for d := range docs {
		for u := range users {
			want[request(d, "left", u)], want[request(d, "right", u)] = false, false
		}
	}

	asked := func(tp postgres.Tuple) memstore.CheckRequest {
		return memstore.CheckRequest{
			ResourceType: tp.ResourceType, ResourceID: tp.ResourceID, Permission: tp.Relation,
			SubjectType: tp.SubjectType, SubjectID: tp.SubjectID,
		}
	}

	for n := range entries.Load() {
		want[asked(entry(n))] = false
	}

	for _, tp := range final.Tuples {
		want[asked(tp)] = true
	}

	deadline := time.Now().Add(20 * time.Second)

	for _, f := range followers {
		for !f.holds(want) {
			if time.Now().After(deadline) {
				t.Errorf("%s never came to hold what the store does", f.name)

				break
			}

			time.Sleep(5 * time.Millisecond)
		}
	}

	close(pruning)
	cancel()
	running.Wait()

	var resets, prunedResets, drops, cutOff, checks int64

	for _, f := range followers {
		t.Logf("%s: %d events, %d resets (%d coming back), %d drops, cut off %d times, %d checks",
			f.name, f.events.Load(), f.resets.Load(), f.prunedResets.Load(), f.drops.Load(), f.cutOff.Load(), f.checks.Load())

		resets += f.resets.Load()
		prunedResets += f.prunedResets.Load()
		drops += f.drops.Load()
		cutOff += f.cutOff.Load()
		checks += f.checks.Load()
	}

	if resets < int64(len(followers)) || prunedResets == 0 || drops == 0 || cutOff == 0 || checks == 0 {
		t.Error("some way of following never came up, so the run tested little")
	}
}
