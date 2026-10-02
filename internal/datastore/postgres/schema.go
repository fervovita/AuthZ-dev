package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/fervovita/AuthZ-dev/internal/core"
	"github.com/fervovita/AuthZ-dev/internal/schema"
)

// ErrTuplesRemain reports a schema that drops what stored tuples still use. The request changes nothing.
var ErrTuplesRemain = errors.New("postgres: tuples remain")

// schemaLabel names the source in compile errors, since a schema arrives as text.
const schemaLabel = "schema"

// Write holds the schema lock shared and WriteSchema holds it exclusively, so a schema change sees
// every tuple written under the schema it replaces, and no tuple is written under a replaced one.
const (
	schemaLockShared    = `SELECT pg_advisory_xact_lock_shared(hashtextextended('datastore.postgres.schema', 0))`
	schemaLockExclusive = `SELECT pg_advisory_xact_lock(hashtextextended('datastore.postgres.schema', 0))`
)

// WriteSchema stores source as the newest schema.
// It refuses to drop a relation, or a subject type a relation accepts, while tuples still use it.
func (s *Store) WriteSchema(ctx context.Context, source string) error {
	d := core.NewDictionary()

	next, err := schema.Compile(schemaLabel, source, d)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	return s.inTx(ctx, "write schema", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, schemaLockExclusive); err != nil {
			return fmt.Errorf("postgres: write schema: %w", err)
		}

		prev, err := latestSchema(ctx, tx, d)
		if err != nil {
			return err
		}

		if prev != nil {
			var used []string

			for _, f := range dropped(prev, next, d) {
				cond, args := f.where()

				var exists bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tuples WHERE `+cond+`)`, args...).Scan(&exists); err != nil {
					return fmt.Errorf("postgres: write schema: %w", err)
				}

				if exists {
					used = append(used, f.String())
				}
			}

			if len(used) > 0 {
				return fmt.Errorf("%w under what the schema drops; delete them first: %s",
					ErrTuplesRemain, strings.Join(used, ", "))
			}
		}

		if _, err := tx.Exec(ctx, `INSERT INTO schema_versions (source) VALUES ($1)`, source); err != nil {
			return fmt.Errorf("postgres: write schema: %w", err)
		}

		return nil
	})
}

// dropped returns a filter for each thing next drops that prev let tuples use: a relation that no longer
// stores tuples, or a subject type a relation no longer accepts.
func dropped(prev, next *core.Schema, d *core.Dictionary) []Filter {
	var out []Filter

	for _, ref := range prev.Relations() {
		typ, _ := d.TypeName(ref.Type)
		rel, _ := d.RelationName(ref.Relation)

		if rw, ok := next.Rewrite(ref); !ok || rw.Op != core.OpThis {
			out = append(out, Filter{ResourceType: typ, Relation: rel})

			continue
		}

		for _, st := range prev.Accepted(ref) {
			if next.Allows(ref, st) {
				continue
			}

			f := Filter{ResourceType: typ, Relation: rel, Wildcard: st.Wildcard}
			f.SubjectType, _ = d.TypeName(st.Type)

			if st.Relation != core.NoRelation {
				f.SubjectRelation, _ = d.RelationName(st.Relation)
			}

			out = append(out, f)
		}
	}

	return out
}

// latestSchema compiles the newest schema tx sees, naming its IDs in d. It returns nil if none has been written.
func latestSchema(ctx context.Context, tx pgx.Tx, d *core.Dictionary) (*core.Schema, error) {
	var (
		version int64
		source  string
	)

	err := tx.QueryRow(ctx, `SELECT version, source FROM schema_versions ORDER BY version DESC LIMIT 1`).Scan(&version, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("postgres: read schema: %w", err)
	}

	compiled, err := schema.Compile(schemaLabel, source, d)
	if err != nil {
		// Stored by a binary whose rules this one no longer shares: not the writer's fault, so not ErrInvalid.
		return nil, fmt.Errorf("postgres: schema version %d no longer compiles: %w", version, err)
	}

	return compiled, nil
}
