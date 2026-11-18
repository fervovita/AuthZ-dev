package fanout

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fervovita/AuthZ-dev/internal/datastore/postgres"
	"github.com/fervovita/AuthZ-dev/internal/watch"
)

var errDown = errors.New("store down")

// fakeStore is the central store in memory. Each write is a revision, named by its number.
type fakeStore struct {
	mu     sync.Mutex
	rev    int
	schema string
	tuples map[postgres.Tuple]bool
	log    []logged // what each write touched, in order
	cut    int      // Changes refuses a revision before it
	fail   error    // the next read fails with it

	snapshots int
	changes   []string // the revision each Changes was asked from

	afterSnapshot func() // runs once, after a Snapshot is read: a commit landing between two reads of one tick
}

type logged struct {
	rev    int
	tuples []postgres.Tuple
	schema bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{tuples: make(map[postgres.Tuple]bool)}
}

// tupleOf reads "type:id#relation@type:id" with an optional "#relation" after the subject.
func tupleOf(line string) postgres.Tuple {
	resource, subject, _ := strings.Cut(line, "@")
	object, relation, _ := strings.Cut(resource, "#")
	typ, id, _ := strings.Cut(object, ":")
	subjectObject, subjectRelation, _ := strings.Cut(subject, "#")
	subjectType, subjectID, _ := strings.Cut(subjectObject, ":")

	return postgres.Tuple{
		ResourceType: typ, ResourceID: id, Relation: relation,
		SubjectType: subjectType, SubjectID: subjectID, SubjectRelation: subjectRelation,
	}
}

// write stores the tuples of lines, or removes them, as one revision.
func (f *fakeStore) write(present bool, lines ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.rev++
	l := logged{rev: f.rev}

	for _, line := range lines {
		t := tupleOf(line)
		if present {
			f.tuples[t] = true
		} else {
			delete(f.tuples, t)
		}

		l.tuples = append(l.tuples, t)
	}

	f.log = append(f.log, l)
}

func (f *fakeStore) writeSchema(source string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.rev++
	f.schema = source
	f.log = append(f.log, logged{rev: f.rev, schema: true})
}

// pruneAll makes every revision but the newest too old to catch up from.
func (f *fakeStore) pruneAll() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.cut = f.rev
}

func (f *fakeStore) failNext(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.fail = err
}

func (f *fakeStore) revision() postgres.Revision {
	return postgres.Revision(strconv.Itoa(f.rev))
}

func (f *fakeStore) Snapshot(context.Context) (postgres.State, error) {
	f.mu.Lock()

	if err := f.fail; err != nil {
		f.fail = nil
		f.mu.Unlock()

		return postgres.State{}, err
	}

	f.snapshots++

	st := postgres.State{Revision: f.revision(), Schema: f.schema}
	for t := range f.tuples {
		st.Tuples = append(st.Tuples, t)
	}

	slices.SortFunc(st.Tuples, func(a, b postgres.Tuple) int { return strings.Compare(a.String(), b.String()) })

	after := f.afterSnapshot
	f.afterSnapshot = nil
	f.mu.Unlock()

	if after != nil {
		after()
	}

	return st, nil
}

func (f *fakeStore) Changes(_ context.Context, since postgres.Revision) (postgres.Delta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.changes = append(f.changes, string(since))

	if err := f.fail; err != nil {
		f.fail = nil

		return postgres.Delta{}, err
	}

	n, err := strconv.Atoi(string(since))
	if err != nil {
		return postgres.Delta{}, fmt.Errorf("revision %q: %w", since, err)
	}

	if n < f.cut {
		return postgres.Delta{}, postgres.ErrPruned
	}

	d := postgres.Delta{Revision: f.revision()}
	seen := make(map[postgres.Tuple]bool)

	for _, l := range f.log {
		if l.rev <= n {
			continue
		}

		if l.schema {
			source := f.schema
			d.Schema = &source
		}

		for _, t := range l.tuples {
			if !seen[t] {
				seen[t] = true
				d.Changes = append(d.Changes, postgres.Change{Tuple: t, Present: f.tuples[t]})
			}
		}
	}

	return d, nil
}

// reads reports how many reads f has served: Snapshots, and Changes from each revision.
func (f *fakeStore) reads() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.snapshots, slices.Clone(f.changes)
}

// reader follows a subscription as a sidecar would, and fails the test on an event that breaks the watch contract.
type reader struct {
	t   *testing.T
	sub *subscription
	c   contract

	rev    watch.Revision
	schema string
	tuples map[watch.Tuple]bool // nil until a reset has landed

	first *watch.Reset         // the first part of the reset under way
	parts map[watch.Tuple]bool // what the reset under way holds so far

	resets, deltas int
}

// follow subscribes to h from nothing, as a sidecar that has just started.
func follow(t *testing.T, h *Hub) *reader {
	t.Helper()

	return subscribe(t, h, nil)
}

func subscribe(t *testing.T, h *Hub, from watch.Revision) *reader {
	t.Helper()

	sub, err := h.Subscribe(t.Context(), from)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	return &reader{t: t, sub: sub.(*subscription), c: contract{from: from}}
}

// resume subscribes again from where r is, as a sidecar does when its subscription ends.
func (r *reader) resume(h *Hub) *reader {
	r.t.Helper()

	next := subscribe(r.t, h, r.rev)
	next.rev, next.schema, next.tuples = r.rev, r.schema, maps.Clone(r.tuples)

	return next
}

// catchUp takes every event waiting for r.
func (r *reader) catchUp() {
	r.t.Helper()

	for len(r.sub.events) > 0 || r.sub.reset != nil {
		ev, err := r.sub.Recv()
		if err != nil {
			r.t.Fatalf("Recv: %v", err)
		}

		r.take(ev)
	}
}

func (r *reader) take(ev watch.Event) {
	r.t.Helper()

	if err := r.c.check(ev); err != nil {
		r.t.Fatal(err)
	}

	if p := ev.Reset; p != nil {
		// Every follower is handed the hub's one copy, so a part has to end where its tuples do.
		if cap(p.Tuples) != len(p.Tuples) {
			r.t.Fatal("a reset part reaches into the next: appending to it would write over another follower's")
		}

		if len(p.Revision) > 0 {
			r.first, r.parts = p, make(map[watch.Tuple]bool)
		}

		for _, t := range p.Tuples {
			r.parts[t] = true
		}

		if !p.More {
			r.rev, r.schema, r.tuples = r.first.Revision, r.first.Schema, r.parts
			r.first, r.parts = nil, nil
			r.resets++
		}

		return
	}

	d := ev.Delta

	for _, c := range d.Changes {
		if c.Present {
			r.tuples[c.Tuple] = true
		} else {
			delete(r.tuples, c.Tuple)
		}
	}

	if d.Schema != nil {
		r.schema = *d.Schema
	}

	r.rev = d.Revision
	r.deltas++
}

// assertHolds checks that r holds what f does, at f's revision.
func (r *reader) assertHolds(f *fakeStore) {
	r.t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	want := make([]string, 0, len(f.tuples))
	for t := range f.tuples {
		want = append(want, t.String())
	}

	got := make([]string, 0, len(r.tuples))
	for t := range r.tuples {
		got = append(got, postgres.Tuple(t).String())
	}

	slices.Sort(want)
	slices.Sort(got)

	if !slices.Equal(got, want) || r.schema != f.schema || string(r.rev) != string(f.revision()) {
		r.t.Errorf("reader holds %q under %q at %s; the store holds %q under %q at %s",
			got, r.schema, r.rev, want, f.schema, f.revision())
	}
}

// assertEnded checks that the hub has ended r's subscription for want.
func (r *reader) assertEnded(want error) {
	r.t.Helper()

	select {
	case <-r.sub.done:
	default:
		r.t.Fatalf("the subscription goes on; want it ended for %v", want)
	}

	if _, err := r.sub.Recv(); !errors.Is(err, want) {
		r.t.Errorf("Recv = %v; want %v", err, want)
	}
}

// lines returns n tuples of document#viewer.
func lines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("document:d%d#viewer@user:u%d", i, i)
	}

	return out
}

// A sidecar that starts from nothing gets the whole store as a reset cut into parts, then what changes.
func TestJoinFromNothingGetsTheStoreInParts(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	f.writeSchema("definition user {}")
	f.write(true, lines(2*partSize+partSize/2)...)

	h := newHub(f, time.Hour)
	r := follow(t, h)

	h.step(t.Context())
	r.catchUp()
	r.assertHolds(f)

	if r.resets != 1 || r.deltas != 1 {
		t.Errorf("took %d resets and %d deltas; want the reset and the tick's delta", r.resets, r.deltas)
	}

	f.write(false, "document:d0#viewer@user:u0")
	f.writeSchema("definition user {}\n\ndefinition team {}")
	h.step(t.Context())
	r.catchUp()
	r.assertHolds(f)
}

// A reset of an empty store is one part, which still carries the revision.
func TestJoinAnEmptyStore(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	h := newHub(f, time.Hour)
	r := follow(t, h)

	h.step(t.Context())

	ev, err := r.sub.Recv()
	if err != nil || ev.Reset == nil || ev.Reset.More || len(ev.Reset.Revision) == 0 {
		t.Fatalf("Recv = %+v, %v; want one part with a revision", ev, err)
	}
}

// Those that join in one tick share its reads: one Snapshot for all that start from nothing,
// and one Changes for those that come back from the same revision.
func TestJoinersShareReads(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	f.write(true, "document:d1#viewer@user:alice")

	h := newHub(f, time.Hour)
	first := follow(t, h)

	h.step(t.Context())
	first.catchUp()

	at := first.rev

	f.write(true, "document:d1#viewer@user:bob")
	h.step(t.Context())
	first.catchUp()

	f.write(true, "document:d1#viewer@user:carol")

	fresh := []*reader{follow(t, h), follow(t, h), follow(t, h)}
	back := []*reader{first.resume(h), first.resume(h)}
	older := subscribe(t, h, at)
	older.rev, older.tuples = at, map[watch.Tuple]bool{watch.Tuple(tupleOf("document:d1#viewer@user:alice")): true}

	snapshotsBefore, changesBefore := f.reads()

	h.step(t.Context())

	snapshots, changes := f.reads()
	if got, want := snapshots-snapshotsBefore, 1; got != want {
		t.Errorf("the tick read %d Snapshots; want %d", got, want)
	}

	// One for each revision some came back from, and the broadcast.
	if got, want := len(changes)-len(changesBefore), 3; got != want {
		t.Errorf("the tick read Changes %d times (%q); want %d", got, changes[len(changesBefore):], want)
	}

	// Those from nothing are handed one reset between them, not a copy each.
	var firsts []*watch.Reset

	for _, r := range fresh[:2] {
		ev, err := r.sub.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}

		r.take(ev)
		firsts = append(firsts, ev.Reset)
	}

	if &firsts[0].Tuples[0] != &firsts[1].Tuples[0] {
		t.Error("the joiners got resets of their own, not one to share")
	}

	for _, r := range append(append(fresh, back...), older) {
		r.catchUp()
		r.assertHolds(f)
	}

	if back[0].resets != 0 || older.resets != 0 {
		t.Error("a subscription that came back from a revision the log reaches got a reset")
	}
}

// A subscription from a revision the log no longer reaches gets a reset instead, while the hub's own head is still
// reached and the others go on.
func TestJoinBehindTheCutGetsAReset(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	f.write(true, "document:d1#viewer@user:alice")

	h := newHub(f, time.Hour)
	r := follow(t, h)

	h.step(t.Context())
	r.catchUp()

	at, held := r.rev, maps.Clone(r.tuples)

	f.write(false, "document:d1#viewer@user:alice")
	h.step(t.Context())
	r.catchUp()
	f.pruneAll()

	back := subscribe(t, h, at)
	back.rev, back.tuples = at, held

	h.step(t.Context())
	back.catchUp()
	back.assertHolds(f)
	r.catchUp()
	r.assertHolds(f)

	if back.resets != 1 {
		t.Errorf("took %d resets; want one", back.resets)
	}
}

// A commit that lands between a joiner's reset and the tick's broadcast reaches it and everyone else:
// the broadcast is read from the head, which the reset already holds.
func TestACommitDuringATickReachesEveryone(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	f.write(true, "document:d1#viewer@user:alice")

	h := newHub(f, time.Hour)
	live := follow(t, h)

	h.step(t.Context())
	live.catchUp()

	f.write(true, "document:d1#viewer@user:bob")
	joiner := follow(t, h)
	f.afterSnapshot = func() { f.write(true, "document:d1#viewer@user:carol") }

	h.step(t.Context())

	for _, r := range []*reader{live, joiner} {
		r.catchUp()
		r.assertHolds(f)
	}
}

// Every live subscription gets every tick's delta, the same one, even with nothing changed.
func TestEveryTickReachesEveryone(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	h := newHub(f, time.Hour)
	rs := []*reader{follow(t, h), follow(t, h)}

	h.step(t.Context())

	for _, r := range rs {
		r.catchUp()
	}

	h.step(t.Context())

	var got []*watch.Delta

	for _, r := range rs {
		ev, err := r.sub.Recv()
		if err != nil || ev.Delta == nil || len(ev.Delta.Changes) != 0 {
			t.Fatalf("Recv = %+v, %v; want a delta with nothing changed", ev, err)
		}

		got = append(got, ev.Delta)
	}

	if got[0] != got[1] {
		t.Error("the subscriptions got deltas of their own, not one to share")
	}
}

// With no subscription the hub reads nothing and forgets the head: the next to join sets it afresh, from a revision
// the log may have dropped in the meantime.
func TestNoSubscriptionsNoReads(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	f.write(true, "document:d1#viewer@user:alice")

	h := newHub(f, time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	sub, err := h.Subscribe(ctx, nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	h.step(t.Context())
	cancel()

	if _, err := sub.Recv(); !errors.Is(err, context.Canceled) {
		t.Errorf("Recv = %v; want context.Canceled", err)
	}

	snapshots, changes := f.reads()

	f.write(true, "document:d1#viewer@user:bob")
	f.pruneAll()
	h.step(t.Context())

	if s, c := f.reads(); s != snapshots || len(c) != len(changes) || h.head != nil {
		t.Errorf("with no subscription the hub read %d more times, or kept the head %q",
			s-snapshots+len(c)-len(changes), h.head)
	}

	r := follow(t, h)

	h.step(t.Context())
	r.catchUp()
	r.assertHolds(f)
}

// A subscription that falls a buffer behind is cut off; the others go on.
func TestASlowSubscriptionIsCutOff(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	h := newHub(f, time.Hour)
	slow, quick := follow(t, h), follow(t, h)

	// The first tick sends two events, the reset and the broadcast; each tick after it, one.
	for range buffer - 1 {
		h.step(t.Context())
		quick.catchUp()
	}

	if len(h.live) != 2 || len(slow.sub.events) != buffer {
		t.Fatalf("%d subscriptions live and %d events waiting; want 2 and a full buffer", len(h.live), len(slow.sub.events))
	}

	f.write(true, "document:d1#viewer@user:alice")
	h.step(t.Context())

	slow.assertEnded(errSlow)

	quick.catchUp()
	quick.assertHolds(f)

	back := slow.resume(h) // from nothing: it took no event

	h.step(t.Context())
	back.catchUp()
	back.assertHolds(f)
}

// A hub whose head the log no longer reaches ends every subscription: each comes back from where it is.
func TestHubBehindTheCutEndsEveryone(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	f.write(true, "document:d1#viewer@user:alice")

	h := newHub(f, time.Hour)
	rs := []*reader{follow(t, h), follow(t, h)}

	h.step(t.Context())

	for _, r := range rs {
		r.catchUp()
	}

	f.write(true, "document:d1#viewer@user:bob")
	f.pruneAll()
	h.step(t.Context())

	for _, r := range rs {
		r.assertEnded(postgres.ErrPruned)

		back := r.resume(h)

		h.step(t.Context())
		back.catchUp()
		back.assertHolds(f)
	}
}

// A read that fails costs a tick, not a change: the head stays, so the next tick sends both ticks' changes.
// A joiner whose read fails, a Snapshot or Changes from its revision, is ended to come back later.
func TestAFailedReadLosesNothing(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	h := newHub(f, time.Hour)
	r := follow(t, h)

	h.step(t.Context())
	r.catchUp()

	f.write(true, "document:d1#viewer@user:alice")
	f.failNext(errDown)
	h.step(t.Context())

	if len(r.sub.events) != 0 {
		t.Fatal("a tick whose read failed sent something")
	}

	f.write(true, "document:d1#viewer@user:bob")
	h.step(t.Context())
	r.catchUp()
	r.assertHolds(f)

	for _, from := range []watch.Revision{nil, r.rev} {
		joiner := subscribe(t, h, from)
		f.failNext(errDown)
		h.step(t.Context())

		joiner.assertEnded(errDown)
		r.catchUp()
		r.assertHolds(f)
	}
}

// A follower that leaves is told so at once, between a reset's parts or waiting for its first,
// and its subscription is dropped at the next tick without a read.
func TestALeftSubscriptionIsDropped(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	f.write(true, lines(partSize+1)...)

	h := newHub(f, time.Hour)
	stay := follow(t, h)

	midwayCtx, leaveMidway := context.WithCancel(t.Context())

	midway, err := h.Subscribe(midwayCtx, nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	h.step(t.Context())
	stay.catchUp()

	if ev, err := midway.Recv(); err != nil || ev.Reset == nil || !ev.Reset.More {
		t.Fatalf("Recv = %+v, %v; want the reset's first part", ev, err)
	}

	leaveMidway()

	if _, err := midway.Recv(); !errors.Is(err, context.Canceled) {
		t.Errorf("Recv after leaving midway = %v; want context.Canceled", err)
	}

	waitingCtx, leaveWaiting := context.WithCancel(t.Context())

	waiting, err := h.Subscribe(waitingCtx, nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	got := make(chan error, 1)

	go func() {
		_, err := waiting.Recv()
		got <- err
	}()

	time.Sleep(10 * time.Millisecond) // for Recv to be waiting when its follower leaves
	leaveWaiting()

	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Recv after leaving while waiting = %v; want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recv went on waiting after its follower left")
	}

	snapshots, _ := f.reads()

	h.step(t.Context())

	if s, _ := f.reads(); s != snapshots || len(h.live) != 1 || h.live[0] != stay.sub {
		t.Errorf("the tick read %d Snapshots and left %d subscriptions live; want none and only the one still followed",
			s-snapshots, len(h.live))
	}
}

// Run ticks until its context ends, then ends every subscription at once, even one midway through a reset,
// and refuses new ones.
func TestRunTicksUntilItsContextEnds(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	f.write(true, lines(partSize+1)...)

	h := newHub(f, time.Millisecond)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- h.Run(ctx) }()

	sub, err := h.Subscribe(t.Context(), nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if ev, err := sub.Recv(); err != nil || ev.Reset == nil || !ev.Reset.More {
		t.Fatalf("Recv = %+v, %v; want the first part of the reset a tick sends", ev, err)
	}

	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v; want context.Canceled", err)
	}

	if _, err := sub.Recv(); !errors.Is(err, errStopped) {
		t.Errorf("Recv = %v; want errStopped", err)
	}

	if _, err := h.Subscribe(t.Context(), nil); !errors.Is(err, errStopped) {
		t.Errorf("Subscribe after Run = %v; want errStopped", err)
	}
}

// A subscription still waiting for its first tick when Run returns is ended too.
func TestRunEndsThoseYetToStart(t *testing.T) {
	t.Parallel()

	h := newHub(newFakeStore(), time.Hour)
	r := follow(t, h)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := h.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v; want context.Canceled", err)
	}

	r.assertEnded(errStopped)
}
