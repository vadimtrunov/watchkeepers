package notebook

import (
	"context"
	"fmt"
)

// MarkNeedsReview flips the `needs_review` flag on the row identified by
// `entryID` to 1, hiding it from [DB.Recall] until [DB.ClearNeedsReview]
// reverses the flip. Returns [ErrInvalidEntry] for an empty `entryID` and
// [ErrNotFound] when the id is well-formed but no row in `entry` matches —
// the per-agent SQLite file boundary means an id belonging to another
// agent's notebook also surfaces as [ErrNotFound], pinning the cross-agent
// isolation contract at the API level.
//
// Re-marking an already-flagged entry is a no-op (the UPDATE sees a row
// whose `needs_review` is already 1; `RowsAffected` reports 1 because
// SQLite's UPDATE counts row matches, not actual mutations). Callers may
// invoke this method idempotently.
func (d *DB) MarkNeedsReview(ctx context.Context, entryID string) error {
	return d.setNeedsReview(ctx, entryID, 1)
}

// ClearNeedsReview flips the `needs_review` flag on the row identified by
// `entryID` to 0, restoring its visibility to [DB.Recall]. Returns
// [ErrInvalidEntry] for an empty `entryID` and [ErrNotFound] when the id is
// well-formed but no row matches. Idempotent: calling it on an already-clear
// row does not error.
func (d *DB) ClearNeedsReview(ctx context.Context, entryID string) error {
	return d.setNeedsReview(ctx, entryID, 0)
}

// setNeedsReview is the shared implementation behind [DB.MarkNeedsReview]
// and [DB.ClearNeedsReview]. It runs a single `UPDATE ... WHERE id = ?` and
// maps a zero `RowsAffected` to [ErrNotFound]. The flip is intentionally
// non-transactional: nothing else in the package writes to `needs_review`
// concurrently, and `entry_vec` does not carry the flag — so the UPDATE
// stands alone without a sibling write to coordinate.
func (d *DB) setNeedsReview(ctx context.Context, entryID string, value int) error {
	if entryID == "" {
		return ErrInvalidEntry
	}

	res, err := d.sql.ExecContext(
		ctx,
		`UPDATE entry SET needs_review = ? WHERE id = ?`,
		value, entryID,
	)
	if err != nil {
		return fmt.Errorf("notebook: update needs_review: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("notebook: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
