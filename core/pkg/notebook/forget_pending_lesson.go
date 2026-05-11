package notebook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// ErrNotPendingLesson is returned by [DB.ForgetPendingLesson] when the
// supplied id resolves to a row that is NOT a cooling-off lesson — the
// row is in a non-lesson category, OR its `active_after` has already
// passed, OR it has been superseded. The transaction is rolled back so
// neither table changes.
//
// The sentinel exists so the M8.3 `ForgetDMHandler` can refuse the
// lead's `forget <id>` with a canonical refusal text WITHOUT exposing
// the row's category / state to the lead (PII surface: a leaked id
// from a different agent could enumerate categories via probing
// behaviour). Mirrors [ErrNotFound] / [ErrInvalidEntry] classification
// discipline.
var ErrNotPendingLesson = errors.New("notebook: id is not a pending (cooling-off) lesson")

// forgetPendingLessonEventType is the `event_type` column written to
// keepers_log for the per-ForgetPendingLesson audit event emitted when
// a [Logger] has been wired in via [WithLogger]. Distinct from the
// generic [forgetEventType] because the audit surface is callers
// explicitly opting into the narrowed-discipline forget path; downstream
// log consumers can distinguish "operator forgot a row" (broad) vs
// "lead forgot a cooling-off lesson" (narrow).
const forgetPendingLessonEventType = "notebook_pending_lesson_forgotten"

// ForgetPendingLesson is the narrowed forget surface the M8.3
// `ForgetDMHandler` consumes. Unlike [DB.Forget] (which deletes any
// canonical-UUID row regardless of category or state), this method
// gates the delete on three predicates:
//
//  1. row exists (`ErrNotFound` if not);
//  2. row is `category = 'lesson'` AND `superseded_by IS NULL` AND
//     `active_after > now` (`ErrNotPendingLesson` if any predicate
//     fails — the row is the "wrong kind" for the cooling-off
//     suppress path);
//  3. id is a canonical UUID (`ErrInvalidEntry` if not — same as
//     [DB.Forget]).
//
// Closing this discipline at the DB layer (vs the consumer) means a
// leaked id from a different category (preference / observation /
// pending_task / already-active lesson) CANNOT be erased via the
// Coordinator's `forget <id>` DM path. The operator's broader
// [DB.Forget] surface (CLI / supervisor-driven) remains the way to
// delete those rows.
//
// `now` is supplied by the caller (not [time.Now]) so the handler
// chain stays clock-injectable and the cooling-off boundary is
// pinnable in tests. Mirrors the M8.2.b/c/d clock-injection
// precedent.
//
// Audit emit (when a Logger is wired): writes a
// [forgetPendingLessonEventType] row carrying agent id + entry id +
// timestamp. The pre-delete classification check ensures the audit
// emit only fires on a successful narrow-discipline delete; refusal
// paths (`ErrNotFound` / `ErrNotPendingLesson` / `ErrInvalidEntry`)
// leave the keepers_log untouched.
func (d *DB) ForgetPendingLesson(ctx context.Context, id string, now time.Time) error {
	if !uuidPattern.MatchString(id) {
		return ErrInvalidEntry
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("notebook: begin forget_pending_lesson tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Predicate check inside the transaction so a concurrent flip of
	// `superseded_by` or a passing-of-the-cooling-off boundary cannot
	// race the classify-then-delete sequence. Using a single SELECT
	// (not a SELECT-then-DELETE pair) keeps the lock window tight.
	var (
		category    string
		activeAfter int64
		superseded  sql.NullString
	)
	row := tx.QueryRowContext(
		ctx,
		`SELECT category, active_after, superseded_by FROM entry WHERE id = ?`, id,
	)
	if err := row.Scan(&category, &activeAfter, &superseded); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("notebook: forget_pending_lesson lookup: %w", err)
	}
	if category != CategoryLesson {
		return ErrNotPendingLesson
	}
	if superseded.Valid {
		return ErrNotPendingLesson
	}
	if activeAfter <= now.UnixMilli() {
		return ErrNotPendingLesson
	}

	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM entry_vec WHERE id = ?`, id,
	); err != nil {
		return fmt.Errorf("notebook: delete entry_vec (pending lesson): %w", err)
	}
	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM entry WHERE id = ?`, id,
	); err != nil {
		return fmt.Errorf("notebook: delete entry (pending lesson): %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("notebook: commit forget_pending_lesson tx: %w", err)
	}

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
			EventType: forgetPendingLessonEventType,
			Payload:   payload,
		}); err != nil {
			return fmt.Errorf("audit emit: %w", err)
		}
	}
	return nil
}
