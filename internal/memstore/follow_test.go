package memstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fervovita/AuthZ-dev/internal/watch"
)

var errDown = errors.New("source down")

// testSource is a watch.Source the test drives: each Subscribe and each Recv waits for the test.
type testSource struct {
	subs chan *testSubscription
}

func newTestSource() *testSource {
	return &testSource{subs: make(chan *testSubscription)}
}

func (src *testSource) Subscribe(ctx context.Context, from watch.Revision) (watch.Subscription, error) {
	sub := &testSubscription{ctx: ctx, from: from, asks: make(chan chan reply)}

	select {
	case src.subs <- sub:
		return sub, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// next waits for Follow to subscribe.
func (src *testSource) next(t *testing.T) *testSubscription {
	t.Helper()

	select {
	case sub := <-src.subs:
		return sub
	case <-time.After(10 * time.Second):
		t.Fatal("Follow did not subscribe")

		return nil
	}
}

type reply struct {
	ev  watch.Event
	err error
}

// testSubscription answers each Recv with what the test gives it.
type testSubscription struct {
	ctx  context.Context
	from watch.Revision
	asks chan chan reply
	held chan reply // a Recv the test has taken and not yet answered
}

func (sub *testSubscription) Recv() (watch.Event, error) {
	r := make(chan reply, 1)

	select {
	case sub.asks <- r:
	case <-sub.ctx.Done():
		return watch.Event{}, sub.ctx.Err()
	}

	select {
	case got := <-r:
		return got.ev, got.err
	case <-sub.ctx.Done():
		return watch.Event{}, sub.ctx.Err()
	}
}

// land hands Follow ev and returns once Follow asks for the next event, by which time ev has landed.
func (sub *testSubscription) land(t *testing.T, ev watch.Event) {
	t.Helper()

	sub.send(t, ev)
	sub.held = sub.ask(t)
}

// send hands Follow ev without waiting for it to land.
func (sub *testSubscription) send(t *testing.T, ev watch.Event) {
	t.Helper()

	sub.answer(t, reply{ev: ev})
}

// end fails Follow's Recv, which ends the subscription.
func (sub *testSubscription) end(t *testing.T) {
	t.Helper()

	sub.answer(t, reply{err: errDown})
}

func (sub *testSubscription) answer(t *testing.T, rep reply) {
	t.Helper()

	if sub.held == nil {
		sub.held = sub.ask(t)
	}

	sub.held <- rep
	sub.held = nil
}

func (sub *testSubscription) ask(t *testing.T) chan reply {
	t.Helper()

	select {
	case r := <-sub.asks:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("Follow did not ask for an event")

		return nil
	}
}

// failingSource refuses every subscription.
type failingSource struct {
	calls atomic.Int32
}

func (src *failingSource) Subscribe(context.Context, watch.Revision) (watch.Subscription, error) {
	src.calls.Add(1)

	return nil, errDown
}

// startFollow runs s.Follow on src until the test ends, and fails the test unless Follow then returns context.Canceled.
func startFollow(t *testing.T, s *Store, src watch.Source) {
	t.Helper()

	ctx := t.Context()
	done := make(chan error, 1)

	go func() { done <- s.Follow(ctx, src) }()

	t.Cleanup(func() {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Follow = %v; want context.Canceled", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Follow did not return when its context ended")
		}
	})
}

// parts cuts a reset to revision under source into one event for each group of lines.
func parts(t *testing.T, revision, source string, groups ...[]string) []watch.Event {
	t.Helper()

	events := make([]watch.Event, len(groups))

	for i, lines := range groups {
		p := &watch.Reset{Tuples: tuplesOf(t, lines...), More: i < len(groups)-1}
		if i == 0 {
			p.Revision, p.Schema = watch.Revision(revision), source
		}

		events[i] = watch.Event{Reset: p}
	}

	return events
}

// whole is a reset in one part.
func whole(t *testing.T, revision, source string, lines ...string) watch.Event {
	t.Helper()

	return parts(t, revision, source, lines)[0]
}

func deltaOf(revision string, source *string, changes ...watch.Change) watch.Event {
	return watch.Event{Delta: &watch.Delta{Revision: watch.Revision(revision), Schema: source, Changes: changes}}
}

func assertNotReady(t *testing.T, s *Store) {
	t.Helper()

	if _, err := s.Check(t.Context(), checkOf(t, "document:d1#view@user:alice")); !errors.Is(err, ErrNotReady) {
		t.Errorf("Check = %v; want ErrNotReady", err)
	}

	if rev := s.Revision(); rev != nil {
		t.Errorf("Revision = %q; want nil", rev)
	}
}

func assertRevision(t *testing.T, s *Store, want string) {
	t.Helper()

	if got := string(s.Revision()); got != want {
		t.Errorf("Revision = %q; want %s", got, want)
	}
}

// Follow serves nothing until a reset's last part has landed, and a reset replaces all the store held before.
func TestFollowServesAfterTheLastPart(t *testing.T) {
	t.Parallel()

	var s Store

	src := newTestSource()
	startFollow(t, &s, src)

	sub := src.next(t)
	if sub.from != nil {
		t.Errorf("first subscribed from %q; want nil", sub.from)
	}

	first := parts(t, "r1", testSchema,
		[]string{"document:d1#viewer@user:alice", "document:d1#banned@user:carol"},
		[]string{"document:d1#viewer@user:carol"},
		[]string{"document:d1#viewer@team:eng#member", "team:eng#member@user:bob"})

	for _, ev := range first[:2] {
		sub.land(t, ev)
		assertNotReady(t, &s)
	}

	sub.land(t, first[2])
	assertRevision(t, &s, "r1")
	assertChecks(t, &s, map[string]bool{
		"document:d1#view@user:alice": true,
		"document:d1#view@user:carol": false, // banned in one part, a viewer in another
		"document:d1#view@user:bob":   true,
	})

	second := parts(t, "r2", testSchema, []string{"document:d1#viewer@user:dave"}, []string{"document:d2#viewer@user:alice"})

	sub.land(t, second[0])
	assertNotReady(t, &s)

	sub.land(t, second[1])
	assertRevision(t, &s, "r2")
	assertChecks(t, &s, map[string]bool{
		"document:d1#view@user:alice": false,
		"document:d1#view@user:bob":   false,
		"document:d1#view@user:dave":  true,
		"document:d2#view@user:alice": true,
	})
}

// However a reset is cut into parts, once the last lands the replica holds just its tuples, nothing of the reset before.
func TestFollowHoldsEachResetWhole(t *testing.T) {
	t.Parallel()

	universe := modelTuples(t)

	//nolint:gosec // G404: a failing run has to be reproducible from its seed
	rnd := rand.New(rand.NewPCG(1, 0))

	var s Store

	src := newTestSource()
	startFollow(t, &s, src)

	sub := src.next(t)

	for round := range 20 {
		held := make(map[watch.Tuple]bool)
		pieces := [][]watch.Tuple{nil}

		for _, tp := range universe {
			if rnd.IntN(3) != 0 {
				continue
			}

			held[tp] = true

			if rnd.IntN(4) == 0 {
				pieces = append(pieces, nil)
			}

			pieces[len(pieces)-1] = append(pieces[len(pieces)-1], tp)
		}

		for i, piece := range pieces {
			p := &watch.Reset{Tuples: piece, More: i < len(pieces)-1}
			if i == 0 {
				p.Revision = watch.Revision(fmt.Sprint(round))
			}

			sub.land(t, watch.Event{Reset: p})
		}

		assertHolds(t, fmt.Sprintf("round %d, %d parts", round, len(pieces)), s.current, held)
	}
}

// Deltas move the store, a schema and a heartbeat's included.
func TestFollowAppliesDeltas(t *testing.T) {
	t.Parallel()

	var s Store

	src := newTestSource()
	startFollow(t, &s, src)

	sub := src.next(t)
	sub.land(t, whole(t, "r1", testSchema, "document:d1#viewer@user:alice", "document:d1#banned@user:carol"))
	sub.land(t, deltaOf("r2", nil, remove(t, "document:d1#viewer@user:alice"), touch(t, "document:d1#viewer@user:carol")))
	assertRevision(t, &s, "r2")
	assertChecks(t, &s, map[string]bool{
		"document:d1#view@user:alice": false,
		"document:d1#view@user:carol": false,
	})

	unbanned := strings.Replace(testSchema, "permission view = viewer - banned", "permission view = viewer", 1)
	sub.land(t, deltaOf("r3", &unbanned))
	assertChecks(t, &s, map[string]bool{"document:d1#view@user:carol": true})

	sub.land(t, deltaOf("r4", nil))
	assertRevision(t, &s, "r4")
}

// A subscription that ends leaves the store as it was, and the next starts from the revision it holds.
func TestFollowSubscribesAgainFromItsRevision(t *testing.T) {
	t.Parallel()

	var s Store

	src := newTestSource()
	startFollow(t, &s, src)

	sub := src.next(t)
	sub.land(t, whole(t, "r1", testSchema, "document:d1#viewer@user:alice"))
	sub.land(t, deltaOf("r2", nil, touch(t, "document:d1#viewer@user:dave")))
	sub.end(t)

	next := src.next(t)
	if got := string(next.from); got != "r2" {
		t.Errorf("subscribed again from %q; want r2", got)
	}

	if sub.ctx.Err() == nil {
		t.Error("the subscription that ended is still live")
	}

	assertChecks(t, &s, map[string]bool{"document:d1#view@user:alice": true, "document:d1#view@user:dave": true})

	next.land(t, deltaOf("r3", nil, remove(t, "document:d1#viewer@user:alice")))
	assertChecks(t, &s, map[string]bool{"document:d1#view@user:alice": false})
}

// An event the store cannot take, or a reset cut short, leaves it not ready: Follow starts over from nil.
func TestFollowStartsOverWhenAnEventFails(t *testing.T) {
	t.Parallel()

	broken := "definition {"

	for name, fail := range map[string]func(*testing.T, *Store, *testSubscription){
		"a reset cut short": func(t *testing.T, s *Store, sub *testSubscription) {
			sub.land(t, parts(t, "r2", testSchema, []string{"document:d1#viewer@user:bob"}, nil)[0])
			assertNotReady(t, s)
			sub.end(t)
		},
		"a delta inside a reset": func(t *testing.T, _ *Store, sub *testSubscription) {
			sub.land(t, parts(t, "r2", testSchema, []string{"document:d1#viewer@user:bob"}, nil)[0])
			sub.send(t, deltaOf("r3", nil))
		},
		"a reset inside a reset": func(t *testing.T, _ *Store, sub *testSubscription) {
			sub.land(t, parts(t, "r2", testSchema, []string{"document:d1#viewer@user:bob"}, nil)[0])
			sub.send(t, whole(t, "r3", testSchema, "document:d1#viewer@user:carol"))
		},
		"a schema in a later part": func(t *testing.T, _ *Store, sub *testSubscription) {
			sub.land(t, parts(t, "r2", testSchema, []string{"document:d1#viewer@user:bob"}, nil)[0])
			sub.send(t, watch.Event{Reset: &watch.Reset{Schema: testSchema}})
		},
		"a delta that does not compile": func(t *testing.T, _ *Store, sub *testSubscription) {
			sub.send(t, deltaOf("r2", &broken))
		},
		"a reset that does not compile": func(t *testing.T, _ *Store, sub *testSubscription) {
			sub.send(t, whole(t, "r2", broken))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var s Store

			src := newTestSource()
			startFollow(t, &s, src)

			sub := src.next(t)
			sub.land(t, whole(t, "r1", testSchema, "document:d1#viewer@user:alice"))
			fail(t, &s, sub)

			next := src.next(t)
			if next.from != nil {
				t.Errorf("subscribed again from %q; want nil", next.from)
			}

			assertNotReady(t, &s)

			next.land(t, whole(t, "r4", testSchema, "document:d1#viewer@user:bob"))
			assertChecks(t, &s, map[string]bool{"document:d1#view@user:alice": false, "document:d1#view@user:bob": true})
		})
	}
}

// An event Follow cannot read ends the subscription and leaves the store as it was: one with neither a reset nor
// a delta, one with both, and a part that continues a reset none has begun.
func TestFollowEndsOnAnEventItCannotRead(t *testing.T) {
	t.Parallel()

	bob := tuplesOf(t, "document:d1#viewer@user:bob")
	both := whole(t, "r2", testSchema, "document:d1#viewer@user:bob")
	both.Delta = deltaOf("r3", nil, remove(t, "document:d1#viewer@user:alice")).Delta

	for name, ev := range map[string]watch.Event{
		"neither":            {},
		"both":               both,
		"a part out of turn": {Reset: &watch.Reset{Tuples: bob}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var s Store

			src := newTestSource()
			startFollow(t, &s, src)

			sub := src.next(t)
			sub.land(t, whole(t, "r1", testSchema, "document:d1#viewer@user:alice"))
			sub.send(t, ev)

			if next := src.next(t); string(next.from) != "r1" {
				t.Errorf("subscribed again from %q; want r1", next.from)
			}

			assertChecks(t, &s, map[string]bool{"document:d1#view@user:alice": true, "document:d1#view@user:bob": false})
		})
	}
}

// What follow reports picks the next wait, so only a subscription that moved the store counts.
func TestFollowReportsWhetherTheStoreMoved(t *testing.T) {
	t.Parallel()

	broken := "definition {"

	for name, c := range map[string]struct {
		events []watch.Event
		open   bool // the events leave the subscription open, for the test to end
		want   bool
	}{
		"nothing":            {nil, true, false},
		"part of a reset":    {parts(t, "r1", testSchema, nil, nil)[:1], true, false},
		"a whole reset":      {parts(t, "r1", testSchema, nil, nil), true, true},
		"a delta":            {[]watch.Event{deltaOf("r1", nil)}, true, true},
		"a delta that fails": {[]watch.Event{deltaOf("r1", &broken)}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var s Store

			reset(t, &s, testSchema)

			src := newTestSource()
			ctx := t.Context()
			moved := make(chan bool, 1)

			go func() { moved <- s.follow(ctx, src) }()

			sub := src.next(t)
			for _, ev := range c.events {
				sub.send(t, ev)
			}

			if c.open {
				sub.end(t)
			}

			select {
			case got := <-moved:
				if got != c.want {
					t.Errorf("follow = %v; want %v", got, c.want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("follow did not return")
			}
		})
	}
}

// Subscriptions that keep failing are retried further and further apart: at 0, 0.1 and 0.3 s.
func TestFollowWaitsLongerAfterEachFailure(t *testing.T) {
	t.Parallel()

	var (
		s   Store
		src failingSource
	)

	ctx, cancel := context.WithTimeout(t.Context(), 350*time.Millisecond)
	defer cancel()

	if err := s.Follow(ctx, &src); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Follow = %v; want context.DeadlineExceeded", err)
	}

	if n := src.calls.Load(); n < 2 || n > 3 {
		t.Errorf("Follow subscribed %d times in 350 ms; want 2 or 3", n)
	}
}

func TestRetry(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		last  time.Duration
		moved bool
		want  time.Duration
	}{
		{0, false, minRetry},
		{minRetry, false, 2 * minRetry},
		{maxRetry * 3 / 4, false, maxRetry},
		{maxRetry, false, maxRetry},
		{maxRetry, true, minRetry},
	} {
		if got := retry(c.last, c.moved); got != c.want {
			t.Errorf("retry(%v, %v) = %v; want %v", c.last, c.moved, got, c.want)
		}
	}
}
