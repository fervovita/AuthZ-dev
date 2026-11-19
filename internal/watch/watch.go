package watch

import "context"

// Revision is a point in the central store's history.
// Keep it and pass it back; do not parse or compare it.
type Revision []byte

// Tuple is one stored relationship.
type Tuple struct {
	ResourceType    string
	ResourceID      string
	Relation        string
	SubjectType     string
	SubjectID       string // "*" names every subject of SubjectType
	SubjectRelation string // "" names the subject itself
}

// Change is a tuple's state at the revision a follower moves to: there or not, whatever happened in between.
type Change struct {
	Tuple   Tuple
	Present bool
}

// Source is the central store as a follower sees it.
type Source interface {
	// Subscribe starts a subscription at from, which is nil or a revision an earlier subscription delivered.
	// It opens with a reset if from is nil or cannot be caught up from, too old or not a revision at all,
	// and otherwise with a delta from it.
	// Ending ctx ends the subscription.
	Subscribe(ctx context.Context, from Revision) (Subscription, error)
}

// Subscription hands over its events in order. Once Recv returns an error it is over: subscribe again.
type Subscription interface {
	Recv() (Event, error)
}

// Event is a part of a reset or a delta: exactly one is set.
// A source may hand one event to many followers, so none may modify it.
type Event struct {
	Reset *Reset
	Delta *Delta
}

// Reset is part of the whole store at Revision, which replaces everything a follower holds.
// Only the first part carries Revision and Schema, and More says the next event is another part.
// The parts name each tuple once between them. A follower serves after the last.
type Reset struct {
	Revision Revision
	Schema   string // the source in force; "" before any is written
	Tuples   []Tuple
	More     bool
}

// Delta moves a follower from the revision it holds to Revision.
// Changes holds every tuple touched in between, each once, and may restate some as the follower already holds them.
type Delta struct {
	Revision Revision
	Schema   *string  // the source in force at Revision if one was written since, nil otherwise
	Changes  []Change // none is a heartbeat
}
