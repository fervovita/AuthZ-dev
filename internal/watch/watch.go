package watch

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
