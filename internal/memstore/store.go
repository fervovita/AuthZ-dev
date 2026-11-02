package memstore

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/fervovita/AuthZ-dev/internal/core"
	"github.com/fervovita/AuthZ-dev/internal/watch"
)

// ErrNotReady reports a store with nothing to answer from.
var ErrNotReady = errors.New("memstore: not ready")

// Store is the central store at one revision, held in memory.
// The zero value is ready for Reset. One goroutine feeds it with Reset and Apply while any number call Check.
type Store struct {
	// mu is held for reading across a whole Check, so a change never lands between two of its reads.
	mu      sync.RWMutex
	current *replica // nil while not ready
}

// CheckRequest asks, by name, whether a subject holds a relation or permission on a resource.
type CheckRequest struct {
	ResourceType    string
	ResourceID      string
	Permission      string // a relation or permission of ResourceType
	SubjectType     string
	SubjectID       string
	SubjectRelation string // "" names the subject itself
}

// Reset replaces everything s holds with the store at revision: the schema then in force and every tuple stored.
// s drops what it held first, so a Check meanwhile answers ErrNotReady and the old and new replicas are never live at once.
func (s *Store) Reset(revision watch.Revision, schema string, tuples []watch.Tuple) error {
	s.mu.Lock()
	s.current = nil
	s.mu.Unlock()

	r, err := newReplica(revision, schema, tuples)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.current = r
	s.mu.Unlock()

	return nil
}

// Apply moves s to revision.
// changes holds every tuple touched since the revision s holds, each once and as it stands at revision.
// schema, unless nil, is the source in force there.
// A failed Apply leaves s not ready, since part of it may not have landed.
func (s *Store) Apply(revision watch.Revision, schema *string, changes []watch.Change) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.current == nil {
		return ErrNotReady
	}

	if err := s.current.apply(revision, schema, changes); err != nil {
		s.current = nil

		return err
	}

	return nil
}

// Revision returns the revision s holds, nil while it is not ready.
func (s *Store) Revision() watch.Revision {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.current == nil {
		return nil
	}

	return s.current.revision
}

// Check answers req from the one revision s holds when it starts.
// An identifier s has never seen is a subject or resource no tuple names, so only a wildcard grants it.
// A subject no relation accepts is an error, not a denial.
func (s *Store) Check(ctx context.Context, req CheckRequest) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	r := s.current
	if r == nil {
		return false, ErrNotReady
	}

	cr, err := r.request(req)
	if err != nil {
		return false, err
	}

	res, err := r.engine.Check(ctx, cr)

	return res.Allowed, err
}

// request resolves req's names without interning them, so a request never grows the dictionary.
func (r *replica) request(req CheckRequest) (core.CheckRequest, error) {
	if !core.ValidObject(req.ResourceType, req.ResourceID) || !core.ValidRelation(req.Permission) ||
		!core.ValidSubject(req.SubjectType, req.SubjectID, req.SubjectRelation) {
		return core.CheckRequest{}, fmt.Errorf("%w: a name breaks the identifier rules", core.ErrInvalidRequest)
	}

	obj := r.names.LookupObjectRef(req.ResourceType, req.ResourceID)
	rel := r.names.LookupRelation(req.Permission)

	if obj.Type == 0 || rel == core.NoRelation {
		return core.CheckRequest{}, fmt.Errorf("%w: %s#%s", core.ErrRelationUndefined, req.ResourceType, req.Permission)
	}

	subj := r.names.LookupSubjectRef(req.SubjectType, req.SubjectID, req.SubjectRelation)

	// An unseen relation would otherwise read as none, and the userset as the object itself.
	unseen := req.SubjectRelation != "" && subj.Relation == core.NoRelation

	// Asked of the schema, not the dictionary: the dictionary keeps the names of what the schema dropped.
	if unseen || !r.subjects[core.SubjectType{Type: subj.Type, Relation: subj.Relation}] {
		return core.CheckRequest{}, fmt.Errorf("%w: no relation accepts subject type %q with relation %q",
			core.ErrInvalidRequest, req.SubjectType, req.SubjectRelation)
	}

	if obj.ID == 0 {
		obj.ID = core.UnknownObject
	}

	if !subj.Wildcard && subj.ID == 0 {
		subj.ID = core.UnknownObject
	}

	return core.CheckRequest{Object: obj, Relation: rel, Subject: subj}, nil
}
