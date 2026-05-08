package notebook

import (
	"context"
	"fmt"
	"time"
)

// RecallFilterCounts returns the number of rows excluded from a [DB.Recall]
// by the cooling-off (`active_after > now`) and needs_review (`needs_review
// = 1`) predicates respectively. The returned `coolingOff` and `needsReview`
// counters are independent and disjoint by construction — a row that is
// both cooling-off AND needs_review is counted only by the predicate the
// query encounters first; in practice the two flags rarely co-exist on the
// same row, and the diagnostic shape values aggregate exclusion volume,
// not per-row classification.
//
// # Why this is a distinct API and not a third column on [DB.Recall]
//
// [DB.Recall]'s SQL applies the predicates in its WHERE clause and emits
// only the surviving rows; counting the excluded rows would require either
// a wider scan (defeating the partial-index fast-path) or a `WITH ... AS`
// CTE that breaks the optimiser's plan. M5.6.d's design lands the counters
// in a sibling helper so [DB.Recall]'s WHERE clause and result shape stay
// frozen — a regression that drops the SQL-level filter is still caught at
// the [BuildTurnRequest] layer, while the diagnostic counters are computed
// without burdening the hot path.
//
// # Implementation: simple counts, not KNN-correlated
//
// The two counters run lightweight `SELECT COUNT(*)` queries against the
// `entry` table directly, ignoring [RecallQuery.Embedding] and
// [RecallQuery.TopK]. KNN-correlated counts ("how many of the TopK
// nearest neighbours would have been excluded") were considered but
// rejected: (a) the test plan does not pin KNN ranking on the counters;
// (b) M5.6.a's `entry_needs_review` partial index and M2b.2.a's
// `entry_active_after` partial index make the simple counts O(rows that
// match the predicate), independent of the embedding query; (c) the
// counters are diagnostic — reporting "X cooling-off rows exist for this
// agent" is more useful than "X cooling-off rows would have ranked in the
// TopK" for operator dashboards. The [RecallQuery] argument is preserved
// for signature symmetry with [DB.Recall] and to leave room for a future
// switch to KNN-correlated counts without an API break.
//
// # Predicate alignment with [DB.Recall]
//
// The cooling-off counter mirrors `superseded_by IS NULL AND active_after
// > activeAt`; the needs_review counter mirrors `superseded_by IS NULL
// AND needs_review = 1`. Rows excluded by `superseded_by IS NOT NULL`
// alone are NOT counted by either — that is a different exclusion class,
// reported by [DB.Stats.Superseded] instead.
//
// # Return semantics
//
// Returns `(0, 0, nil)` for an empty notebook. Validation errors
// (`TopK <= 0`, embedding-dim mismatch) return `[ErrInvalidEntry]`
// without touching the database, mirroring [DB.Recall]. A cancelled
// context surfaces ctx.Err verbatim from the first
// [database/sql].QueryRowContext call.
func (d *DB) RecallFilterCounts(ctx context.Context, q RecallQuery) (coolingOff, needsReview int, err error) {
	if q.TopK <= 0 {
		return 0, 0, ErrInvalidEntry
	}
	if len(q.Embedding) != EmbeddingDim {
		return 0, 0, ErrInvalidEntry
	}

	activeAt := q.ActiveAt
	if activeAt.IsZero() {
		activeAt = time.Now()
	}
	activeAtMillis := activeAt.UnixMilli()

	// Cooling-off: rows that are otherwise live (`superseded_by IS NULL`)
	// but whose `active_after > now`. Excludes needs_review rows so the
	// two counters stay disjoint — a needs_review row that is ALSO in
	// cooling-off is counted only by the needs_review counter, mirroring
	// the order [DB.Recall]'s WHERE clause evaluates the predicates in.
	const coolingOffSQL = `
		SELECT COUNT(*) FROM entry
		 WHERE superseded_by IS NULL
		   AND needs_review = 0
		   AND active_after > ?
	`
	if err := d.sql.QueryRowContext(ctx, coolingOffSQL, activeAtMillis).Scan(&coolingOff); err != nil {
		return 0, 0, fmt.Errorf("notebook: cooling-off count: %w", err)
	}

	// Needs-review: rows flagged with `needs_review = 1` and not yet
	// superseded. Hits the M5.6.a `entry_needs_review` partial index.
	const needsReviewSQL = `
		SELECT COUNT(*) FROM entry
		 WHERE superseded_by IS NULL
		   AND needs_review = 1
	`
	if err := d.sql.QueryRowContext(ctx, needsReviewSQL).Scan(&needsReview); err != nil {
		return 0, 0, fmt.Errorf("notebook: needs-review count: %w", err)
	}

	return coolingOff, needsReview, nil
}
