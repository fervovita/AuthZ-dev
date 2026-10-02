package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Revision is a point in the store's history.
// Keep it and pass it back; do not parse or compare it.
type Revision []byte

// State is the whole store at one revision.
type State struct {
	Revision Revision
	Schema   string // the source in force; "" before any is written
	Tuples   []Tuple
}

// Delta takes a reader from one revision to a newer one.
type Delta struct {
	Revision Revision // the newer revision
	Schema   *string  // the source in force at Revision if a schema was written since, nil otherwise
	Changes  []Change // every tuple a commit since touched, as it stands at Revision
}

// Change is a tuple's state at a Delta's revision.
type Change struct {
	Tuple   Tuple
	Present bool
}

// Snapshot reads the whole store at one revision.
func (s *Store) Snapshot(ctx context.Context) (State, error) {
	var st State

	err := s.inTx(ctx, "snapshot", readTx, func(tx pgx.Tx) error {
		var err error

		if st.Revision, err = revision(ctx, tx); err != nil {
			return err
		}

		err = tx.QueryRow(ctx, `SELECT source FROM schema_versions ORDER BY version DESC LIMIT 1`).Scan(&st.Schema)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: snapshot: %w", err)
		}

		rows, err := tx.Query(ctx, `SELECT `+columns+` FROM tuples`)
		if err != nil {
			return fmt.Errorf("postgres: snapshot: %w", err)
		}

		if st.Tuples, err = pgx.CollectRows(rows, scanTuple); err != nil {
			return fmt.Errorf("postgres: snapshot: %w", err)
		}

		return nil
	})

	return st, err
}

// Changes reads what a reader at since needs to reach the newest revision.
// since comes from Snapshot or Changes.
func (s *Store) Changes(ctx context.Context, since Revision) (Delta, error) {
	var d Delta

	err := s.inTx(ctx, "changes", readTx, func(tx pgx.Tx) error {
		var err error

		if d.Revision, err = revision(ctx, tx); err != nil {
			return err
		}

		var source string

		err = tx.QueryRow(ctx, `SELECT source FROM schema_versions
			WHERE NOT pg_visible_in_snapshot(xid, $1::pg_snapshot)
			ORDER BY version DESC LIMIT 1`, string(since)).Scan(&source)

		switch {
		case err == nil:
			d.Schema = &source
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("postgres: changes: %w", err)
		}

		rows, err := tx.Query(ctx, `SELECT DISTINCT `+columns+`, t.resource_type IS NOT NULL
			FROM change_log LEFT JOIN tuples t USING (`+columns+`)
			WHERE xid >= pg_snapshot_xmin($1::pg_snapshot) AND NOT pg_visible_in_snapshot(xid, $1::pg_snapshot)`,
			string(since))
		if err != nil {
			return fmt.Errorf("postgres: changes: %w", err)
		}

		if d.Changes, err = pgx.CollectRows(rows, scanChange); err != nil {
			return fmt.Errorf("postgres: changes: %w", err)
		}

		return nil
	})

	return d, err
}

// revision returns the snapshot tx reads at, which is the revision of everything it reads.
func revision(ctx context.Context, tx pgx.Tx) (Revision, error) {
	var snapshot string
	if err := tx.QueryRow(ctx, `SELECT pg_current_snapshot()::text`).Scan(&snapshot); err != nil {
		return nil, fmt.Errorf("postgres: read revision: %w", err)
	}

	return Revision(snapshot), nil
}

func scanTuple(row pgx.CollectableRow) (Tuple, error) {
	var t Tuple
	err := row.Scan(&t.ResourceType, &t.ResourceID, &t.Relation, &t.SubjectType, &t.SubjectID, &t.SubjectRelation)

	return t, err
}

func scanChange(row pgx.CollectableRow) (Change, error) {
	var c Change
	t := &c.Tuple
	err := row.Scan(&t.ResourceType, &t.ResourceID, &t.Relation, &t.SubjectType, &t.SubjectID, &t.SubjectRelation, &c.Present)

	return c, err
}
