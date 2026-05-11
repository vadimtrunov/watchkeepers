package coordinator

// list_pending_lessons — Coordinator read tool (M8.3).
//
// Resolution order (handler closure body):
//
//  1. ctx.Err() pre-check (no notebook read on cancel).
//  2. Limit arg → optional int (accepts JSON-decoded float64) with
//     [defaultListPendingLessonsLimit] when absent; range pre-validation
//     1 ≤ N ≤ [maxListPendingLessonsLimit]. Refusal text NEVER echoes
//     raw value.
//  3. Call [PendingLessonLister.PendingLessons] with the per-fire
//     clock + clamped limit.
//  4. Project each [notebook.PendingLessonRow] onto a flat map shape
//     dropping [notebook.PendingLessonRow.Content] (per docblock PII
//     discipline; lessons may carry verbatim tool error messages).
//
// Audit discipline: handler returns a [agentruntime.ToolResult] only.
// No keeperslog.Append from this file (asserted via source-grep AC).
//
// PII discipline: success Output drops the lesson [Content] field and
// emits only [ID] + [Subject] + [ActiveAfter] (RFC3339 UTC) +
// `cooling_off_hours_left` (computed against the per-fire clock). The
// agent uses this digest to compose the daily briefing's "Pending
// lessons (24h cooling-off)" section; if a future use case needs the
// full body, prefer a separate operator-only CLI surface over
// widening this tool's output.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// ListPendingLessonsName is the manifest tool name the Coordinator
// dispatcher registers this handler under. Mirrors the toolset entry
// in `deploy/migrations/028_coordinator_manifest_v5_seed.sql`.
const ListPendingLessonsName = "list_pending_lessons"

// listPendingRefusalPrefix is the leading namespace for every
// [agentruntime.ToolResult.Error] string this handler surfaces.
const listPendingRefusalPrefix = "coordinator: " + ListPendingLessonsName + ": "

// list_pending_lessons argument keys.
const (
	// ToolArgLimit caps the number of rows the handler returns. Optional;
	// defaults to [defaultListPendingLessonsLimit] when absent. Accepts
	// JSON-decoded float64 so long as the value is a non-negative
	// whole number; range 1 ≤ N ≤ [maxListPendingLessonsLimit].
	ToolArgLimit = "limit"
)

// Caps for list_pending_lessons.
const (
	// defaultListPendingLessonsLimit is the row count returned when
	// the caller omits `limit`. 50 covers a typical day's reflection
	// volume while keeping the briefing-section bullet count
	// manageable for a human reader. Mirrors the notebook layer's
	// `maxPendingLessons` headroom (200) so a default of 50 leaves
	// the agent room to explicitly request more.
	defaultListPendingLessonsLimit = 50

	// maxListPendingLessonsLimit is the upper bound on `limit`.
	// Aligned with [notebook.maxPendingLessons] (clamped server-side
	// inside the DB layer) so the handler-side cap is the
	// authoritative refusal boundary — surfacing the limit before the
	// DB clamp lets the agent know it hit the wall, vs the DB
	// silently truncating.
	maxListPendingLessonsLimit = 200
)

// PendingLessonLister is the single-method interface
// [NewListPendingLessonsHandler] consumes for the notebook read.
// Mirrors `notebook.DB.PendingLessons`'s signature exactly so
// production code passes a `*notebook.DB` through verbatim; tests
// inject a hand-rolled fake without touching the DB. The interface
// lives at the consumer (this package) per the project's
// "interfaces belong to the consumer" convention.
type PendingLessonLister interface {
	PendingLessons(ctx context.Context, now time.Time, limit int) ([]notebook.PendingLessonRow, error)
}

// defaultListPendingLessonsNow is the production clock the public
// factory binds. Tests reach for the unexported
// [newListPendingLessonsHandlerWithClock] to substitute a fixed
// `time.Time`.
var defaultListPendingLessonsNow = time.Now

// NewListPendingLessonsHandler constructs the [agentruntime.ToolHandler]
// the Coordinator dispatcher registers under [ListPendingLessonsName].
// Panics on a nil `lister` per the M*.c.* / M8.2 "panic on nil deps"
// discipline.
//
// Args contract (read from [agentruntime.ToolCall.Arguments]):
//
//   - `limit` (number, optional): integer in
//     [1, [maxListPendingLessonsLimit]]. Defaults to
//     [defaultListPendingLessonsLimit] when absent.
//
// Refusal contract — returned via [agentruntime.ToolResult.Error]
// (NOT a Go error so the agent can re-plan):
//
//   - non-number / out-of-range / non-integer `limit`.
//
// Refusal text NEVER echoes a raw arg value.
//
// Output (success) — keys on the returned [agentruntime.ToolResult.Output]:
//
//   - `lessons`        (array of object): one entry per pending lesson.
//     Per-entry keys: `id` (UUID v7), `subject` (short
//     `<toolName>: <errClass>` line composed by the M5.6.b reflector),
//     `active_after` (RFC3339 UTC clock-stamp at which auto-injection
//     unlocks), `cooling_off_hours_left` (int — hours remaining until
//     `active_after` against the per-fire clock; floor-rounded).
//     The lesson [notebook.PendingLessonRow.Content] body is
//     INTENTIONALLY OMITTED — lessons may carry tool error messages
//     with raw URLs, ticket numbers, or stack traces; the agent uses
//     the digest as a "what's pending?" surface, not a full incident
//     review.
//   - `total_returned` (int): `len(lessons)`.
//   - `limit_applied`  (int): the clamped limit the handler passed to
//     the notebook layer, echoed so the agent can audit whether it
//     hit the cap.
func NewListPendingLessonsHandler(lister PendingLessonLister) agentruntime.ToolHandler {
	return newListPendingLessonsHandlerWithClock(lister, defaultListPendingLessonsNow)
}

// newListPendingLessonsHandlerWithClock is the test-internal factory
// that lets tests inject a fixed clock without mutating package
// state. Same nil-lister panic discipline; clock MUST also be
// non-nil.
func newListPendingLessonsHandlerWithClock(
	lister PendingLessonLister,
	clock func() time.Time,
) agentruntime.ToolHandler {
	if lister == nil {
		panic("coordinator: NewListPendingLessonsHandler: lister must not be nil")
	}
	if clock == nil {
		panic("coordinator: NewListPendingLessonsHandler: clock must not be nil")
	}
	return func(ctx context.Context, call agentruntime.ToolCall) (agentruntime.ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return agentruntime.ToolResult{}, err
		}

		limit, refusal := readListPendingLessonsLimitArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		now := clock().UTC()
		rows, err := lister.PendingLessons(ctx, now, limit)
		if err != nil {
			return agentruntime.ToolResult{}, fmt.Errorf("coordinator: list_pending_lessons: %w", err)
		}

		return agentruntime.ToolResult{
			Output: map[string]any{
				"lessons":        projectPendingLessons(rows, now),
				"total_returned": len(rows),
				"limit_applied":  limit,
			},
		}, nil
	}
}

// readListPendingLessonsLimitArg projects the optional `limit` arg
// into a typed int with [defaultListPendingLessonsLimit] when absent.
// Refusal text NEVER echoes the raw value.
func readListPendingLessonsLimitArg(args map[string]any) (int, string) {
	raw, present := args[ToolArgLimit]
	if !present {
		return defaultListPendingLessonsLimit, ""
	}
	var n int
	switch v := raw.(type) {
	case int:
		n = v
	case int64:
		if v < int64(-(1<<31)) || v > int64(1<<31-1) {
			return 0, listPendingRefusalPrefix + ToolArgLimit + " out of range"
		}
		n = int(v)
	case float64:
		if v != float64(int(v)) {
			return 0, listPendingRefusalPrefix + ToolArgLimit + " must be an integer"
		}
		n = int(v)
	default:
		return 0, listPendingRefusalPrefix + ToolArgLimit + " must be a number"
	}
	if n < 1 {
		return 0, listPendingRefusalPrefix + ToolArgLimit + " must be ≥ 1"
	}
	if n > maxListPendingLessonsLimit {
		return 0, listPendingRefusalPrefix + ToolArgLimit +
			" must be ≤ " + strconv.Itoa(maxListPendingLessonsLimit)
	}
	return n, ""
}

// projectPendingLessons flattens [notebook.PendingLessonRow] values
// into the wire shape the agent receives. Drops the [Content] body
// per the docblock PII discipline.
//
// `cooling_off_hours_left` is computed as
// `(active_after_unix_ms - now_unix_ms) / hourMillis` with floor
// rounding. A defensive `<0 → 0` clamp handles the race where the DB
// returned a row that has just crossed the threshold between the
// query and the projection; the agent sees a zero cooling-off and
// can treat the row as "about to activate".
func projectPendingLessons(rows []notebook.PendingLessonRow, now time.Time) []map[string]any {
	const hourMillis = int64(time.Hour / time.Millisecond)
	nowMillis := now.UnixMilli()
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		delta := row.ActiveAfter - nowMillis
		hoursLeft := delta / hourMillis
		if hoursLeft < 0 {
			hoursLeft = 0
		}
		activeAfterUTC := time.UnixMilli(row.ActiveAfter).UTC().Format(time.RFC3339)
		out = append(out, map[string]any{
			"id":                     row.ID,
			"subject":                row.Subject,
			"active_after":           activeAfterUTC,
			"cooling_off_hours_left": int(hoursLeft),
		})
	}
	return out
}
