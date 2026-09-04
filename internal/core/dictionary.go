package core

import (
	"errors"
	"fmt"
	"math"
	"sync"
)

// TypeID identifies an interned object type.
type TypeID uint32

// RelationID identifies an interned relation or permission name.
type RelationID uint32

// ObjectID identifies an interned object identifier.
type ObjectID uint32

// NoRelation marks a subject that names an object directly rather than through a userset.
// Zero is reserved in every ID space, so an uninitialised value is never a valid identifier.
const NoRelation RelationID = 0

// ErrIDSpaceExhausted reports that an ID space would exceed uint32.
//
// Dropping the tuple denies: safe for a positive tuple, but for an exclusion reachable one it removes a prohibition.
// This design makes that a readiness failure, not a per-tuple skip.
var ErrIDSpaceExhausted = errors.New("core: id space exhausted")

// defaultLimit is the highest ID that fits in uint32.
const defaultLimit = uint64(math.MaxUint32)

type table struct {
	space string
	ids   map[string]uint32
	names []string
}

func newTable(space string) table {
	return table{
		space: space,
		ids:   make(map[string]uint32),
		names: []string{""}, // index 0 is the reserved sentinel
	}
}

func (t *table) intern(s string, limit uint64) (uint32, error) {
	// "" is index 0's own value, so interning it would give Valid a second id meaning "empty".
	// No Valid* accepts it, so central cannot have sent it.
	if s == "" {
		return 0, nil
	}

	if id, ok := t.ids[s]; ok {
		return id, nil
	}

	// limit is policy that tests lower; the constant is the hard uint32 ceiling.
	next := uint64(len(t.names))
	if next > limit || next > uint64(math.MaxUint32) {
		return 0, fmt.Errorf("%w: %s", ErrIDSpaceExhausted, t.space)
	}

	id := uint32(next)
	t.names = append(t.names, s)
	t.ids[s] = id

	return id, nil
}

// lookup returns 0 for a string the table has not seen.
func (t *table) lookup(s string) uint32 {
	return t.ids[s]
}

func (t *table) name(id uint32) (string, bool) {
	if id == 0 || uint64(id) >= uint64(len(t.names)) {
		return "", false
	}

	return t.names[id], true
}

// Dictionary interns the strings that appear in tuples.
//
// Intern* is the write path, for strings arriving from replication.
// Lookup* is the read path, and returns zero for a name never interned, which matches no tuple.
type Dictionary struct {
	mu        sync.RWMutex
	limit     uint64
	types     table
	relations table
	objects   table
}

// NewDictionary returns an empty Dictionary.
func NewDictionary() *Dictionary {
	return newDictionary(defaultLimit)
}

// newDictionary lowers the ceiling so the overflow guard is testable.
func newDictionary(limit uint64) *Dictionary {
	return &Dictionary{
		limit:     limit,
		types:     newTable("types"),
		relations: newTable("relations"),
		objects:   newTable("objects"),
	}
}

// InternType adds an object type name if it is not already present.
func (d *Dictionary) InternType(name string) (TypeID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	id, err := d.types.intern(name, d.limit)

	return TypeID(id), err
}

// InternRelation adds a relation or permission name if it is not already present.
func (d *Dictionary) InternRelation(name string) (RelationID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	id, err := d.relations.intern(name, d.limit)

	return RelationID(id), err
}

// InternObject adds an object identifier if it is not already present.
func (d *Dictionary) InternObject(name string) (ObjectID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	id, err := d.objects.intern(name, d.limit)

	return ObjectID(id), err
}

// LookupType resolves an object type name without adding it.
func (d *Dictionary) LookupType(name string) TypeID {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return TypeID(d.types.lookup(name))
}

// LookupRelation resolves a relation or permission name without adding it.
func (d *Dictionary) LookupRelation(name string) RelationID {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return RelationID(d.relations.lookup(name))
}

// LookupObject resolves an object identifier without adding it.
func (d *Dictionary) LookupObject(name string) ObjectID {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return ObjectID(d.objects.lookup(name))
}

// TypeName resolves an interned type back to its string.
func (d *Dictionary) TypeName(id TypeID) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return d.types.name(uint32(id))
}

// RelationName resolves an interned relation back to its string.
func (d *Dictionary) RelationName(id RelationID) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return d.relations.name(uint32(id))
}

// ObjectName resolves an interned object identifier back to its string.
func (d *Dictionary) ObjectName(id ObjectID) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return d.objects.name(uint32(id))
}
