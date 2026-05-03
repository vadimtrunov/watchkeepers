package notebook

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

// Recall runs a sqlite-vec cosine KNN over `entry_vec`, joins the result back
// against `entry` to project the regular columns, and applies the
// "active and not superseded" filter required by AC2:
//
//   - rows with `superseded_by IS NOT NULL` are excluded;
//   - rows with `active_after > activeAt.UnixMilli()` are excluded;
//   - if [RecallQuery.Category] is non-empty, only rows in that category are
//     returned.
//
// `TopK` must be > 0; values above [maxTopK] are silently clamped. The query
// vector length must equal [EmbeddingDim]. Both shape errors return
// [ErrInvalidEntry] without touching the database.
//
// Results are ordered by ascending cosine distance reported by sqlite-vec.
func (d *DB) Recall(ctx context.Context, q RecallQuery) ([]RecallResult, error) {
	if q.TopK <= 0 {
		return nil, ErrInvalidEntry
	}
	if len(q.Embedding) != EmbeddingDim {
		return nil, ErrInvalidEntry
	}
	if q.Category != "" {
		if _, ok := categoryEnum[q.Category]; !ok {
			return nil, ErrInvalidEntry
		}
	}

	topK := q.TopK
	if topK > maxTopK {
		topK = maxTopK
	}

	activeAt := q.ActiveAt
	if activeAt.IsZero() {
		activeAt = time.Now()
	}
	activeAtMillis := activeAt.UnixMilli()

	queryBlob, err := sqlitevec.SerializeFloat32(q.Embedding)
	if err != nil {
		return nil, fmt.Errorf("notebook: serialize query embedding: %w", err)
	}

	// The CTE pulls `topK` candidates ranked by sqlite-vec cosine distance,
	// then we join against `entry` to apply the regular-table filters. Doing
	// the filters in the outer query (vs an `IN (...)` subselect) lets the
	// optimiser use the M2b.2.a partial indexes on `category` and
	// `active_after`. The `:= ?` placeholder is used four times: the query
	// blob, the K, the activeAt millis, and the optional category filter
	// (twice — once for the empty-string sentinel, once for equality).
	const sqlText = `
		WITH knn AS (
			SELECT id, distance
			FROM entry_vec
			WHERE embedding MATCH ? AND k = ?
		)
		SELECT e.id, e.category, e.subject, e.content, e.created_at,
		       e.last_used_at, e.relevance_score,
		       e.evidence_log_ref, e.tool_version, e.active_after,
		       knn.distance
		FROM knn
		JOIN entry e ON e.id = knn.id
		WHERE e.superseded_by IS NULL
		  AND e.active_after <= ?
		  AND (? = '' OR e.category = ?)
		ORDER BY knn.distance ASC
	`

	rows, err := d.sql.QueryContext(ctx, sqlText,
		queryBlob,
		topK,
		activeAtMillis,
		q.Category,
		q.Category,
	)
	if err != nil {
		return nil, fmt.Errorf("notebook: recall query: %w", err)
	}
	defer rows.Close()

	out := make([]RecallResult, 0, topK)
	for rows.Next() {
		var (
			r              RecallResult
			subject        sql.NullString
			lastUsedAt     sql.NullInt64
			relevanceScore sql.NullFloat64
			evidenceLogRef sql.NullString
			toolVersion    sql.NullString
		)
		if err := rows.Scan(
			&r.ID, &r.Category, &subject, &r.Content, &r.CreatedAt,
			&lastUsedAt, &relevanceScore,
			&evidenceLogRef, &toolVersion, &r.ActiveAfter,
			&r.Distance,
		); err != nil {
			return nil, fmt.Errorf("notebook: scan recall row: %w", err)
		}
		r.Subject = stringFromNullable(subject)
		r.LastUsedAt = int64PtrFromNullable(lastUsedAt)
		r.RelevanceScore = float64PtrFromNullable(relevanceScore)
		r.EvidenceLogRef = stringPtrFromNullable(evidenceLogRef)
		r.ToolVersion = stringPtrFromNullable(toolVersion)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notebook: iterate recall rows: %w", err)
	}
	return out, nil
}
