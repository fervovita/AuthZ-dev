package memstore

import (
	"context"
	"time"

	"github.com/fervovita/AuthZ-dev/internal/watch"
)

// How long Follow waits before subscribing again: minRetry after a subscription that moved the store,
// then twice as long after each one that did not, up to maxRetry.
const (
	minRetry = 100 * time.Millisecond
	maxRetry = 10 * time.Second
)

// Follow keeps s at the newest revision src sends until ctx ends, then returns ctx's error.
// Whenever a subscription ends it subscribes again, from the revision s holds or from nil while s is not ready.
func (s *Store) Follow(ctx context.Context, src watch.Source) error {
	var wait time.Duration

	for {
		wait = retry(wait, s.follow(ctx, src))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// retry is the wait after a subscription, given the wait before it and whether it moved the store.
func retry(last time.Duration, moved bool) time.Duration {
	if moved {
		return minRetry
	}

	return min(max(2*last, minRetry), maxRetry)
}

// follow runs one subscription to its end and reports whether s moved on it.
// A reset's parts are interned as they arrive and land together after the last: s holds nothing from the first.
func (s *Store) follow(ctx context.Context, src watch.Source) bool {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sub, err := src.Subscribe(ctx, s.Revision())
	if err != nil {
		return false
	}

	var (
		moved bool
		r     *replica // the reset under way
		items []item
	)

	for {
		ev, err := sub.Recv()
		if err != nil {
			return moved
		}

		switch {
		case ev.Reset != nil && ev.Delta == nil:
			p := ev.Reset
			first := len(p.Revision) > 0

			// A part with a revision begins a reset, and one with neither a revision nor a schema continues it.
			// Any other part, or one out of turn, would leave the replica holding what no single reset sent.
			if first == (r != nil) || (!first && p.Schema != "") {
				return moved
			}

			if first {
				s.set(nil)

				if r, err = newReplica(p.Revision, p.Schema, nil); err != nil {
					return moved
				}
			}

			if items, err = r.load(items, p.Tuples); err != nil {
				return moved
			}

			if !p.More {
				if err := r.merge(items); err != nil {
					return moved
				}

				s.set(r)

				r, items, moved = nil, nil, true
			}
		case ev.Delta != nil && ev.Reset == nil:
			// A delta moves s to its revision, so one without a revision leaves nowhere to subscribe again from.
			if len(ev.Delta.Revision) == 0 {
				return moved
			}

			// Within a reset s holds nothing, so Apply refuses the delta.
			if err := s.Apply(ev.Delta.Revision, ev.Delta.Schema, ev.Delta.Changes); err != nil {
				return moved
			}

			moved = true
		default:
			return moved // neither or both set: an event this version does not know
		}
	}
}
