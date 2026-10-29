package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrPruned reports a revision older than the changes the store still holds. The reader starts over from Snapshot.
var ErrPruned = errors.New("postgres: revision pruned")

const (
	// markSQL notes where the log could be cut later.
	markSQL = `INSERT INTO log_marks (xid, marked_at)
		VALUES (pg_snapshot_xmin(pg_current_snapshot()), now())
		ON CONFLICT DO NOTHING`

	// cutSQL finds the newest mark at least $1 old, where the log is cut.
	cutSQL = `SELECT xid
		FROM log_marks
		WHERE marked_at <= now() - $1::interval
		ORDER BY xid DESC
		LIMIT 1`
)

// Prune drops the changes that only a reader more than keep behind still needs.
// It cuts where a call stood at least keep ago, so nothing is dropped until calls are keep apart: call it on a timer.
func (s *Store) Prune(ctx context.Context, keep time.Duration) error {
	return s.inTx(ctx, "prune", writeTx, func(tx pgx.Tx) error {
		if err := acquire(ctx, tx, "prune", []lock{pruneLock()}); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, markSQL); err != nil {
			return fmt.Errorf("postgres: prune: %w", err)
		}

		// Both find the same cut: dropping the marks before it leaves the cut itself.
		for _, table := range []string{"change_log", "log_marks"} {
			if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE xid < (`+cutSQL+`)`, keep); err != nil {
				return fmt.Errorf("postgres: prune: %w", err)
			}
		}

		return nil
	})
}
