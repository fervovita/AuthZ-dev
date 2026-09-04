package core

import (
	"strconv"
	"strings"
)

const (
	unresolvedName = "?"         // a zero id: the dictionary has not seen the name
	danglingName   = "!dangling" // a nonzero id that does not resolve, which should be impossible
)

// InternObjectRef resolves a resource reference, adding unseen strings, and does
// not validate: the loader calls ValidObject and counts what fails.
func (d *Dictionary) InternObjectRef(typ, id string) (ObjectRef, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	t, err := d.types.intern(typ, d.limit)
	if err != nil {
		return ObjectRef{}, err
	}

	o, err := d.objects.intern(id, d.limit)
	if err != nil {
		return ObjectRef{}, err
	}

	return ObjectRef{Type: TypeID(t), ID: ObjectID(o)}, nil
}

// InternSubjectRef resolves a subject reference, adding unseen strings; validating it is ValidSubject's job.
// An empty relation means the subject names an object directly, and an id of WildcardMarker yields the type's wildcard.
func (d *Dictionary) InternSubjectRef(typ, id, relation string) (SubjectRef, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	t, err := d.types.intern(typ, d.limit)
	if err != nil {
		return SubjectRef{}, err
	}

	s := SubjectRef{Type: TypeID(t), Wildcard: id == WildcardMarker}

	if !s.Wildcard {
		o, err := d.objects.intern(id, d.limit)
		if err != nil {
			return SubjectRef{}, err
		}

		s.ID = ObjectID(o)
	}

	if relation != "" {
		r, err := d.relations.intern(relation, d.limit)
		if err != nil {
			return SubjectRef{}, err
		}

		s.Relation = RelationID(r)
	}

	return s, nil
}

// LookupObjectRef resolves a resource reference without adding, zero for unseen names.
func (d *Dictionary) LookupObjectRef(typ, id string) ObjectRef {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return ObjectRef{
		Type: TypeID(d.types.lookup(typ)),
		ID:   ObjectID(d.objects.lookup(id)),
	}
}

// LookupSubjectRef resolves a subject reference without adding, zero for unseen
// names, and reads WildcardMarker as InternSubjectRef does.
func (d *Dictionary) LookupSubjectRef(typ, id, relation string) SubjectRef {
	d.mu.RLock()
	defer d.mu.RUnlock()

	s := SubjectRef{
		Type:     TypeID(d.types.lookup(typ)),
		Wildcard: id == WildcardMarker,
	}

	if !s.Wildcard {
		s.ID = ObjectID(d.objects.lookup(id))
	}

	if relation != "" {
		s.Relation = RelationID(d.relations.lookup(relation))
	}

	return s
}

// FormatObjectRef renders a resource reference as "type:id".
func (d *Dictionary) FormatObjectRef(r ObjectRef) string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return d.objectRefString(r)
}

// FormatSubjectRef renders a subject as "type:id", "type:id#relation", or "type:*".
func (d *Dictionary) FormatSubjectRef(s SubjectRef) string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return d.subjectRefString(s)
}

// FormatTuple renders a tuple as "type:id#relation@subject".
func (d *Dictionary) FormatTuple(t Tuple) string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var b strings.Builder

	b.WriteString(d.objectRefString(t.Object))
	b.WriteByte('#')
	b.WriteString(nameOf(&d.relations, uint32(t.Relation)))
	b.WriteByte('@')
	b.WriteString(d.subjectRefString(t.Subject))

	return b.String()
}

// objectRefString, subjectRefString, and nameOf expect the caller to hold a read lock.
func (d *Dictionary) objectRefString(r ObjectRef) string {
	return nameOf(&d.types, uint32(r.Type)) + ":" + nameOf(&d.objects, uint32(r.ID))
}

func (d *Dictionary) subjectRefString(s SubjectRef) string {
	id := WildcardMarker
	if !s.Wildcard {
		id = nameOf(&d.objects, uint32(s.ID))
	}

	out := nameOf(&d.types, uint32(s.Type)) + ":" + id
	if s.Relation != NoRelation {
		out += "#" + nameOf(&d.relations, uint32(s.Relation))
	}

	return out
}

func nameOf(t *table, id uint32) string {
	if id == 0 {
		return unresolvedName
	}

	if name, ok := t.name(id); ok {
		return name
	}

	return danglingName + "(" + strconv.FormatUint(uint64(id), 10) + ")"
}
