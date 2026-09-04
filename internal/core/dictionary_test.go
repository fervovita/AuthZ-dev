package core

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestDictionaryRoundTrip(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	typ, err := d.InternType("document")
	if err != nil {
		t.Fatalf("InternType: %v", err)
	}

	rel, err := d.InternRelation("viewer")
	if err != nil {
		t.Fatalf("InternRelation: %v", err)
	}

	obj, err := d.InternObject("d1")
	if err != nil {
		t.Fatalf("InternObject: %v", err)
	}

	if name, ok := d.TypeName(typ); !ok || name != "document" {
		t.Errorf("TypeName(%d) = %q, %v; want \"document\", true", typ, name, ok)
	}

	if name, ok := d.RelationName(rel); !ok || name != "viewer" {
		t.Errorf("RelationName(%d) = %q, %v; want \"viewer\", true", rel, name, ok)
	}

	if name, ok := d.ObjectName(obj); !ok || name != "d1" {
		t.Errorf("ObjectName(%d) = %q, %v; want \"d1\", true", obj, name, ok)
	}
}

func TestDictionaryInterns(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	first, err := d.InternObject("alice")
	if err != nil {
		t.Fatalf("InternObject: %v", err)
	}

	second, err := d.InternObject("alice")
	if err != nil {
		t.Fatalf("InternObject: %v", err)
	}

	if first != second {
		t.Errorf("interning %q twice gave %d and %d; want the same id", "alice", first, second)
	}
}

func TestDictionarySpacesAreIndependent(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	typ, _ := d.InternType("group")
	rel, _ := d.InternRelation("group")

	if name, _ := d.TypeName(typ); name != "group" {
		t.Errorf("TypeName = %q; want \"group\"", name)
	}

	if name, _ := d.RelationName(rel); name != "group" {
		t.Errorf("RelationName = %q; want \"group\"", name)
	}

	// type id must not resolve in the object space.
	if _, ok := d.ObjectName(ObjectID(typ)); ok {
		t.Errorf("ObjectName(%d) resolved, but nothing was interned in the object space", typ)
	}
}

func TestDictionaryZeroIsReserved(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	if id, _ := d.InternType("document"); id == 0 {
		t.Error("InternType returned 0; zero must stay reserved")
	}

	if _, ok := d.TypeName(0); ok {
		t.Error("TypeName(0) resolved; zero must stay reserved")
	}

	if NoRelation != 0 {
		t.Errorf("NoRelation = %d; want 0", NoRelation)
	}
}

// "" is what index 0 holds; a second id meaning "empty" would let tuples with empty components pass Valid.
func TestDictionaryEmptyStringStaysSentinel(t *testing.T) {
	t.Parallel()

	spaces := []struct {
		name   string
		intern func(*Dictionary, string) (uint32, error)
		lookup func(*Dictionary, string) uint32
	}{
		{
			"types",
			func(d *Dictionary, s string) (uint32, error) { id, err := d.InternType(s); return uint32(id), err },
			func(d *Dictionary, s string) uint32 { return uint32(d.LookupType(s)) },
		},
		{
			"relations",
			func(d *Dictionary, s string) (uint32, error) { id, err := d.InternRelation(s); return uint32(id), err },
			func(d *Dictionary, s string) uint32 { return uint32(d.LookupRelation(s)) },
		},
		{
			"objects",
			func(d *Dictionary, s string) (uint32, error) { id, err := d.InternObject(s); return uint32(id), err },
			func(d *Dictionary, s string) uint32 { return uint32(d.LookupObject(s)) },
		},
	}

	for _, space := range spaces {
		t.Run(space.name, func(t *testing.T) {
			t.Parallel()

			// One slot, so a consumed slot is visible.
			d := newDictionary(1)

			id, err := space.intern(d, "")
			if err != nil {
				t.Fatalf("interning %q: %v; it resolves to the sentinel, it does not fail", "", err)
			}

			if id != 0 {
				t.Errorf("intern(%q) = %d; want 0, the reserved sentinel", "", id)
			}

			if got := space.lookup(d, ""); got != 0 {
				t.Errorf("lookup(%q) = %d; want 0", "", got)
			}

			if _, err := space.intern(d, "real"); err != nil {
				t.Errorf("the empty string consumed a slot: %v", err)
			}
		})
	}
}

func TestDictionaryUnknownID(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	if _, ok := d.ObjectName(9999); ok {
		t.Error("ObjectName(9999) resolved on an empty dictionary")
	}
}

func TestIDSpaceExhausted(t *testing.T) {
	t.Parallel()

	d := newDictionary(2) // room for exactly two entries

	if _, err := d.InternObject("a"); err != nil {
		t.Fatalf("first intern: %v", err)
	}

	if _, err := d.InternObject("b"); err != nil {
		t.Fatalf("second intern: %v", err)
	}

	_, err := d.InternObject("c")
	if !errors.Is(err, ErrIDSpaceExhausted) {
		t.Fatalf("third intern: err = %v; want ErrIDSpaceExhausted", err)
	}

	// A full dictionary still resolves what it already holds.
	if _, err := d.InternObject("a"); err != nil {
		t.Errorf("re-interning an existing entry after the limit: %v", err)
	}
}

// The ceiling applies to every ID space, and the error says which one filled.
func TestIDSpaceExhaustedAppliesToEverySpace(t *testing.T) {
	t.Parallel()

	spaces := []struct {
		name   string
		intern func(*Dictionary, string) error
	}{
		{"types", func(d *Dictionary, s string) error { _, err := d.InternType(s); return err }},
		{"relations", func(d *Dictionary, s string) error { _, err := d.InternRelation(s); return err }},
		{"objects", func(d *Dictionary, s string) error { _, err := d.InternObject(s); return err }},
	}

	for _, space := range spaces {
		t.Run(space.name, func(t *testing.T) {
			t.Parallel()

			d := newDictionary(1) // room for exactly one entry

			if err := space.intern(d, "a"); err != nil {
				t.Fatalf("first intern: %v", err)
			}

			err := space.intern(d, "b")
			if !errors.Is(err, ErrIDSpaceExhausted) {
				t.Fatalf("second intern: err = %v; want ErrIDSpaceExhausted", err)
			}

			if !strings.Contains(err.Error(), space.name) {
				t.Errorf("err = %q; want it to name the %q space", err, space.name)
			}
		})
	}
}

func TestDictionaryLookupResolvesInterned(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	typ, _ := d.InternType("document")
	rel, _ := d.InternRelation("viewer")
	obj, _ := d.InternObject("d1")

	if got := d.LookupType("document"); got != typ {
		t.Errorf("LookupType = %d; want %d", got, typ)
	}

	if got := d.LookupRelation("viewer"); got != rel {
		t.Errorf("LookupRelation = %d; want %d", got, rel)
	}

	if got := d.LookupObject("d1"); got != obj {
		t.Errorf("LookupObject = %d; want %d", got, obj)
	}
}

// An unknown name resolves to the reserved zero, which matches no tuple.
func TestDictionaryLookupUnknownIsZero(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	if got := d.LookupType("nope"); got != 0 {
		t.Errorf("LookupType = %d; want 0", got)
	}

	if got := d.LookupRelation("nope"); got != 0 {
		t.Errorf("LookupRelation = %d; want 0", got)
	}

	if got := d.LookupObject("nope"); got != 0 {
		t.Errorf("LookupObject = %d; want 0", got)
	}
}

func TestDictionaryLookupDoesNotGrow(t *testing.T) {
	t.Parallel()

	d := newDictionary(2) // Room for exactly two entries

	if _, err := d.InternObject("real"); err != nil {
		t.Fatalf("InternObject: %v", err)
	}

	for i := range 1000 {
		if got := d.LookupObject("attacker-" + strconv.Itoa(i)); got != 0 {
			t.Fatalf("LookupObject(unknown) = %d; want 0", got)
		}
	}

	if _, err := d.InternObject("also-real"); err != nil {
		t.Fatalf("lookups consumed the interning budget: %v", err)
	}
}

func TestDictionaryConcurrentAccess(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	const writers, readers, iterations = 4, 4, 200

	prefixes := make([]string, writers)
	for i := range prefixes {
		prefixes[i] = "w" + strconv.Itoa(i)
	}

	var wg sync.WaitGroup

	// Writers must touch every space.
	for _, prefix := range prefixes {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range iterations {
				key := prefix + "-" + strconv.Itoa(i)

				if _, err := d.InternObject(key); err != nil {
					t.Errorf("InternObject: %v", err)
					return
				}

				if _, err := d.InternType(key); err != nil {
					t.Errorf("InternType: %v", err)
					return
				}

				if _, err := d.InternRelation(key); err != nil {
					t.Errorf("InternRelation: %v", err)
					return
				}
			}
		}()
	}

	// Readers must exercise Lookup* as well as *Name.
	for range readers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range iterations {
				key := "w0-" + strconv.Itoa(i)

				d.ObjectName(ObjectID(i))
				d.LookupObject(key)
				d.LookupType(key)
				d.LookupRelation(key)
			}
		}()
	}

	wg.Wait()
}
