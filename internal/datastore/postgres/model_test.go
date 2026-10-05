//go:build integration

package postgres

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/fervovita/AuthZ-dev/internal/core"
	"github.com/fervovita/AuthZ-dev/internal/schema"
)

const (
	modelSeeds = 4   // the runs, each on a store of its own with its own sequence of requests
	modelSteps = 400 // the requests one run sends, one after another
	modelLarge = 70  // the tuples a large request writes at once
)

// One request at a time, the store does what a model of it kept in memory does.
// TestContract watches what no interleaving may break; this checks what each request does.
func TestModel(t *testing.T) {
	t.Parallel()

	if modelLarge <= maxTupleLocks {
		t.Fatalf("a large request writes %d tuples, which this store still locks one by one; raise modelLarge", modelLarge)
	}

	runModel(t, func(t *testing.T) *Store {
		t.Helper()

		s := openStore(t, newDatabase(t))

		if err := s.Migrate(t.Context()); err != nil {
			t.Fatalf("Migrate: %v", err)
		}

		return s
	})
}

// runModel is the comparison itself, apart from how a store is opened: open returns a fresh one for each seed.
func runModel(t *testing.T, open func(*testing.T) *Store) {
	for seed := range modelSeeds {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()

			//nolint:gosec // G404: a failing run has to be reproducible from its seed
			rnd := rand.New(rand.NewPCG(uint64(seed), 0))

			// The run starts before any schema is written: requests are refused until one is,
			// and the reader hears of the first as a change.
			s := open(t)
			m := newModel()
			f := follow(t, s)

			var writes, large, deletes, schemas outcomes

			for step := range modelSteps {
				at := fmt.Sprintf("seed %d, step %d", seed, step)

				// Of twenty requests: nine writes, one large write, five filter deletes, five schema changes.
				switch n := rnd.IntN(20); {
				case n < 9:
					updates := randomUpdates(rnd)

					agree(t, at, "Write("+describe(updates)+")", s.Write(t.Context(), updates), writes.note(m.write(updates)))

				case n < 10:
					updates := randomLargeUpdates(rnd)

					agree(t, at, "Write("+describe(updates)+")", s.Write(t.Context(), updates), large.note(m.write(updates)))

				case n < 15:
					by := randomFilter(rnd)

					agree(t, at, fmt.Sprintf("DeleteMatching(%#v)", by), s.DeleteMatching(t.Context(), by), deletes.note(m.delete(by)))

				default:
					source := randomSchema(rnd)

					// 50% of the time, clear what the schema would strand first, so that it goes through.
					if rnd.IntN(2) == 0 {
						for _, by := range m.stranded(source) {
							agree(t, at, fmt.Sprintf("DeleteMatching(%#v)", by), s.DeleteMatching(t.Context(), by), deletes.note(m.delete(by)))
						}
					}

					agree(t, at, fmt.Sprintf("WriteSchema(%q)", source), s.WriteSchema(t.Context(), source), schemas.note(m.writeSchema(source)))
				}

				// A reader catches up after a few requests, not after each: a delta then spans several.
				if rnd.IntN(3) == 0 {
					m.checkChanges(t, at, f)
				}

				if rnd.IntN(10) == 0 {
					m.checkSnapshot(t, at, s)
				}
			}

			at := fmt.Sprintf("seed %d, the end", seed)

			m.checkChanges(t, at, f)
			m.checkSnapshot(t, at, s)

			t.Logf("seed %d: writes %s; large writes %s; deletes %s; schemas %s", seed, &writes, &large, &deletes, &schemas)

			if writes.taken == 0 || writes.invalid == 0 || large.taken == 0 || large.invalid == 0 ||
				deletes.taken == 0 || deletes.invalid == 0 ||
				schemas.taken == 0 || schemas.invalid == 0 || schemas.remain == 0 {
				t.Errorf("seed %d: one kind of answer never came up, so the run compared little", seed)
			}
		})
	}
}

// agree fails unless the store answered a request as the model did: with nil, ErrInvalid or ErrTuplesRemain.
func agree(t *testing.T, at, request string, got, want error) {
	t.Helper()

	if (want == nil && got == nil) || (want != nil && errors.Is(got, want)) {
		return
	}

	t.Fatalf("%s: %s: the store answered %v; the model answers %v", at, request, got, want)
}

// describe formats updates for a failure: each as its operation and its tuple.
func describe(updates []Update) string {
	out := make([]string, 0, len(updates))

	for _, u := range updates {
		op := "op " + strconv.Itoa(int(u.Op))

		switch u.Op {
		case Touch:
			op = "touch"
		case Delete:
			op = "delete"
		}

		out = append(out, op+" "+u.Tuple.String())
	}

	return strings.Join(out, ", ")
}

// model is the store as its contract describes it, kept in memory.
// Whether a schema takes a tuple is asked of core, not of the store's own checks.
type model struct {
	source   string
	compiled *core.Schema // nil before any schema is written
	names    *core.Dictionary
	tuples   map[Tuple]bool

	// What has changed since a reader last caught up.
	changed       map[Tuple]bool
	schemaChanged bool
}

func newModel() *model {
	return &model{tuples: make(map[Tuple]bool), changed: make(map[Tuple]bool)}
}

// write is Write: every update must be one the schema takes, and no tuple may come twice.
func (m *model) write(updates []Update) error {
	if m.compiled == nil {
		return ErrInvalid
	}

	seen := make(map[Tuple]bool)

	for _, u := range updates {
		if !m.takes(u) || seen[u.Tuple] {
			return ErrInvalid
		}

		seen[u.Tuple] = true
	}

	for _, u := range updates {
		m.set(u.Tuple, u.Op == Touch)
	}

	return nil
}

func (m *model) takes(u Update) bool {
	t := u.Tuple

	return (u.Op == Touch || u.Op == Delete) &&
		core.ValidObject(t.ResourceType, t.ResourceID) && core.ValidRelation(t.Relation) &&
		core.ValidSubject(t.SubjectType, t.SubjectID, t.SubjectRelation) &&
		accepts(m.compiled, m.names, t)
}

// set stores or removes u, and notes the change only if it is one: a reader hears of nothing else.
func (m *model) set(u Tuple, present bool) {
	if m.tuples[u] == present {
		return
	}

	if present {
		m.tuples[u] = true
	} else {
		delete(m.tuples, u)
	}

	m.changed[u] = true
}

// delete is DeleteMatching: the schema must name what by matches.
func (m *model) delete(by Filter) error {
	if m.compiled == nil || !m.knows(by) {
		return ErrInvalid
	}

	for u := range m.tuples {
		if matches(by, u) {
			m.set(u, false)
		}
	}

	return nil
}

// knows reports whether by names a relation that stores tuples and, if it names a subject type, one the
// relation takes: the type a tuple of that shape would have.
func (m *model) knows(by Filter) bool {
	if !core.ValidType(by.ResourceType) || !core.ValidRelation(by.Relation) {
		return false
	}

	ref := core.RelationRef{Type: m.names.LookupType(by.ResourceType), Relation: m.names.LookupRelation(by.Relation)}
	if !slices.Contains(m.compiled.Relations(), ref) {
		return false
	}

	if by.SubjectType == "" {
		return by.SubjectRelation == "" && !by.Wildcard
	}

	id := "x"
	if by.Wildcard {
		id = core.WildcardMarker
	}

	return core.ValidSubject(by.SubjectType, id, by.SubjectRelation) &&
		accepts(m.compiled, m.names, Tuple{by.ResourceType, "x", by.Relation, by.SubjectType, id, by.SubjectRelation})
}

// matches reports whether by covers u. A subject type covers its direct subjects or its wildcard, never both.
func matches(by Filter, u Tuple) bool {
	if u.ResourceType != by.ResourceType || u.Relation != by.Relation {
		return false
	}

	return by.SubjectType == "" || (u.SubjectType == by.SubjectType && u.SubjectRelation == by.SubjectRelation &&
		(u.SubjectID == core.WildcardMarker) == by.Wildcard)
}

// writeSchema is WriteSchema: source must compile, and must take every tuple that is stored.
func (m *model) writeSchema(source string) error {
	names := core.NewDictionary()

	compiled, err := schema.Compile("schema", source, names)
	if err != nil {
		return ErrInvalid
	}

	for u := range m.tuples {
		if !accepts(compiled, names, u) {
			return ErrTuplesRemain
		}
	}

	m.source, m.compiled, m.names, m.schemaChanged = source, compiled, names, true

	return nil
}

// stranded returns, in a fixed order, a filter for each kind of stored tuple that source would not take.
func (m *model) stranded(source string) []Filter {
	names := core.NewDictionary()

	compiled, err := schema.Compile("schema", source, names)
	if err != nil {
		return nil
	}

	var out []Filter

	for u := range m.tuples {
		by := Filter{u.ResourceType, u.Relation, u.SubjectType, u.SubjectRelation, u.SubjectID == core.WildcardMarker}
		if !accepts(compiled, names, u) && !slices.Contains(out, by) {
			out = append(out, by)
		}
	}

	slices.SortFunc(out, func(a, b Filter) int { return strings.Compare(a.String(), b.String()) })

	return out
}

// held returns the tuples m holds as sorted strings, the form a follower's held returns.
func (m *model) held() []string {
	out := make([]string, 0, len(m.tuples))
	for u := range m.tuples {
		out = append(out, u.String())
	}

	slices.Sort(out)

	return out
}

// checkChanges has f catch up, and fails unless Changes told it of each tuple that changed since it last
// did, once and as it now stands, and of the schema only if one was written.
func (m *model) checkChanges(t *testing.T, at string, f *follower) {
	t.Helper()

	d, err := f.s.Changes(t.Context(), f.rev)
	if err != nil {
		t.Fatalf("%s: Changes: %v", at, err)
	}

	want := make([]string, 0, len(m.changed))

	for u := range m.changed {
		state := "-"
		if m.tuples[u] {
			state = "+"
		}

		want = append(want, u.String()+" "+state)
	}

	slices.Sort(want)

	if got := changes(d); !slices.Equal(got, want) {
		t.Fatalf("%s: Changes = %q; the model expects %q", at, got, want)
	}

	wantSchema := "none"
	if m.schemaChanged {
		wantSchema = strconv.Quote(m.source)
	}

	if got := schemaIn(d); got != wantSchema {
		t.Fatalf("%s: Changes schema = %s; the model expects %s", at, got, wantSchema)
	}

	f.advance(d)
	clear(m.changed)
	m.schemaChanged = false

	if got, want := f.held(), m.held(); !slices.Equal(got, want) || f.schema != m.source {
		t.Fatalf("%s: the reader holds %q under schema %q; the model holds %q under %q", at, got, f.schema, want, m.source)
	}
}

// checkSnapshot fails unless Snapshot returns the tuples and the schema the model holds.
func (m *model) checkSnapshot(t *testing.T, at string, s *Store) {
	t.Helper()

	st, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("%s: Snapshot: %v", at, err)
	}

	if got, want := newFollower(s, st).held(), m.held(); !slices.Equal(got, want) || st.Schema != m.source {
		t.Fatalf("%s: Snapshot holds %q under schema %q; the model holds %q under %q", at, got, st.Schema, want, m.source)
	}
}

// modelShapes are the tuples a request names, with R for a resource ID and S for a subject ID.
// Some schema of the family takes each, and most take only some of them.
var modelShapes = []string{
	"document:R#viewer@user:S",
	"document:R#viewer@user:*",
	"document:R#viewer@team:S#member",
	"document:R#viewer@team:S",
	"document:R#banned@user:S",
	"document:R#banned@team:S#member",
	"team:R#member@user:S",
	"team:R#member@team:S#member",
	"team:R#viewer@user:S",
}

// modelBadTuples are tuples no schema of the family takes.
var modelBadTuples = []string{
	"document:a#view@user:a",         // a permission
	"document:a#editor@user:a",       // an undefined relation
	"folder:a#viewer@user:a",         // an undefined type
	"document:a#viewer@robot:a",      // an unknown subject type
	"document:a#viewer@team:a#bogus", // an unknown subject relation
	"document:a#banned@user:*",       // a wildcard where none is taken
	"document:a b#viewer@user:a",     // a malformed identifier
}

// modelFilters name what some schema of the family stores.
var modelFilters = []string{
	"document#viewer",
	"document#viewer@user",
	"document#viewer@user:*",
	"document#viewer@team#member",
	"document#viewer@team",
	"document#banned",
	"document#banned@user",
	"document#banned@team#member",
	"team#member",
	"team#member@user",
	"team#member@team#member",
	"team#viewer",
	"team#viewer@user",
}

// modelBadFilters are filters every schema of the family refuses, by what they name or by their shape.
var modelBadFilters = []Filter{
	filter("document#view"),
	filter("document#editor"),
	filter("folder#viewer"),
	filter("document#viewer@robot"),
	filter("document#viewer@team#bogus"),
	filter("document#Viewer"),
	{"document", "viewer", "team", "member", true},
	{ResourceType: "document", Relation: "viewer", SubjectRelation: "member"},
	{ResourceType: "document", Relation: "viewer", Wildcard: true},
}

// modelSchema is one schema of the family the requests move between. bans is 0 for no banned, 1 and 2 for
// a relation that takes users and then team members too, 3 for a permission of that name.
func modelSchema(wildcard, usersets, teams, nested bool, bans int) string {
	viewer, member := "user", "user"

	if wildcard {
		viewer += " | user:*"
	}

	if usersets {
		viewer += " | team#member"
	}

	if teams {
		viewer += " | team"
	}

	if nested {
		member += " | team#member"
	}

	banned, view := "", "viewer"

	switch bans {
	case 1:
		banned, view = "relation banned: user", "viewer - banned"
	case 2:
		banned, view = "relation banned: user | team#member", "viewer - banned"
	case 3:
		banned = "permission banned = viewer"
	}

	return `
	definition user {}

	definition team {
		relation member: ` + member + `
		relation viewer: user
	}

	definition document {
		relation viewer: ` + viewer + `
		` + banned + `
		permission view = ` + view + `
	}
`
}

// randomSchema returns a schema of the family, or now and then a source that does not compile or an empty one.
func randomSchema(rnd *rand.Rand) string {
	switch rnd.IntN(16) {
	case 0:
		return "definition"
	case 1:
		return ""
	}

	return modelSchema(rnd.IntN(2) == 0, rnd.IntN(2) == 0, rnd.IntN(2) == 0, rnd.IntN(2) == 0, rnd.IntN(4))
}

// randomUpdates returns up to four updates, and now and then one more that makes the request one to refuse:
// a bad tuple, a tuple given twice or an operation that does not exist.
func randomUpdates(rnd *rand.Rand) []Update {
	var updates []Update

	for range rnd.IntN(5) {
		updates = append(updates, Update{randomOp(rnd), randomTuple(rnd)})
	}

	switch rnd.IntN(12) {
	case 0:
		updates = append(updates, Update{randomOp(rnd), tuple(modelBadTuples[rnd.IntN(len(modelBadTuples))])})
	case 1:
		if len(updates) > 0 {
			updates = append(updates, Update{randomOp(rnd), updates[0].Tuple})
		}
	case 2:
		updates = append(updates, Update{Op(9), randomTuple(rnd)})
	}

	return updates
}

// randomLargeUpdates touches or deletes modelLarge tuples of one shape at once:
// more than a store may be willing to lock one by one.
func randomLargeUpdates(rnd *rand.Rand) []Update {
	op, shape := randomOp(rnd), modelShapes[rnd.IntN(len(modelShapes))]

	updates := make([]Update, modelLarge)
	for i := range updates {
		updates[i] = Update{op, tuple(strings.NewReplacer("R", "n"+strconv.Itoa(i), "S", "a").Replace(shape))}
	}

	return updates
}

func randomTuple(rnd *rand.Rand) Tuple {
	ids := []string{"a", "b"}
	shape := modelShapes[rnd.IntN(len(modelShapes))]

	return tuple(strings.NewReplacer("R", ids[rnd.IntN(len(ids))], "S", ids[rnd.IntN(len(ids))]).Replace(shape))
}

// randomFilter returns a filter some schema of the family takes, or now and then one none does.
func randomFilter(rnd *rand.Rand) Filter {
	if rnd.IntN(6) == 0 {
		return modelBadFilters[rnd.IntN(len(modelBadFilters))]
	}

	return filter(modelFilters[rnd.IntN(len(modelFilters))])
}

// outcomes counts how the model answered one kind of request, so a run shows it compared each answer.
type outcomes struct {
	taken, invalid, remain int
}

// note counts the answer want and returns it.
func (o *outcomes) note(want error) error {
	switch {
	case want == nil:
		o.taken++
	case errors.Is(want, ErrTuplesRemain):
		o.remain++
	default:
		o.invalid++
	}

	return want
}

func (o *outcomes) String() string {
	if o.remain == 0 {
		return fmt.Sprintf("%d taken and %d invalid", o.taken, o.invalid)
	}

	return fmt.Sprintf("%d taken, %d invalid and %d with tuples remaining", o.taken, o.invalid, o.remain)
}
