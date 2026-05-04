package notebook

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// forgetEventType is the `event_type` column written to keepers_log for
// the per-Forget audit event emitted by [DB.Forget] when a [Logger] has
// been wired in via [WithLogger]. Held as a const so tests pin against
// the same string the production code emits.
const forgetEventType = "notebook_entry_forgotten"

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

	// Audit emit (M2b.7). The transaction is committed — the row is gone
	// — before we touch the logger. Pre-commit failures (ErrInvalidEntry,
	// ErrNotFound, tx errors) return earlier and never reach this point.
	//
	// Payload omits PII / large fields by design (AC5): only the agent
	// id, the entry id, and the wall-clock at which the row went away.
	if d.logger != nil {
		payload, err := json.Marshal(map[string]any{
			"agent_id":     d.agentID,
			"entry_id":     id,
			"forgotten_at": time.Now().UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			return fmt.Errorf("audit marshal: %w", err)
		}
		if _, err := d.logger.LogAppend(ctx, keepclient.LogAppendRequest{
			EventType: forgetEventType,
			Payload:   payload,
		}); err != nil {
			// The row IS gone — caller can retry just the audit emit with
			// the same id (mirrors the M2b.4 partial-failure shape).
			return fmt.Errorf("audit emit: %w", err)
		}
	}
	return nil
}
