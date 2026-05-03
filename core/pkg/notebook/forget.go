package notebook

import (
	"context"
	"fmt"
)

// Forget deletes the entry with the given id from both `entry` and
// `entry_vec` in a single transaction. The id must be a canonical UUID;
// non-canonical input returns [ErrInvalidEntry] without touching the DB.
//
// Returns [ErrNotFound] when the id is well-formed but no row in `entry`
// matches; the transaction is rolled back so neither table changes. The
// `entry_vec` DELETE happens first because the FK on
// `entry.superseded_by REFERENCES entry(id)` is irrelevant to entry_vec —
// either order would work, but doing entry_vec first matches the inverse of
// the [DB.Remember] order which keeps mental models simple.
func (d *DB) Forget(ctx context.Context, id string) error {
	if !uuidPattern.MatchString(id) {
		return ErrInvalidEntry
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("notebook: begin forget tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entry_vec WHERE id = ?`, id,
	); err != nil {
		return fmt.Errorf("notebook: delete entry_vec: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`DELETE FROM entry WHERE id = ?`, id,
	)
	if err != nil {
		return fmt.Errorf("notebook: delete entry: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("notebook: rows affected: %w", err)
	}
	if n == 0 {
		// Roll back so the (already-executed) entry_vec delete is also
		// reverted — keeps the sync contract intact when the caller
		// supplied a stray id.
		return ErrNotFound
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("notebook: commit forget tx: %w", err)
	}
	return nil
}
