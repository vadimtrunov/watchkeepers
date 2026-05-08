package notebook

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// flagCandidate is one row from the `FlagSupersededLessons` SELECT scan.
// Held at package scope so the per-row decision helper
// `evaluateAndFlagCandidate` can name it in its signature; otherwise
// it would be a function-local struct trapped inside the outer body.
type flagCandidate struct {
	id          string
	subject     string
	toolVersion string
}

// flagConfig holds the resolved options for [DB.FlagSupersededLessons]. The
// fields stay package-private so the public surface is the option type plus
// the helper signature — adding a new field here only requires a sibling
// `WithX` option and does not break existing callers.
type flagConfig struct {
	logger *slog.Logger
}

// FlagOption configures a single [DB.FlagSupersededLessons] invocation.
// Functional-option discipline mirrors [WithLogger] on [DB] and the
// [ToolErrorReflectorOption] surface in `core/pkg/runtime`: callers
// supply zero or more options at the call site without forcing a wider
// helper signature.
type FlagOption func(*flagConfig)

// WithFlagLogger threads a structured [*slog.Logger] onto a
// [DB.FlagSupersededLessons] call. The helper logs:
//
//   - a debug breadcrumb for each lesson row whose subject does not match
//     the M5.6.b `composeSubject` `"<toolName>: <errClass>"` shape (those
//     rows are skipped, never flagged); and
//   - a single info-level summary on completion with the scanned and
//     flagged counters.
//
// A nil logger is a no-op so callers can always pass through whatever
// they have. The default — applied by the helper when the option is
// omitted — is [slog.Default]. The name `WithFlagLogger` (rather than the
// shorter `WithLogger`) avoids shadowing the package-level [WithLogger]
// [DBOption] used by [Open] for audit-emit wiring.
func WithFlagLogger(l *slog.Logger) FlagOption {
	return func(c *flagConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// FlagSupersededLessons scans `lesson` rows whose `tool_version` is set
// and `needs_review = 0`, and flips each row whose tool name (parsed
// from the M5.6.b `composeSubject` `"<toolName>: <errClass>"` subject
// shape) is either absent from `currentVersions` (the tool has been
// retired) OR maps to a different version (the tool was upgraded). The
// flip is performed via [DB.MarkNeedsReview] per row — the M5.6.a
// primitive — so the row count is small (lessons-only, indexed) and the
// per-row UPDATE benefits from the same partial-index fast path.
//
// The returned `newlyFlagged` is the count of rows whose `needs_review`
// transitioned from 0 to 1 in this call. Already-flagged rows are
// excluded from the candidate set by the SQL WHERE clause, so the count
// matches the cardinality of `MarkNeedsReview` invocations on the happy
// path. Rows whose subject does not match the composeSubject shape are
// SKIPPED — the helper logs a debug breadcrumb via the configured logger
// and does not flag them. Rows with `tool_version IS NULL` or empty
// string are also SKIPPED at the SQL layer (no version target to compare
// against).
//
// # Comparison rule
//
// A lesson is flagged when EITHER:
//
//   - `currentVersions[toolName] != lesson.toolVersion`, OR
//   - `currentVersions` does not contain `toolName` (tool retired).
//
// A nil or empty `currentVersions` map yields the second branch on every
// row — every lesson is treated as "tool retired" and flagged.
//
// # Context cancellation
//
// The helper honours `ctx` cancellation between rows: each iteration
// checks `ctx.Err()` before issuing the per-row UPDATE so a cancellation
// stops further flips at the next loop boundary. Rows already flagged in
// earlier iterations remain flagged — the per-row UPDATE is autocommit
// and is not rolled back on cancellation. This mirrors
// [DB.RecallFilterCounts]'s ctx-honouring discipline.
//
// # Subject parsing contract
//
// The first `": "` separator in the subject splits the toolName prefix
// from the rest; the helper does NOT assume the suffix matches any
// particular shape. An empty subject (or one without `": "`) is skipped.
// A subject of the literal form `": foo"` (empty toolName prefix) is
// also skipped — an empty toolName cannot match a key in
// `currentVersions`.
//
// # Scope notes
//
// Subject-parsing is intentionally fragile: it documents the M5.6.b
// `composeSubject` format as the contract. A future schema migration
// adding a dedicated `tool_name` column on the `entry` table would
// replace this parsing pass; the runtime callsite that invokes a
// supervisor BootCheck from a manifest-aware boot path is also
// out of scope here.
func (d *DB) FlagSupersededLessons(
	ctx context.Context,
	currentVersions map[string]string,
	opts ...FlagOption,
) (newlyFlagged int, err error) {
	cfg := flagConfig{logger: slog.Default()}
	for _, opt := range opts {
		opt(&cfg)
	}

	// Honour cancellation before the SELECT — mirrors the
	// [DB.RecallFilterCounts] discipline of returning ctx.Err verbatim
	// before any DB work when the caller pre-cancelled.
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// Candidate set: lesson rows that are otherwise live, carry a
	// non-empty tool_version, and have not yet been flagged. The
	// `superseded_by IS NULL` clause keeps already-superseded rows out
	// of the scan — those have been retired by an explicit
	// supersession edge and need no second flag.
	const selectSQL = `
		SELECT id, COALESCE(subject, ''), COALESCE(tool_version, '')
		  FROM entry
		 WHERE category = 'lesson'
		   AND superseded_by IS NULL
		   AND tool_version IS NOT NULL
		   AND tool_version != ''
		   AND needs_review = 0
	`
	rows, err := d.sql.QueryContext(ctx, selectSQL)
	if err != nil {
		return 0, fmt.Errorf("notebook: select superseded candidates: %w", err)
	}
	defer rows.Close()

	// Materialise the scan first so the per-row MarkNeedsReview UPDATE
	// does not race the SELECT cursor — `database/sql` with a single
	// open conn would otherwise deadlock on the second statement while
	// the first cursor still holds the conn.
	var candidates []flagCandidate
	for rows.Next() {
		var c flagCandidate
		if err := rows.Scan(&c.id, &c.subject, &c.toolVersion); err != nil {
			return 0, fmt.Errorf("notebook: scan superseded candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("notebook: iterate superseded candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("notebook: close superseded candidates: %w", err)
	}

	scanned := len(candidates)
	for _, c := range candidates {
		flipped, err := d.evaluateAndFlagCandidate(ctx, c, currentVersions, cfg.logger)
		if err != nil {
			return newlyFlagged, err
		}
		if flipped {
			newlyFlagged++
		}
	}

	if cfg.logger != nil {
		cfg.logger.LogAttrs(
			ctx, slog.LevelInfo,
			"notebook: flag_superseded completed",
			slog.Int("scanned", scanned),
			slog.Int("newly_flagged", newlyFlagged),
		)
	}

	return newlyFlagged, nil
}

// evaluateAndFlagCandidate decides — for a single candidate row — whether
// the lesson should be flagged, and performs the flip via
// [DB.MarkNeedsReview] when the decision is "yes". Returns `(flipped,
// err)` where `flipped` is true exactly when MarkNeedsReview was called
// and succeeded. The split out of [DB.FlagSupersededLessons] keeps the
// outer loop's cyclomatic complexity inside the project's gocyclo cap
// without changing observable behaviour.
//
// Decision tree (mirrors the godoc on [DB.FlagSupersededLessons]):
//   - cancelled ctx → return ctx.Err verbatim
//   - unparseable subject → emit debug breadcrumb (when logger != nil),
//     return (false, nil)
//   - currentVersions[toolName] == c.toolVersion → return (false, nil)
//   - otherwise → call MarkNeedsReview and return (true, nil) on success
func (d *DB) evaluateAndFlagCandidate(
	ctx context.Context,
	c flagCandidate,
	currentVersions map[string]string,
	logger *slog.Logger,
) (flipped bool, err error) {
	// ctx.Err between rows so a cancelled context halts further flips at
	// the next loop boundary.
	if err := ctx.Err(); err != nil {
		return false, err
	}

	toolName, ok := extractToolNameFromSubject(c.subject)
	if !ok {
		if logger != nil {
			logger.LogAttrs(
				ctx, slog.LevelDebug,
				"notebook: flag_superseded skipped row with unparseable subject",
				slog.String("entry_id", c.id),
				slog.String("subject", c.subject),
			)
		}
		return false, nil
	}

	current, present := currentVersions[toolName]
	if present && current == c.toolVersion {
		return false, nil
	}

	if err := d.MarkNeedsReview(ctx, c.id); err != nil {
		return false, fmt.Errorf("notebook: mark needs_review %q: %w", c.id, err)
	}
	return true, nil
}

// extractToolNameFromSubject returns the prefix before the FIRST `": "`
// separator in `subject`. An empty subject, a subject without the
// separator, and a subject whose prefix would be empty (e.g. `": foo"`)
// all return `("", false)`. The contract pins the M5.6.b
// `composeSubject` format: see [composeSubject] in
// `core/pkg/runtime/tool_error_reflector.go`.
func extractToolNameFromSubject(subject string) (toolName string, ok bool) {
	idx := strings.Index(subject, ": ")
	if idx <= 0 {
		return "", false
	}
	return subject[:idx], true
}
