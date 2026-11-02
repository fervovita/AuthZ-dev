package memstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fervovita/AuthZ-dev/internal/core"
	"github.com/fervovita/AuthZ-dev/internal/schema"
	"github.com/fervovita/AuthZ-dev/internal/watch"
)

const testSchema = `
	definition user {}

	definition team {
		relation member: user | team#member
	}

	definition document {
		relation viewer: user | user:* | team | team#member
		relation banned: user
		permission view = viewer - banned
	}
`

// tupleOf reads "type:id#relation@subject", where the subject is type:id, type:id#relation or type:*.
func tupleOf(tb testing.TB, s string) watch.Tuple {
	tb.Helper()

	left, subject, ok1 := strings.Cut(s, "@")
	resource, relation, ok2 := strings.Cut(left, "#")
	resourceType, resourceID, ok3 := strings.Cut(resource, ":")
	subjectType, rest, ok4 := strings.Cut(subject, ":")
	subjectID, subjectRelation, _ := strings.Cut(rest, "#")

	if !ok1 || !ok2 || !ok3 || !ok4 {
		tb.Fatalf("%q is not type:id#relation@subject", s)
	}

	return watch.Tuple{
		ResourceType: resourceType, ResourceID: resourceID, Relation: relation,
		SubjectType: subjectType, SubjectID: subjectID, SubjectRelation: subjectRelation,
	}
}

// checkOf reads a check in the form tupleOf does.
func checkOf(tb testing.TB, s string) CheckRequest {
	tb.Helper()

	t := tupleOf(tb, s)

	return CheckRequest{
		ResourceType: t.ResourceType, ResourceID: t.ResourceID, Permission: t.Relation,
		SubjectType: t.SubjectType, SubjectID: t.SubjectID, SubjectRelation: t.SubjectRelation,
	}
}

func tuplesOf(tb testing.TB, lines ...string) []watch.Tuple {
	tb.Helper()

	tuples := make([]watch.Tuple, len(lines))
	for i, line := range lines {
		tuples[i] = tupleOf(tb, line)
	}

	return tuples
}

func reset(tb testing.TB, s *Store, source string, lines ...string) {
	tb.Helper()

	if err := s.Reset(watch.Revision("r0"), source, tuplesOf(tb, lines...)); err != nil {
		tb.Fatalf("Reset: %v", err)
	}
}

func apply(t *testing.T, s *Store, revision string, source *string, changes ...watch.Change) {
	t.Helper()

	if err := s.Apply(watch.Revision(revision), source, changes); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func touch(t *testing.T, line string) watch.Change {
	t.Helper()

	return watch.Change{Tuple: tupleOf(t, line), Present: true}
}

func remove(t *testing.T, line string) watch.Change {
	t.Helper()

	return watch.Change{Tuple: tupleOf(t, line)}
}

// assertChecks checks each "check: want" pair against s.
func assertChecks(t *testing.T, s *Store, want map[string]bool) {
	t.Helper()

	for check, allowed := range want {
		got, err := s.Check(t.Context(), checkOf(t, check))
		if err != nil {
			t.Errorf("Check(%s): %v", check, err)

			continue
		}

		if got != allowed {
			t.Errorf("Check(%s) = %v; want %v", check, got, allowed)
		}
	}
}

func TestCheckReadsTheReplica(t *testing.T) {
	t.Parallel()

	var s Store

	reset(t, &s, testSchema,
		"document:d1#viewer@user:alice",
		"document:d1#viewer@team:eng#member",
		"team:eng#member@user:bob",
		"document:d1#viewer@user:carol",
		"document:d1#banned@user:carol")

	assertChecks(t, &s, map[string]bool{
		"document:d1#view@user:alice": true,
		"document:d1#view@user:bob":   true,
		"document:d1#view@user:carol": false,
		"document:d1#view@user:dave":  false,
		"document:d2#view@user:alice": false,
	})
}

// Apply moves the tuples, the schema and the revision together.
func TestApplyMovesTheReplica(t *testing.T) {
	t.Parallel()

	var s Store

	reset(t, &s, testSchema, "document:d1#viewer@user:alice", "document:d1#viewer@user:carol", "document:d1#banned@user:carol")

	if got := string(s.Revision()); got != "r0" {
		t.Errorf("Revision = %q; want r0", got)
	}

	apply(t, &s, "r1", nil, remove(t, "document:d1#viewer@user:alice"), touch(t, "document:d1#viewer@user:dave"))

	if got := string(s.Revision()); got != "r1" {
		t.Errorf("Revision = %q; want r1", got)
	}

	assertChecks(t, &s, map[string]bool{
		"document:d1#view@user:alice": false,
		"document:d1#view@user:dave":  true,
		"document:d1#view@user:carol": false,
	})

	unbanned := strings.Replace(testSchema, "permission view = viewer - banned", "permission view = viewer", 1)
	apply(t, &s, "r2", &unbanned)

	assertChecks(t, &s, map[string]bool{"document:d1#view@user:carol": true})
}

func TestStoreIsNotReadyBeforeReset(t *testing.T) {
	t.Parallel()

	var s Store

	if _, err := s.Check(t.Context(), checkOf(t, "document:d1#view@user:alice")); !errors.Is(err, ErrNotReady) {
		t.Errorf("Check = %v; want ErrNotReady", err)
	}

	if err := s.Apply(watch.Revision("r1"), nil, nil); !errors.Is(err, ErrNotReady) {
		t.Errorf("Apply = %v; want ErrNotReady", err)
	}

	if rev := s.Revision(); rev != nil {
		t.Errorf("Revision = %q; want nil", rev)
	}
}

// A store that failed to take a change or a reset may hold part of it, so it answers nothing until the next Reset.
func TestStoreIsNotReadyAfterAFailure(t *testing.T) {
	t.Parallel()

	broken := "definition {"

	for name, fail := range map[string]func(*Store) error{
		"Apply": func(s *Store) error { return s.Apply(watch.Revision("r1"), &broken, nil) },
		"Reset": func(s *Store) error { return s.Reset(watch.Revision("r1"), broken, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var s Store

			reset(t, &s, testSchema, "document:d1#viewer@user:alice")

			if err := fail(&s); err == nil {
				t.Fatalf("%s took a schema that does not compile", name)
			}

			if _, err := s.Check(t.Context(), checkOf(t, "document:d1#view@user:alice")); !errors.Is(err, ErrNotReady) {
				t.Errorf("Check = %v; want ErrNotReady", err)
			}

			reset(t, &s, testSchema, "document:d1#viewer@user:alice")
			assertChecks(t, &s, map[string]bool{"document:d1#view@user:alice": true})
		})
	}
}

// A Reset interns into a new dictionary, so names that left the store leave the dictionary too.
func TestResetStartsAFreshDictionary(t *testing.T) {
	t.Parallel()

	var s Store

	reset(t, &s, testSchema, "document:d1#viewer@user:alice")
	reset(t, &s, testSchema, "document:d1#viewer@user:bob")

	if id := s.current.names.LookupObject("alice"); id != 0 {
		t.Errorf("alice is still interned, as %d", id)
	}

	assertChecks(t, &s, map[string]bool{
		"document:d1#view@user:alice": false,
		"document:d1#view@user:bob":   true,
	})
}

// Removing a tuple the store never held adds no names and takes nothing else with it.
func TestRemovingAnUnheldTupleChangesNothing(t *testing.T) {
	t.Parallel()

	var s Store

	reset(t, &s, testSchema, "document:d1#viewer@user:alice")

	// The second names alice through a relation never seen, which must not read as alice herself.
	apply(t, &s, "r1", nil, remove(t, "document:ghost#viewer@user:ghost"), remove(t, "document:d1#viewer@user:alice#ghost"))

	if id := s.current.names.LookupObject("ghost"); id != 0 {
		t.Errorf("removing an unheld tuple interned ghost as %d", id)
	}

	assertChecks(t, &s, map[string]bool{"document:d1#view@user:alice": true})
}

// A request names identifiers freely: one the store has not seen is answered as a subject or resource
// no tuple names, without growing the dictionary.
func TestCheckAsksAboutUnseenIdentifiers(t *testing.T) {
	t.Parallel()

	var s Store

	reset(t, &s, testSchema, "document:public#viewer@user:*", "document:d1#viewer@team:eng#member")

	assertChecks(t, &s, map[string]bool{
		"document:public#view@user:newcomer":         true, // the wildcard covers a user no tuple names
		"document:nowhere#view@user:newcomer":        false,
		"document:d1#viewer@team:newteam#member":     false,
		"document:public#viewer@team:newteam#member": false, // a wildcard grants users, not usersets
	})

	for _, name := range []string{"newcomer", "nowhere", "newteam"} {
		if id := s.current.names.LookupObject(name); id != 0 {
			t.Errorf("a check interned %s as %d", name, id)
		}
	}
}

func TestCheckRefusesWhatTheSchemaCannotAnswer(t *testing.T) {
	t.Parallel()

	var s Store

	reset(t, &s, testSchema, "document:d1#viewer@user:alice", "document:d1#viewer@team:eng")

	for check, want := range map[string]error{
		"document:d1#nope@user:alice":       core.ErrRelationUndefined,
		"folder:d1#view@user:alice":         core.ErrRelationUndefined,
		"document:d1#view@robot:r2":         core.ErrInvalidRequest,
		"document:d1#view@team:eng#nope":    core.ErrInvalidRequest, // not the object team:eng
		"document:d1#view@team:eng#viewer":  core.ErrInvalidRequest, // viewer is document's, not team's
		"document:d1#view@document:d2":      core.ErrInvalidRequest, // no relation takes a document
		"document:d1#view@user:*":           core.ErrInvalidRequest,
		"document:d1#view@user:not allowed": core.ErrInvalidRequest,
		"document:not allowed#view@user:a1": core.ErrInvalidRequest,
		"document:d1#Not_Allowed@user:a1":   core.ErrInvalidRequest,
	} {
		if _, err := s.Check(t.Context(), checkOf(t, check)); !errors.Is(err, want) {
			t.Errorf("Check(%s) = %v; want %v", check, err, want)
		}
	}
}

// The schema in force decides what is refused, not the names the store has seen:
// a type it dropped is refused as one it never had, so a store that saw the old schema answers as one that never did.
func TestCheckRefusesByTheSchemaInForce(t *testing.T) {
	t.Parallel()

	robots := strings.Replace(testSchema, "relation banned: user", "relation banned: user | robot", 1) + "definition robot {}"

	var s Store

	reset(t, &s, robots)
	assertChecks(t, &s, map[string]bool{"document:d1#view@robot:r2": false})

	plain := testSchema
	apply(t, &s, "r1", &plain)

	if _, err := s.Check(t.Context(), checkOf(t, "document:d1#view@robot:r2")); !errors.Is(err, core.ErrInvalidRequest) {
		t.Errorf("Check = %v; want ErrInvalidRequest", err)
	}
}

// pausing is the replica with its second Lookup held until release: in a check of view = viewer - banned,
// after viewer is read and before banned is.
type pausing struct {
	*replica

	calls   atomic.Int32
	reached chan struct{}
	release chan struct{}
}

func (p *pausing) Lookup(ctx context.Context, reqs []core.LookupRequest, yield func(int, core.LookupResult) bool) error {
	if p.calls.Add(1) == 2 {
		close(p.reached)
		<-p.release
	}

	return p.replica.Lookup(ctx, reqs, yield)
}

// One request removing both of alice's tuples leaves her denied before and after it.
// A check that read viewer before it and banned after would let her in, so no change lands while a check is reading.
func TestCheckSeesOneRevision(t *testing.T) {
	t.Parallel()

	var s Store

	reset(t, &s, testSchema, "document:d1#viewer@user:alice", "document:d1#banned@user:alice")

	p := &pausing{replica: s.current, reached: make(chan struct{}), release: make(chan struct{})}

	compiled, err := schema.Compile("schema", testSchema, s.current.names)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if s.current.engine, err = core.NewEngine(compiled, p); err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	release := sync.OnceFunc(func() { close(p.release) })
	t.Cleanup(release)

	type answer struct {
		allowed bool
		err     error
	}

	answered := make(chan answer, 1)
	req := checkOf(t, "document:d1#view@user:alice")

	go func() {
		allowed, err := s.Check(t.Context(), req)
		answered <- answer{allowed, err}
	}()

	select {
	case <-p.reached:
	case a := <-answered:
		t.Fatalf("Check = %v, %v without a second Lookup to hold", a.allowed, a.err)
	}

	if s.mu.TryLock() {
		s.mu.Unlock()
		t.Fatal("a change could land between two reads of one check")
	}

	release()

	if a := <-answered; a.err != nil || a.allowed {
		t.Fatalf("Check = %v, %v; want denied", a.allowed, a.err)
	}

	apply(t, &s, "r1", nil, remove(t, "document:d1#viewer@user:alice"), remove(t, "document:d1#banned@user:alice"))
	assertChecks(t, &s, map[string]bool{"document:d1#view@user:alice": false})
}

// Requests that add or remove both of alice's tuples leave her denied either way, so a check racing them
// is only ever allowed if it read one tuple on each side of a change.
func TestChecksRacingChangesSeeWholeRequests(t *testing.T) {
	t.Parallel()

	var s Store

	reset(t, &s, testSchema)

	both := []watch.Change{touch(t, "document:d1#viewer@user:alice"), touch(t, "document:d1#banned@user:alice")}
	neither := []watch.Change{remove(t, "document:d1#viewer@user:alice"), remove(t, "document:d1#banned@user:alice")}
	req := checkOf(t, "document:d1#view@user:alice")

	var checkers sync.WaitGroup

	stop := make(chan struct{})

	for range 4 {
		checkers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}

				if allowed, err := s.Check(t.Context(), req); err != nil || allowed {
					t.Errorf("Check = %v, %v; want denied", allowed, err)

					return
				}
			}
		})
	}

	for round := range 2000 {
		changes := both
		if round%2 == 1 {
			changes = neither
		}

		if err := s.Apply(watch.Revision("r"), nil, changes); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}

	close(stop)
	checkers.Wait()
}
