package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/fervovita/AuthZ-dev/internal/core"
	"github.com/fervovita/AuthZ-dev/internal/schema"
	"github.com/fervovita/AuthZ-dev/internal/watch"
)

// replica is the store at one revision with its own dictionary: a Reset builds a new one, so no ID is ever reused.
// It is the engine's TupleSource and takes no lock of its own: Store holds one across each Check and each Apply.
type replica struct {
	revision watch.Revision
	names    *core.Dictionary
	engine   *core.Engine
	subjects map[core.SubjectType]bool // the subject types some relation accepts
	entries  map[key]*entry
}

// key is what the evaluator reads by: one relation on one object.
type key struct {
	object   core.ObjectRef
	relation core.RelationID
}

// entry holds a key's subjects in three sorted lists, one for each question the evaluator asks of it.
type entry struct {
	direct    []core.ObjectRef  // subjects that are an object itself, such as user:alice
	usersets  []core.SubjectRef // subjects that are a relation on an object, such as team:eng#member
	wildcards []core.TypeID     // types granted whole, such as user:*
}

// item is one tuple's state with its names interned.
type item struct {
	key     key
	subject core.SubjectRef
	present bool
}

func newReplica(revision watch.Revision, source string, tuples []watch.Tuple) (*replica, error) {
	r := &replica{revision: revision, names: core.NewDictionary(), entries: make(map[key]*entry)}

	if err := r.compile(source); err != nil {
		return nil, err
	}

	items := make([]item, 0, len(tuples))

	for _, t := range tuples {
		it, err := r.intern(t)
		if err != nil {
			return nil, err
		}

		items = append(items, it)
	}

	r.merge(items)

	return r, nil
}

func (r *replica) apply(revision watch.Revision, source *string, changes []watch.Change) error {
	if source != nil {
		if err := r.compile(*source); err != nil {
			return err
		}
	}

	items := make([]item, 0, len(changes))

	for _, c := range changes {
		if c.Present {
			it, err := r.intern(c.Tuple)
			if err != nil {
				return err
			}

			items = append(items, it)
		} else if it, ok := r.resolve(c.Tuple); ok {
			items = append(items, it)
		}
	}

	r.merge(items)
	r.revision = revision

	return nil
}

func (r *replica) compile(source string) error {
	compiled, err := schema.Compile("schema", source, r.names)
	if err != nil {
		return fmt.Errorf("memstore: %w", err)
	}

	if r.engine, err = core.NewEngine(compiled, r); err != nil {
		return fmt.Errorf("memstore: %w", err)
	}

	r.subjects = make(map[core.SubjectType]bool)

	for _, ref := range compiled.Relations() {
		for _, st := range compiled.Accepted(ref) {
			st.Wildcard = false // user:* admits every user
			r.subjects[st] = true
		}
	}

	return nil
}

// intern resolves a present tuple's names, adding the unseen.
func (r *replica) intern(t watch.Tuple) (item, error) {
	obj, err := r.names.InternObjectRef(t.ResourceType, t.ResourceID)
	if err != nil {
		return item{}, fmt.Errorf("memstore: %v: %w", t, err)
	}

	rel, err := r.names.InternRelation(t.Relation)
	if err != nil {
		return item{}, fmt.Errorf("memstore: %v: %w", t, err)
	}

	subj, err := r.names.InternSubjectRef(t.SubjectType, t.SubjectID, t.SubjectRelation)
	if err != nil {
		return item{}, fmt.Errorf("memstore: %v: %w", t, err)
	}

	return item{key{obj, rel}, subj, true}, nil
}

// resolve is intern for a removed tuple: it adds no name, since one never seen means the tuple is not held.
func (r *replica) resolve(t watch.Tuple) (item, bool) {
	obj := r.names.LookupObjectRef(t.ResourceType, t.ResourceID)
	rel := r.names.LookupRelation(t.Relation)
	subj := r.names.LookupSubjectRef(t.SubjectType, t.SubjectID, t.SubjectRelation)

	if !obj.Valid() || rel == core.NoRelation || !subj.Valid() || (t.SubjectRelation != "" && subj.Relation == core.NoRelation) {
		return item{}, false
	}

	return item{key{obj, rel}, subj, false}, true
}

// merge lands items a key at a time, so a key's lists are rebuilt once however many of its subjects changed.
func (r *replica) merge(items []item) {
	slices.SortFunc(items, func(a, b item) int {
		return cmp.Or(compareKeys(a.key, b.key), compareSubjects(a.subject, b.subject))
	})

	for len(items) > 0 {
		n := 1
		for n < len(items) && items[n].key == items[0].key {
			n++
		}

		r.mergeKey(items[0].key, items[:n])
		items = items[n:]
	}
}

// mergeKey lands items, all on k and sorted, and drops k once it holds nothing.
func (r *replica) mergeKey(k key, items []item) {
	var (
		direct    patch[core.ObjectRef]
		usersets  patch[core.SubjectRef]
		wildcards patch[core.TypeID]
	)

	// Sorted by subject, each list comes out in its own order: wildcards and objects carry no relation.
	for _, it := range items {
		s := it.subject

		switch {
		case s.Wildcard:
			wildcards.put(s.Type, it.present)
		case s.Relation != core.NoRelation:
			usersets.put(s, it.present)
		default:
			direct.put(core.ObjectRef{Type: s.Type, ID: s.ID}, it.present)
		}
	}

	e := r.entries[k]
	if e == nil {
		e = &entry{}
	}

	e.direct = mergeSorted(e.direct, direct.adds, direct.drops, compareObjects)
	e.usersets = mergeSorted(e.usersets, usersets.adds, usersets.drops, compareSubjects)
	e.wildcards = mergeSorted(e.wildcards, wildcards.adds, wildcards.drops, cmp.Compare[core.TypeID])

	if len(e.direct) == 0 && len(e.usersets) == 0 && len(e.wildcards) == 0 {
		delete(r.entries, k)

		return
	}

	r.entries[k] = e
}

// patch is what one change list does to one of a key's lists.
type patch[T any] struct {
	adds, drops []T
}

func (p *patch[T]) put(v T, present bool) {
	if present {
		p.adds = append(p.adds, v)
	} else {
		p.drops = append(p.drops, v)
	}
}

// mergeSorted returns base with adds put in and drops taken out, all three sorted by compare.
// adds may hold what base does and drops what it does not: a change says what is, not what happened.
func mergeSorted[T any](base, adds, drops []T, compare func(a, b T) int) []T {
	if len(adds) == 0 && len(drops) == 0 {
		return base
	}

	out := make([]T, 0, len(base)+len(adds))
	d := 0

	for i, j := 0, 0; i < len(base) || j < len(adds); {
		var next T

		switch {
		case j == len(adds):
			next, i = base[i], i+1
		case i == len(base):
			next, j = adds[j], j+1
		default:
			switch c := compare(base[i], adds[j]); {
			case c < 0:
				next, i = base[i], i+1
			case c > 0:
				next, j = adds[j], j+1
			default:
				next, i, j = base[i], i+1, j+1
			}
		}

		for d < len(drops) && compare(drops[d], next) < 0 {
			d++
		}

		if d < len(drops) && compare(drops[d], next) == 0 {
			continue
		}

		out = append(out, next)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func compareKeys(a, b key) int {
	return cmp.Or(compareObjects(a.object, b.object), cmp.Compare(a.relation, b.relation))
}

func compareObjects(a, b core.ObjectRef) int {
	return cmp.Or(cmp.Compare(a.Type, b.Type), cmp.Compare(a.ID, b.ID))
}

// compareSubjects orders by type -> ID -> relation: a wildcard's ID and relation are both zero.
func compareSubjects(a, b core.SubjectRef) int {
	return cmp.Or(cmp.Compare(a.Type, b.Type), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Relation, b.Relation))
}

// Lookup hands back the key's own userset list: callers copy what they keep before yield returns.
func (r *replica) Lookup(ctx context.Context, reqs []core.LookupRequest, yield func(int, core.LookupResult) bool) error {
	for i, req := range reqs {
		if err := ctx.Err(); err != nil {
			return err
		}

		var res core.LookupResult

		if e := r.entries[key{req.Object, req.Relation}]; e != nil {
			res = core.LookupResult{Direct: e.holds(req.Subject), Usersets: e.usersets}
		}

		if !yield(i, res) {
			return nil
		}
	}

	return nil
}

// holds reports whether e names s itself. A wildcard subject has ID 0, which no object here has: HasWildcard answers for it.
func (e *entry) holds(s core.SubjectRef) bool {
	if s.Relation != core.NoRelation {
		_, ok := slices.BinarySearchFunc(e.usersets, s, compareSubjects)

		return ok
	}

	_, ok := slices.BinarySearchFunc(e.direct, core.ObjectRef{Type: s.Type, ID: s.ID}, compareObjects)

	return ok
}

// Subjects builds each answer afresh: concurrent Checks share no buffer.
func (r *replica) Subjects(ctx context.Context, reqs []core.SubjectsRequest, yield func(int, []core.SubjectRef) bool) error {
	for i, req := range reqs {
		if err := ctx.Err(); err != nil {
			return err
		}

		var subjects []core.SubjectRef

		if e := r.entries[key{req.Object, req.Relation}]; e != nil {
			subjects = make([]core.SubjectRef, 0, len(e.direct)+len(e.usersets))

			for _, o := range e.direct {
				subjects = append(subjects, core.SubjectRef{Type: o.Type, ID: o.ID})
			}

			subjects = append(subjects, e.usersets...)
		}

		if !yield(i, subjects) {
			return nil
		}
	}

	return nil
}

func (r *replica) HasWildcard(ctx context.Context, reqs []core.WildcardRequest, out []bool) error {
	if len(out) < len(reqs) {
		return core.ErrShortBuffer
	}

	for i, req := range reqs {
		if err := ctx.Err(); err != nil {
			return err
		}

		e := r.entries[key{req.Object, req.Relation}]
		out[i] = e != nil && slices.Contains(e.wildcards, req.SubjectType)
	}

	return nil
}
