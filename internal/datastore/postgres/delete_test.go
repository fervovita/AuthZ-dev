//go:build integration

package postgres

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/fervovita/AuthZ-dev/internal/core"
)

// filter reads document#viewer@team#member or document#viewer@user:* the way Filter.String writes it.
func filter(s string) Filter {
	resource, subject, _ := strings.Cut(s, "@")
	typ, rel, _ := strings.Cut(resource, "#")

	f := Filter{ResourceType: typ, Relation: rel}
	if subject != "" {
		subject, f.Wildcard = strings.CutSuffix(subject, ":"+core.WildcardMarker)
		f.SubjectType, f.SubjectRelation, _ = strings.Cut(subject, "#")
	}

	return f
}

func deleteMatching(t *testing.T, s *Store, f Filter) {
	t.Helper()

	if err := s.DeleteMatching(t.Context(), f); err != nil {
		t.Fatalf("DeleteMatching(%s): %v", f, err)
	}
}

// filterTuples holds a tuple of every subject type document#viewer accepts, and a few beside them.
var filterTuples = []string{
	"document:d1#viewer@user:alice",
	"document:d2#viewer@user:bob",
	"document:d1#viewer@user:*",
	"document:d1#viewer@team:eng",
	"document:d1#viewer@team:eng#member",
	"document:d1#publisher@team:eng",
	"document:d1#banned@user:alice",
	"team:eng#member@user:alice",
	"team:eng#viewer@user:alice",
}

// filterStore opens a schemaStore holding filterTuples.
func filterStore(t *testing.T) *Store {
	t.Helper()

	s := schemaStore(t)

	updates := make([]Update, 0, len(filterTuples))
	for _, u := range filterTuples {
		updates = append(updates, touch(u))
	}

	write(t, s, updates...)

	return s
}

// A subject type matches only itself: user is not user:*, and team is not team#member.
// Deleting again finds nothing, so nothing more is logged.
func TestDeleteMatchingDeletesWhatItMatches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		filter  string
		deleted []string
	}{
		{"document#viewer", []string{
			"document:d1#viewer@user:alice",
			"document:d2#viewer@user:bob",
			"document:d1#viewer@user:*",
			"document:d1#viewer@team:eng",
			"document:d1#viewer@team:eng#member",
		}},
		{"document#viewer@user", []string{"document:d1#viewer@user:alice", "document:d2#viewer@user:bob"}},
		{"document#viewer@user:*", []string{"document:d1#viewer@user:*"}},
		{"document#viewer@team", []string{"document:d1#viewer@team:eng"}},
		{"document#viewer@team#member", []string{"document:d1#viewer@team:eng#member"}},
	}

	for _, c := range cases {
		t.Run(c.filter, func(t *testing.T) {
			t.Parallel()

			s := filterStore(t)

			deleteMatching(t, s, filter(c.filter))
			deleteMatching(t, s, filter(c.filter))

			var kept []string

			for _, u := range filterTuples {
				if !slices.Contains(c.deleted, u) {
					kept = append(kept, u)
				}
			}

			assertRows(t, s, "tuples", kept...)
			assertRows(t, s, "change_log", append(slices.Clone(filterTuples), c.deleted...)...)
		})
	}
}

// Every case is refused before anything is deleted.
func TestDeleteMatchingRefusesABadFilter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		f    Filter
		want string
	}{
		{"an undefined relation", filter("document#editor"), "document#editor is not defined"},
		{"an undefined type", filter("folder#viewer"), "folder#viewer is not defined"},
		{"a permission", filter("document#view"), "document#view is a permission"},
		{"a subject type not accepted", filter("document#banned@user:*"), "document#banned does not accept user:*"},
		{"an unknown subject relation", filter("document#publisher@team#bogus"), "document#publisher does not accept team#bogus"},
		{"a malformed name", filter("document#Viewer"), "breaks the identifier rules"},
		{"a wildcard with a relation", Filter{"document", "viewer", "team", "member", true}, "breaks the identifier rules"},
		{"a subject relation alone", Filter{ResourceType: "document", Relation: "viewer", SubjectRelation: "member"}, "breaks the identifier rules"},
		{"a wildcard alone", Filter{ResourceType: "document", Relation: "viewer", Wildcard: true}, "breaks the identifier rules"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			s := filterStore(t)

			err := s.DeleteMatching(t.Context(), c.f)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("DeleteMatching = %v; want ErrInvalid saying %q", err, c.want)
			}

			assertRows(t, s, "tuples", filterTuples...)
		})
	}
}

func TestDeleteMatchingRefusesBeforeAnySchema(t *testing.T) {
	t.Parallel()

	s := openStore(t, newDatabase(t))

	if err := s.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if err := s.DeleteMatching(t.Context(), filter("document#viewer")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("DeleteMatching = %v; want ErrInvalid", err)
	}
}

// The rows are stored in the reverse of key order, and this connection makes the planner read them
// as stored, as it may choose to on a large table. Locking them in that order would cross Write's.
func TestDeleteMatchingRacingAWriteBothSucceed(t *testing.T) {
	t.Parallel()

	u, err := url.Parse(newDatabase(t))
	if err != nil {
		t.Fatalf("parse the database URL: %v", err)
	}

	q := u.Query()
	q.Set("options", "-c enable_indexscan=off -c enable_bitmapscan=off")
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20") // pgx passes + on as it is, not as a space

	s := schemaStoreOn(t, u.String())

	for i := range 20 {
		a, b := fmt.Sprintf("document:d1#viewer@user:a%d", i), fmt.Sprintf("document:d1#viewer@user:b%d", i)

		write(t, s, touch(b))
		write(t, s, touch(a))

		errs := make([]error, 2)

		var wg sync.WaitGroup

		wg.Go(func() { errs[0] = s.Write(t.Context(), []Update{remove(a), remove(b)}) })
		wg.Go(func() { errs[1] = s.DeleteMatching(t.Context(), filter("document#viewer@user")) })
		wg.Wait()

		if err := errors.Join(errs...); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
}
