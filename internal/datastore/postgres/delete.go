package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fervovita/AuthZ-dev/internal/core"
)

// Filter names the tuples of one relation, or those of one subject type it accepts: user, user:* or team#member.
type Filter struct {
	ResourceType    string
	Relation        string
	SubjectType     string // "" matches every subject
	SubjectRelation string // "" names the subject itself
	Wildcard        bool   // true names every subject of SubjectType, as SubjectID "*" does in a Tuple
}

// String formats f as document#viewer or document#viewer@team#member.
func (f Filter) String() string {
	s := f.ResourceType + "#" + f.Relation
	if f.SubjectType != "" {
		s += "@" + f.subjectType()
	}

	return s
}

// DeleteMatching removes every tuple f matches in one transaction, checked against the newest schema.
// Writes to f's relation wait until it ends.
func (s *Store) DeleteMatching(ctx context.Context, f Filter) error {
	return s.inTx(ctx, "delete", writeTx, func(tx pgx.Tx) error {
		if err := acquire(ctx, tx, "delete", []lock{relationLock(f.ResourceType, f.Relation, true)}); err != nil {
			return err
		}

		d := core.NewDictionary()

		compiled, err := latestSchema(ctx, tx, d)
		if err != nil {
			return err
		}

		if compiled == nil {
			return fmt.Errorf("%w: no schema has been written", ErrInvalid)
		}

		if !f.valid() {
			return fmt.Errorf("%w: filter %s: breaks the identifier rules", ErrInvalid, f)
		}

		if err := checkFilter(compiled, d, f); err != nil {
			return fmt.Errorf("%w: filter %s: %w", ErrInvalid, f, err)
		}

		cond, args := f.where()

		sql := `WITH changed AS (DELETE FROM tuples WHERE ` + cond + ` RETURNING ` + columns + `)
			INSERT INTO change_log (` + columns + `) SELECT ` + columns + ` FROM changed`

		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			return fmt.Errorf("postgres: delete: %w", err)
		}

		return nil
	})
}

// subjectType formats f's subject type as a schema would: user, user:* or team#member.
func (f Filter) subjectType() string {
	switch {
	case f.Wildcard:
		return f.SubjectType + ":" + core.WildcardMarker
	case f.SubjectRelation != "":
		return f.SubjectType + "#" + f.SubjectRelation
	}

	return f.SubjectType
}

// valid reports whether f keeps the identifier rules and its subject fields name one subject type.
func (f Filter) valid() bool {
	if !core.ValidType(f.ResourceType) || !core.ValidRelation(f.Relation) {
		return false
	}

	switch {
	case f.SubjectType == "":
		return f.SubjectRelation == "" && !f.Wildcard
	case f.SubjectRelation == "":
		return core.ValidType(f.SubjectType)
	}

	return !f.Wildcard && core.ValidType(f.SubjectType) && core.ValidRelation(f.SubjectRelation)
}

// where returns the condition f puts on tuples, and the arguments for its parameters.
func (f Filter) where() (string, []any) {
	cond := `resource_type = $1 AND relation = $2`
	args := []any{f.ResourceType, f.Relation}

	if f.SubjectType != "" {
		cond += ` AND subject_type = $3 AND subject_relation = $4`
		args = append(args, f.SubjectType, f.SubjectRelation)

		if f.Wildcard {
			cond += ` AND subject_id = $5`
		} else {
			cond += ` AND subject_id <> $5`
		}

		args = append(args, core.WildcardMarker)
	}

	return cond, args
}
