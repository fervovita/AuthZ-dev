package postgres

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
)

// maxTupleLocks is the most tuples a request locks one by one. A larger request locks their relations whole,
// so no request fills the lock table every session shares.
const maxTupleLocks = 64

// lock names one advisory lock a request takes and the mode it takes it in.
type lock struct {
	name      string
	exclusive bool
}

// acquireSQL takes the locks in the order it is given them.
const acquireSQL = `SELECT CASE WHEN l.exclusive
		THEN pg_advisory_xact_lock(hashtextextended(l.name, 0))
		ELSE pg_advisory_xact_lock_shared(hashtextextended(l.name, 0)) END
	FROM unnest($1::text[], $2::bool[]) WITH ORDINALITY AS l(name, exclusive, i)
	ORDER BY l.i`

// acquire takes locks in tx, holding them until it ends.
func acquire(ctx context.Context, tx pgx.Tx, op string, locks []lock) error {
	locks = ordered(locks)

	names := make([]string, len(locks))
	exclusive := make([]bool, len(locks))

	for i, l := range locks {
		names[i], exclusive[i] = l.name, l.exclusive
	}

	if _, err := tx.Exec(ctx, acquireSQL, names, exclusive); err != nil {
		return fmt.Errorf("postgres: %s: %w", op, err)
	}

	return nil
}

// ordered returns each lock once, in the strongest mode asked for it, sorted by name.
// Every request takes its locks in that one order, so none waits on another in a cycle.
func ordered(locks []lock) []lock {
	exclusive := make(map[string]bool, len(locks))
	for _, l := range locks {
		exclusive[l.name] = exclusive[l.name] || l.exclusive
	}

	out := make([]lock, 0, len(exclusive))
	for _, name := range slices.Sorted(maps.Keys(exclusive)) {
		out = append(out, lock{name, exclusive[name]})
	}

	return out
}

// writeLocks returns the locks Write takes for updates.
func writeLocks(updates []Update) []lock {
	whole := len(updates) > maxTupleLocks

	locks := []lock{schemaLock(false)}
	for _, u := range updates {
		locks = append(locks, relationLock(u.Tuple.ResourceType, u.Tuple.Relation, whole))
		if !whole {
			locks = append(locks, tupleLock(u.Tuple))
		}
	}

	return locks
}

func schemaLock(exclusive bool) lock {
	return lock{"datastore.postgres.schema", exclusive}
}

func relationLock(typ, rel string, exclusive bool) lock {
	return lock{"datastore.postgres.relation:" + typ + "#" + rel, exclusive}
}

// tupleLock names t whether or not it is stored, which a row lock cannot.
func tupleLock(t Tuple) lock {
	return lock{"datastore.postgres.tuple:" + t.String(), true}
}
