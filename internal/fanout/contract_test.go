package fanout

import (
	"errors"
	"fmt"

	"github.com/fervovita/AuthZ-dev/internal/watch"
)

// contract follows one subscription's events and reports the first that breaks the watch contract,
// so a hub that sends one fails the test instead of a follower quietly subscribing again.
type contract struct {
	from    watch.Revision
	started bool                 // an event has come
	parts   map[watch.Tuple]bool // what the reset under way has named; nil between resets
}

func (c *contract) check(ev watch.Event) error {
	defer func() { c.started = true }()

	switch {
	case (ev.Reset == nil) == (ev.Delta == nil):
		return errors.New("an event with neither a reset nor a delta, or both")
	case ev.Reset != nil:
		p := ev.Reset
		first := len(p.Revision) > 0

		if first == (c.parts != nil) || !first && p.Schema != "" {
			return fmt.Errorf("a reset part out of turn: revision %q, schema %q", p.Revision, p.Schema)
		}

		if first {
			c.parts = make(map[watch.Tuple]bool)
		}

		for _, t := range p.Tuples {
			if c.parts[t] {
				return fmt.Errorf("a reset names %v twice", t)
			}

			c.parts[t] = true
		}

		if !p.More {
			c.parts = nil
		}

		return nil
	}

	d := ev.Delta

	switch {
	case c.parts != nil:
		return errors.New("a delta inside a reset")
	case !c.started && len(c.from) == 0:
		return errors.New("a delta first, to a subscription from nothing")
	case len(d.Revision) == 0:
		return errors.New("a delta without a revision")
	}

	seen := make(map[watch.Tuple]bool, len(d.Changes))

	for _, ch := range d.Changes {
		if seen[ch.Tuple] {
			return fmt.Errorf("a delta names %v twice", ch.Tuple)
		}

		seen[ch.Tuple] = true
	}

	return nil
}
