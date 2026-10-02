package postgres

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/fervovita/AuthZ-dev/internal/core"
)

// ErrInvalid reports a schema, an update or a filter the store refuses. The request changes nothing.
var ErrInvalid = errors.New("postgres: invalid")

// Tuple names one relationship in strings: IDs interned in a process never reach the store.
type Tuple struct {
	ResourceType    string
	ResourceID      string
	Relation        string
	SubjectType     string
	SubjectID       string // "*" names every subject of SubjectType
	SubjectRelation string // "" names the subject itself
}

// String formats t as document:d1#viewer@team:eng#member.
func (t Tuple) String() string {
	s := t.ResourceType + ":" + t.ResourceID + "#" + t.Relation + "@" + t.SubjectType + ":" + t.SubjectID
	if t.SubjectRelation != "" {
		s += "#" + t.SubjectRelation
	}

	return s
}

// Op is what an Update does to its tuple.
type Op uint8

// Op values.
const (
	Touch  Op = iota + 1 // store the tuple; storing a stored one changes nothing
	Delete               // remove the tuple; removing an absent one changes nothing
)

// Update is one change a Write makes.
type Update struct {
	Op    Op
	Tuple Tuple
}

const columns = `resource_type, resource_id, relation, subject_type, subject_id, subject_relation`

// keyColumns is the primary key's order, not the table's.
const keyColumns = `resource_type, relation, resource_id, subject_type, subject_id, subject_relation`

// Each statement logs its tuple only when it changed one: a conflict or a missing row returns nothing to log.
const (
	touchSQL = `WITH changed AS (
		INSERT INTO tuples (` + columns + `) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING
		RETURNING ` + columns + `)
	INSERT INTO change_log (` + columns + `) SELECT ` + columns + ` FROM changed`

	deleteSQL = `WITH changed AS (
		DELETE FROM tuples
		WHERE resource_type = $1 AND resource_id = $2 AND relation = $3
			AND subject_type = $4 AND subject_id = $5 AND subject_relation = $6
		RETURNING ` + columns + `)
	INSERT INTO change_log (` + columns + `) SELECT ` + columns + ` FROM changed`
)

// Write applies updates in one transaction, checked against the newest schema.
func (s *Store) Write(ctx context.Context, updates []Update) error {
	return s.inTx(ctx, "write", writeTx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, schemaLockShared); err != nil {
			return fmt.Errorf("postgres: write: %w", err)
		}

		d := core.NewDictionary()

		compiled, err := latestSchema(ctx, tx, d)
		if err != nil {
			return err
		}

		if compiled == nil {
			return fmt.Errorf("%w: no schema has been written", ErrInvalid)
		}

		seen := make(map[Tuple]int, len(updates))

		for i, u := range updates {
			if err := check(compiled, d, u); err != nil {
				return fmt.Errorf("%w: updates[%d] %s: %w", ErrInvalid, i, u.Tuple, err)
			}

			if j, ok := seen[u.Tuple]; ok {
				return fmt.Errorf("%w: updates[%d] %s: repeats updates[%d]", ErrInvalid, i, u.Tuple, j)
			}

			seen[u.Tuple] = i
		}

		// Applying in one order keeps two requests over the same tuples from each holding one and waiting on the other.
		ordered := slices.Clone(updates)
		slices.SortFunc(ordered, func(a, b Update) int { return compareTuples(a.Tuple, b.Tuple) })

		for _, u := range ordered {
			var sql string

			switch u.Op {
			case Touch:
				sql = touchSQL
			case Delete:
				sql = deleteSQL
			}

			t := u.Tuple
			if _, err := tx.Exec(ctx, sql, t.ResourceType, t.ResourceID, t.Relation, t.SubjectType, t.SubjectID, t.SubjectRelation); err != nil {
				return fmt.Errorf("postgres: write: %w", err)
			}
		}

		return nil
	})
}

// check reports why compiled refuses u, looking names up in d without interning them.
func check(compiled *core.Schema, d *core.Dictionary, u Update) error {
	t := u.Tuple

	if u.Op != Touch && u.Op != Delete {
		return fmt.Errorf("unknown operation %d", u.Op)
	}

	if !core.ValidObject(t.ResourceType, t.ResourceID) || !core.ValidRelation(t.Relation) ||
		!core.ValidSubject(t.SubjectType, t.SubjectID, t.SubjectRelation) {
		return errors.New("breaks the identifier rules")
	}

	return checkFilter(compiled, d, Filter{
		ResourceType:    t.ResourceType,
		Relation:        t.Relation,
		SubjectType:     t.SubjectType,
		SubjectRelation: t.SubjectRelation,
		Wildcard:        t.SubjectID == core.WildcardMarker,
	})
}

// checkFilter reports why compiled refuses the tuples f matches, looking names up in d without interning them.
func checkFilter(compiled *core.Schema, d *core.Dictionary, f Filter) error {
	ref := core.RelationRef{Type: d.LookupType(f.ResourceType), Relation: d.LookupRelation(f.Relation)}

	rw, ok := compiled.Rewrite(ref)
	if !ok {
		return fmt.Errorf("%s#%s is not defined", f.ResourceType, f.Relation)
	}

	if rw.Op != core.OpThis {
		return fmt.Errorf("%s#%s is a permission, which stores no tuples", f.ResourceType, f.Relation)
	}

	if f.SubjectType == "" {
		return nil
	}

	st := core.SubjectType{Type: d.LookupType(f.SubjectType), Wildcard: f.Wildcard}
	if f.SubjectRelation != "" {
		st.Relation = d.LookupRelation(f.SubjectRelation)
	}

	if (f.SubjectRelation != "" && st.Relation == core.NoRelation) || !compiled.Allows(ref, st) {
		return fmt.Errorf("%s#%s does not accept %s", f.ResourceType, f.Relation, f.subjectType())
	}

	return nil
}

// compareTuples orders tuples as the primary key does.
func compareTuples(a, b Tuple) int {
	return cmp.Or(
		strings.Compare(a.ResourceType, b.ResourceType),
		strings.Compare(a.Relation, b.Relation),
		strings.Compare(a.ResourceID, b.ResourceID),
		strings.Compare(a.SubjectType, b.SubjectType),
		strings.Compare(a.SubjectID, b.SubjectID),
		strings.Compare(a.SubjectRelation, b.SubjectRelation),
	)
}
