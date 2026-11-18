package fanout

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/fervovita/AuthZ-dev/internal/datastore/postgres"
	"github.com/fervovita/AuthZ-dev/internal/watch"
)

const (
	partSize = 1000 // tuples in a reset part
	buffer   = 256  // events a subscription may fall behind by, before it is cut off
)

var (
	errStopped = errors.New("fanout: hub stopped")
	errSlow    = errors.New("fanout: subscription fell behind")
)

// store is what the hub reads: a postgres.Store.
type store interface {
	Snapshot(ctx context.Context) (postgres.State, error)
	Changes(ctx context.Context, since postgres.Revision) (postgres.Delta, error)
}

// Hub is a watch.Source for any number of followers: once a tick it reads what changed and sends it to all of them.
// Run does the reading; Subscribe may be called from any goroutine.
type Hub struct {
	st   store
	tick time.Duration

	mu      sync.Mutex
	joining []*subscription // subscribed since the last tick
	stopped bool

	// Run's alone.
	live []*subscription
	head postgres.Revision // every live subscription has been sent the store up to it. A tick with none live drops it
}

// New returns a hub on st that reads once every tick.
func New(st *postgres.Store, tick time.Duration) *Hub {
	return newHub(st, tick)
}

func newHub(st store, tick time.Duration) *Hub {
	return &Hub{st: st, tick: tick}
}

// Run reads the store once a tick until ctx ends, then ends every subscription and returns ctx's error.
func (h *Hub) Run(ctx context.Context) error {
	t := time.NewTicker(h.tick)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			h.stop()

			return ctx.Err()
		case <-t.C:
			h.step(ctx)
		}
	}
}

// Subscribe starts a subscription at from. The next tick sends where it starts: a delta from from, or a reset.
// Once Run has returned it fails.
func (h *Hub) Subscribe(ctx context.Context, from watch.Revision) (watch.Subscription, error) {
	sub := &subscription{ctx: ctx, from: from, events: make(chan event, buffer), done: make(chan struct{})}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.stopped {
		return nil, errStopped
	}

	h.joining = append(h.joining, sub)

	return sub, nil
}

// step is one tick: each new subscription is sent where it starts,
// then every subscription is sent what changed after that.
func (h *Hub) step(ctx context.Context) {
	h.mu.Lock()
	joining := h.joining
	h.joining = nil
	h.mu.Unlock()

	if h.live = slices.DeleteFunc(h.live, (*subscription).over); len(h.live) == 0 {
		h.head = nil // none to send to, so nothing is read
	}

	h.join(ctx, slices.DeleteFunc(joining, (*subscription).over))

	if len(h.live) > 0 {
		h.broadcast(ctx)
	}
}

// join sends each new subscription a delta from its revision, or a reset if it has none or the log no longer
// reaches it, and makes it live. Those from one revision share a read, as all resets share one Snapshot.
func (h *Hub) join(ctx context.Context, subs []*subscription) {
	var resets []*subscription

	for _, group := range byRevision(subs) {
		if len(group[0].from) == 0 {
			resets = append(resets, group...)

			continue
		}

		d, err := h.st.Changes(ctx, postgres.Revision(group[0].from))

		switch {
		case errors.Is(err, postgres.ErrPruned):
			resets = append(resets, group...)
		case err != nil:
			end(group, fmt.Errorf("fanout: %w", err))
		default:
			h.start(group, event{delta: deltaOf(d)}, d.Revision)
		}
	}

	if len(resets) == 0 {
		return
	}

	st, err := h.st.Snapshot(ctx)
	if err != nil {
		end(resets, fmt.Errorf("fanout: %w", err))

		return
	}

	h.start(resets, event{reset: resetOf(st)}, st.Revision)
}

// start sends subs their first event, read at rev, and makes them live. With none live before, rev becomes the head.
func (h *Hub) start(subs []*subscription, ev event, rev postgres.Revision) {
	if h.head == nil {
		h.head = rev
	}

	for _, sub := range subs {
		sub.events <- ev // the first it is sent, so there is room
		h.live = append(h.live, sub)
	}
}

// broadcast sends every live subscription what changed since the head, and moves the head on.
// One with no room is cut off.
func (h *Hub) broadcast(ctx context.Context) {
	d, err := h.st.Changes(ctx, h.head)

	switch {
	case errors.Is(err, postgres.ErrPruned):
		// No one can be sent what they missed: all start again, from where they are or from a reset.
		end(h.live, fmt.Errorf("fanout: %w", err))
		h.live = nil

		return
	case err != nil:
		return // the head stays, so the next tick sends what this one could not
	}

	ev := event{delta: deltaOf(d)}

	h.live = slices.DeleteFunc(h.live, func(sub *subscription) bool { return !sub.send(ev) })
	h.head = d.Revision
}

// stop ends every subscription, and every one to come.
func (h *Hub) stop() {
	h.mu.Lock()
	h.stopped = true
	joining := h.joining
	h.joining = nil
	h.mu.Unlock()

	end(joining, errStopped)
	end(h.live, errStopped)
	h.live = nil
}

// byRevision groups subs by the revision they start from, in the order each revision first came.
func byRevision(subs []*subscription) [][]*subscription {
	var groups [][]*subscription

	at := make(map[string]int)

	for _, sub := range subs {
		i, ok := at[string(sub.from)]
		if !ok {
			i = len(groups)
			at[string(sub.from)] = i
			groups = append(groups, nil)
		}

		groups[i] = append(groups[i], sub)
	}

	return groups
}

// resetOf turns st into the watch type once, for every subscription to hand over in parts.
func resetOf(st postgres.State) *watch.Reset {
	tuples := make([]watch.Tuple, len(st.Tuples))
	for i, t := range st.Tuples {
		tuples[i] = watch.Tuple(t)
	}

	return &watch.Reset{Revision: watch.Revision(st.Revision), Schema: st.Schema, Tuples: tuples}
}

// deltaOf turns d into the watch type once, for every subscription to share.
func deltaOf(d postgres.Delta) *watch.Delta {
	changes := make([]watch.Change, len(d.Changes))
	for i, c := range d.Changes {
		changes[i] = watch.Change{Tuple: watch.Tuple(c.Tuple), Present: c.Present}
	}

	return &watch.Delta{Revision: watch.Revision(d.Revision), Schema: d.Schema, Changes: changes}
}

func end(subs []*subscription, err error) {
	for _, sub := range subs {
		sub.end(err)
	}
}
