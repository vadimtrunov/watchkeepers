package notebook

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// maxPendingLessons clamps the [DB.PendingLessons] limit server-side.
// The query is a daily-briefing surface backing the M8.3 lesson-digest
// section; an unbounded limit would let a misconfigured caller pull a
// crash-loop's worth of reflector-emitted lessons into the agent's
// prompt window. 200 covers a heavy reflection day with headroom and
// matches the M8.2 family's `maxOverdueIssues` / `maxFetchMessages`
// cap shape so the per-tool truncation discipline reads consistently
// across the Coordinator surface.
const maxPendingLessons = 200

// PendingLessonRow is one row returned by [DB.PendingLessons].
// Surfaces ONLY the columns the M8.3 lesson-digest needs — id,
// subject, created_at, active_after — and intentionally OMITS the
// lesson `content` body, the embedding blob, and the recall-only
// columns (`last_used_at` / `relevance_score`).
//
// PII discipline (iter-1 codex Minor): the content body is dropped
// at the QUERY layer (not just on the agent-side projection) so any
// future caller of [DB.PendingLessons] — a debug log, an `fmt`
// dump of the interface payload, a follow-up tool surface — gets
// the digest shape by default. A future operator-side CLI surface
// that genuinely needs the full body must add a separate method
// (e.g. `PendingLessonsWithContent`) and own the additional
// PII-handling discipline at THAT boundary. Mirrors M8.2.b/c/d
// lesson #10 generalisation: drop identifier echoes at the
// projection that crosses the agent boundary, do not rely on
// every downstream caller remembering to drop them.
type PendingLessonRow struct {
	// ID is the canonical UUID v7 string PK from `entry`.
	ID string

	// Subject is the optional human-readable subject (e.g. the
	// `<toolName>: <errClass>` shape composed by
	// [github.com/vadimtrunov/watchkeepers/core/pkg/runtime.ToolErrorReflector]).
	// Empty when the underlying row stored SQL NULL.
	Subject string

	// CreatedAt is the unix epoch millisecond at which the entry was
	// first recorded.
	CreatedAt int64

	// ActiveAfter is a unix epoch millisecond before which Recall must
	// not surface the entry. By construction of [DB.PendingLessons],
	// this is strictly greater than the supplied `now`.
	ActiveAfter int64
}

// PendingLessons returns the lesson entries currently in the cooling-
// off window (`active_after > now AND superseded_by IS NULL AND
// needs_review = 0`), ordered by ascending `active_after` (the most
// imminent activation first). `limit` is clamped server-side at
// [maxPendingLessons]; non-positive `limit` returns [ErrInvalidEntry]
// without touching the DB.
//
// Filter rationale:
//
//   - `category = 'lesson'` — the digest surfaces lessons specifically;
//     `pending_task` (M8.3.a Watch Orders) and other categories use
//     their own surfaces.
//   - `superseded_by IS NULL` — superseded rows are dormant; their
//     successor (which fired the supersession) carries the active
//     lesson signal.
//   - `needs_review = 0` — M5.6.a rows flagged after a tool-version
//     hot-load are hidden from Recall AND from the digest (they need a
//     human ack first; surfacing them in the digest would let an
//     agent's reply path race the operator review).
//   - `active_after > now` — the discriminant: rows still in the 24h
//     cooling-off window from [WithCoolingOff] in
//     `tool_error_reflector.go`. Auto-activation past the window is
//     implicit via Recall's `active_after <= ?` filter; the digest is
//     the operator's chance to `forget <id>` before that crossover.
//
// Result is ordered by ascending `active_after` so the most-imminent
// auto-activation appears first — the operator's attention budget
// favours the row that is about to land in agent prompts vs the row
// that has another 22 hours.
//
// Returns an empty slice (not nil) when no rows match; callers can
// `len(rows) == 0` without a nil-vs-empty check.
func (d *DB) PendingLessons(ctx context.Context, now time.Time, limit int) ([]PendingLessonRow, error) {
	if limit <= 0 {
		return nil, ErrInvalidEntry
	}
	if limit > maxPendingLessons {
		limit = maxPendingLessons
	}

	nowMillis := now.UnixMilli()

	// SELECT does NOT pull `content` — least-privilege boundary at
	// the query layer (iter-1 codex Minor). A future operator-only
	// surface adds its own SELECT, not this one.
	const sqlText = `
		SELECT id, subject, created_at, active_after
		FROM entry
		WHERE category = ?
		  AND superseded_by IS NULL
		  AND needs_review = 0
		  AND active_after > ?
		ORDER BY active_after ASC, id ASC
		LIMIT ?
	`

	rows, err := d.sql.QueryContext(ctx, sqlText, CategoryLesson, nowMillis, limit)
	if err != nil {
		return nil, fmt.Errorf("notebook: pending lessons query: %w", err)
	}
	defer rows.Close()

	out := make([]PendingLessonRow, 0, limit)
	for rows.Next() {
		var (
			row     PendingLessonRow
			subject sql.NullString
		)
		if err := rows.Scan(
			&row.ID, &subject, &row.CreatedAt, &row.ActiveAfter,
		); err != nil {
			return nil, fmt.Errorf("notebook: scan pending-lessons row: %w", err)
		}
		row.Subject = stringFromNullable(subject)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notebook: iterate pending-lessons rows: %w", err)
	}
	return out, nil
}
