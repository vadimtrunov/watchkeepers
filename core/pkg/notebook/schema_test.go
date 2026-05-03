package notebook

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// indexNames returns the set of user-defined index names present in the
// open DB. SQLite synthesises auto-indexes for PKs / UNIQUE constraints with
// names starting `sqlite_autoindex_`; we filter those out so the test focuses
// on indexes the schema-init code creates explicitly.
func indexNames(ctx context.Context, t *testing.T, db *sql.DB) map[string]struct{} {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_schema WHERE type = 'index' AND name NOT LIKE 'sqlite_autoindex_%'`,
	)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer rows.Close()

	out := map[string]struct{}{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		out[n] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate indexes: %v", err)
	}
	return out
}

func TestSchema_IndexesPresent(t *testing.T) {
	db, ctx, _ := freshDB(t)

	names := indexNames(ctx, t, db.sql)
	for _, want := range []string{"entry_category_active", "entry_active_after"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("missing index %q (got %v)", want, names)
		}
	}
}

func TestSchema_IndexesAddedOnReopen(t *testing.T) {
	// Simulate an upgrade from M2b.1 to M2b.2.a: open a SQLite file with the
	// pre-M2b.2.a schema, confirm the indexes are missing, then reopen via
	// the package's openAt and confirm the indexes were added by the
	// idempotent schema-init.
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.sqlite")
	ctx := context.Background()

	// Step 1: build a "legacy" file with the M2b.1 schema (no new indexes).
	const legacySchema = `
		CREATE TABLE IF NOT EXISTS entry (
			id TEXT PRIMARY KEY,
			category TEXT NOT NULL CHECK (category IN ('lesson', 'preference', 'observation', 'pending_task', 'relationship_note')),
			subject TEXT NULL,
			content TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			last_used_at INTEGER NULL,
			relevance_score REAL NULL,
			superseded_by TEXT NULL REFERENCES entry(id),
			evidence_log_ref TEXT NULL,
			tool_version TEXT NULL,
			active_after INTEGER NOT NULL DEFAULT 0
		);
		CREATE VIRTUAL TABLE IF NOT EXISTS entry_vec USING vec0(
			id TEXT PRIMARY KEY,
			embedding float[1536]
		);
	`

	// Open via openAt first so sqlite-vec is registered as an auto-extension
	// (vec0 needs the extension active). Then drop any indexes the current
	// schema-init might have created so we can prove they get re-added.
	legacy, err := openAt(ctx, path)
	if err != nil {
		t.Fatalf("legacy openAt: %v", err)
	}
	if _, err := legacy.sql.ExecContext(ctx, legacySchema); err != nil {
		_ = legacy.Close()
		t.Fatalf("apply legacy schema: %v", err)
	}
	if _, err := legacy.sql.ExecContext(ctx,
		`DROP INDEX IF EXISTS entry_category_active`,
	); err != nil {
		_ = legacy.Close()
		t.Fatalf("drop entry_category_active: %v", err)
	}
	if _, err := legacy.sql.ExecContext(ctx,
		`DROP INDEX IF EXISTS entry_active_after`,
	); err != nil {
		_ = legacy.Close()
		t.Fatalf("drop entry_active_after: %v", err)
	}

	// Confirm the indexes are gone.
	before := indexNames(ctx, t, legacy.sql)
	for _, gone := range []string{"entry_category_active", "entry_active_after"} {
		if _, present := before[gone]; present {
			_ = legacy.Close()
			t.Fatalf("index %q unexpectedly present after DROP", gone)
		}
	}

	// Insert a row to confirm data survives the upgrade.
	if _, err := legacy.sql.ExecContext(ctx,
		`INSERT INTO entry(id, category, content, created_at) VALUES (?,?,?,?)`,
		"00000000-0000-0000-0000-000000000001", "lesson", "x", int64(1),
	); err != nil {
		_ = legacy.Close()
		t.Fatalf("legacy insert: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("legacy close: %v", err)
	}

	// Step 2: re-open with the current schema-init; the partial indexes must
	// appear without losing data.
	upgraded, err := openAt(ctx, path)
	if err != nil {
		t.Fatalf("upgraded openAt: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	after := indexNames(ctx, t, upgraded.sql)
	for _, want := range []string{"entry_category_active", "entry_active_after"} {
		if _, ok := after[want]; !ok {
			t.Fatalf("re-open did not add index %q (got %v)", want, after)
		}
	}

	var n int
	if err := upgraded.sql.QueryRowContext(ctx, `SELECT count(*) FROM entry`).Scan(&n); err != nil {
		t.Fatalf("count after upgrade: %v", err)
	}
	if n != 1 {
		t.Fatalf("entry rows after upgrade = %d, want 1 (data lost)", n)
	}
}
