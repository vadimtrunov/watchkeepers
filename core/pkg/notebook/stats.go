package notebook

import (
	"context"
	"fmt"
	"time"
)

// Stats returns aggregate counts over the agent's notebook:
//
//   - `TotalEntries` — every row, including superseded ones;
//   - `Active` — `superseded_by IS NULL AND active_after <= now`;
//   - `Superseded` — `superseded_by IS NOT NULL`;
//   - `ByCategory` — count of *active* entries per category, with the five
//     [categoryEnum] keys always present (zero when none).
//
// Two queries are issued back-to-back rather than one CTE so the second can
// drive the partial index on `entry(category) WHERE superseded_by IS NULL`.
// The pair is not wrapped in a transaction: under WAL a brief write between
// the two queries can produce slightly inconsistent totals, which we accept
// because Stats is a diagnostic surface (M2b.6 Watchmaster reports it once
// per planning cycle), not a transactional read.
func (d *DB) Stats(ctx context.Context) (Stats, error) {
	nowMillis := time.Now().UnixMilli()

	var s Stats
	if err := d.sql.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE superseded_by IS NULL AND active_after <= ?),
			COUNT(*) FILTER (WHERE superseded_by IS NOT NULL)
		FROM entry
	`, nowMillis).Scan(&s.TotalEntries, &s.Active, &s.Superseded); err != nil {
		return Stats{}, fmt.Errorf("notebook: stats totals: %w", err)
	}

	// Pre-populate every category with zero so callers get a stable map
	// shape regardless of which categories actually have rows.
	s.ByCategory = make(map[string]int, len(categoryEnum))
	for cat := range categoryEnum {
		s.ByCategory[cat] = 0
	}

	rows, err := d.sql.QueryContext(ctx, `
		SELECT category, COUNT(*)
		FROM entry
		WHERE superseded_by IS NULL AND active_after <= ?
		GROUP BY category
	`, nowMillis)
	if err != nil {
		return Stats{}, fmt.Errorf("notebook: stats by-category query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cat string
			n   int
		)
		if err := rows.Scan(&cat, &n); err != nil {
			return Stats{}, fmt.Errorf("notebook: stats scan: %w", err)
		}
		s.ByCategory[cat] = n
	}
	if err := rows.Err(); err != nil {
		return Stats{}, fmt.Errorf("notebook: stats iterate: %w", err)
	}
	return s, nil
}
