package fanout

import (
	"context"
	"sync"

	"github.com/fervovita/AuthZ-dev/internal/watch"
)

// event is what the hub sends a subscription: a delta, or the whole store as one reset that Recv hands over in parts.
// Every subscription it goes to shares it.
type event struct {
	delta *watch.Delta
	reset *watch.Reset
}

// subscription is one follower's place in the hub. The hub sends it events, and Recv hands them over.
type subscription struct {
	ctx    context.Context
	from   watch.Revision
	events chan event
	done   chan struct{} // closed once the hub ends the subscription, after err is set
	err    error
	once   sync.Once

	// Recv's alone: the reset being handed over, and how many of its tuples have gone.
	reset *watch.Reset
	sent  int
}

// Recv hands over the next event, a reset part by part. Once the hub ends the subscription it says why at once,
// events waiting or not: coming back takes them as one delta.
func (sub *subscription) Recv() (watch.Event, error) {
	select {
	case <-sub.done:
		return watch.Event{}, sub.err
	default:
		if err := sub.ctx.Err(); err != nil {
			return watch.Event{}, err
		}
	}

	if sub.reset == nil {
		select {
		case ev := <-sub.events:
			if ev.delta != nil {
				return watch.Event{Delta: ev.delta}, nil
			}

			sub.reset, sub.sent = ev.reset, 0
		case <-sub.done:
			return watch.Event{}, sub.err
		case <-sub.ctx.Done():
			return watch.Event{}, sub.ctx.Err()
		}
	}

	return watch.Event{Reset: sub.part()}, nil
}

// part cuts the next part off the reset being handed over. Only the first carries its revision and schema.
func (sub *subscription) part() *watch.Reset {
	all := sub.reset
	next := min(sub.sent+partSize, len(all.Tuples))

	// Capped, so a follower that appends to its part cannot write over the next one.
	p := &watch.Reset{Tuples: all.Tuples[sub.sent:next:next], More: next < len(all.Tuples)}
	if sub.sent == 0 {
		p.Revision, p.Schema = all.Revision, all.Schema
	}

	if sub.sent = next; !p.More {
		sub.reset = nil
	}

	return p
}

// send queues ev, or cuts sub off if it has fallen too far behind to take it.
func (sub *subscription) send(ev event) bool {
	select {
	case sub.events <- ev:
		return true
	default:
		sub.end(errSlow)

		return false
	}
}

func (sub *subscription) end(err error) {
	sub.once.Do(func() {
		sub.err = err
		close(sub.done)
	})
}

// over reports whether sub needs nothing more: the hub has ended it, or its follower's ctx has.
func (sub *subscription) over() bool {
	select {
	case <-sub.done:
		return true
	default:
		return sub.ctx.Err() != nil
	}
}
