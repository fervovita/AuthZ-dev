package core

// ObjectRef names the interned type and identifier.
type ObjectRef struct {
	Type TypeID
	ID   ObjectID
}

// SubjectRef names the subject side of a tuple.
type SubjectRef struct {
	Type     TypeID
	ID       ObjectID
	Relation RelationID
	Wildcard bool
}

// Tuple is one stored relationship: Subject holds Relation on Object.
type Tuple struct {
	Object   ObjectRef
	Relation RelationID
	Subject  SubjectRef
}

// WildcardSubject returns the subject matching every subject of typ.
func WildcardSubject(typ TypeID) SubjectRef {
	return SubjectRef{Type: typ, Wildcard: true}
}

// Valid reports whether every component resolved.
func (r ObjectRef) Valid() bool {
	return r.Type != 0 && r.ID != 0
}

// Valid reports whether the subject is well-formed.
func (s SubjectRef) Valid() bool {
	if s.Type == 0 {
		return false
	}

	if s.Wildcard {
		return s.ID == 0 && s.Relation == NoRelation
	}

	return s.ID != 0
}

// Valid reports whether the tuple is well-formed.
func (t Tuple) Valid() bool {
	return t.Object.Valid() && t.Relation != NoRelation && t.Subject.Valid()
}
