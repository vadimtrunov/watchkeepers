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
	rows, err := db.QueryContext(
		ctx,
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

// columnNames returns the set of column names on the `entry` table via
// `PRAGMA table_info`. Used by the M5.6.a migration regression to assert
// the `needs_review` column lands on both fresh-create and existing-DB paths.
func columnNames(ctx context.Context, t *testing.T, db *sql.DB) map[string]struct{} {
	t.Helper()
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(entry)`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer rows.Close()

	out := map[string]struct{}{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan pragma row: %v", err)
		}
		out[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pragma rows: %v", err)
	}
	return out
}

func TestSchema_IndexesPresent(t *testing.T) {
	db, ctx, _ := freshDB(t)

	names := indexNames(ctx, t, db.sql)
	for _, want := range []string{"entry_category_active", "entry_active_after", "entry_needs_review"} {
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
	if _, err := legacy.sql.ExecContext(
		ctx,
		`DROP INDEX IF EXISTS entry_category_active`,
	); err != nil {
		_ = legacy.Close()
		t.Fatalf("drop entry_category_active: %v", err)
	}
	if _, err := legacy.sql.ExecContext(
		ctx,
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
	if _, err := legacy.sql.ExecContext(
		ctx,
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

// ---- M5.6.a schema-migration regression tests ----------------------------

// TestSchema_NeedsReviewColumn_FreshDB pins AC1 fresh-create path: a brand
// new Notebook file carries `needs_review` inline from the CREATE TABLE DDL.
func TestSchema_NeedsReviewColumn_FreshDB(t *testing.T) {
	db, ctx, _ := freshDB(t)

	if _, ok := columnNames(ctx, t, db.sql)["needs_review"]; !ok {
		t.Fatal("needs_review column missing on fresh DB")
	}
	if _, ok := indexNames(ctx, t, db.sql)["entry_needs_review"]; !ok {
		t.Fatal("entry_needs_review index missing on fresh DB")
	}
}

// TestSchema_NeedsReviewColumn_DoubleOpen pins AC1 idempotency: re-opening a
// Notebook that already has the column is a no-op — the migration's PRAGMA
// probe short-circuits and no ALTER TABLE is issued. Asserts no error, column
// still present, data intact.
func TestSchema_NeedsReviewColumn_DoubleOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.sqlite")
	ctx := context.Background()

	first, err := openAt(ctx, path)
	if err != nil {
		t.Fatalf("openAt 1: %v", err)
	}
	if _, err := first.sql.ExecContext(
		ctx,
		`INSERT INTO entry(id, category, content, created_at) VALUES (?,?,?,?)`,
		"id-1", "lesson", "x", int64(1),
	); err != nil {
		_ = first.Close()
		t.Fatalf("insert: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	second, err := openAt(ctx, path)
	if err != nil {
		t.Fatalf("openAt 2 (re-init must not error): %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	if _, ok := columnNames(ctx, t, second.sql)["needs_review"]; !ok {
		t.Fatal("needs_review column missing after re-open")
	}
	var n int
	if err := second.sql.QueryRowContext(ctx, `SELECT count(*) FROM entry`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("row count = %d, want 1 (data lost across re-open)", n)
	}
}

// legacySchemaM5 is the M2b.2.a-era CREATE TABLE + virtual table DDL —
// the shape pre-M5.6.a Notebook files carry on disk (no needs_review).
const legacySchemaM5 = `
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

// buildLegacyNotebook creates a pre-M5.6.a Notebook file at path: drops the
// post-M5.6.a entry table (if any), applies the legacy DDL without
// needs_review, removes the entry_needs_review index, and inserts one row so
// data-survival can be verified after the upgrade. The returned *DB is
// already closed.
func buildLegacyNotebook(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	db, err := openAt(ctx, path)
	if err != nil {
		t.Fatalf("legacy openAt: %v", err)
	}
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS entry`,
		`DROP TABLE IF EXISTS entry_vec`,
		legacySchemaM5,
		`DROP INDEX IF EXISTS entry_needs_review`,
	} {
		if _, err := db.sql.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			t.Fatalf("legacy DDL %q: %v", stmt, err)
		}
	}
	if _, ok := columnNames(ctx, t, db.sql)["needs_review"]; ok {
		_ = db.Close()
		t.Fatal("needs_review column unexpectedly present in legacy fixture")
	}
	if _, err := db.sql.ExecContext(
		ctx,
		`INSERT INTO entry(id, category, content, created_at) VALUES (?,?,?,?)`,
		"00000000-0000-0000-0000-000000000001", "lesson", "x", int64(1),
	); err != nil {
		_ = db.Close()
		t.Fatalf("legacy insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("legacy close: %v", err)
	}
}

// assertUpgraded opens path via openAt (triggering the M5.6.a migration) and
// asserts: no error, needs_review column present, entry_needs_review index
// present, row count == 1, and the legacy row has needs_review = 0 from the
// ALTER TABLE DEFAULT clause.
func assertUpgraded(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	db, err := openAt(ctx, path)
	if err != nil {
		t.Fatalf("upgraded openAt: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, ok := columnNames(ctx, t, db.sql)["needs_review"]; !ok {
		t.Fatal("re-open did not add needs_review column")
	}
	if _, ok := indexNames(ctx, t, db.sql)["entry_needs_review"]; !ok {
		t.Fatal("re-open did not add entry_needs_review index")
	}
	var n int
	if err := db.sql.QueryRowContext(ctx, `SELECT count(*) FROM entry`).Scan(&n); err != nil {
		t.Fatalf("count after upgrade: %v", err)
	}
	if n != 1 {
		t.Fatalf("entry rows after upgrade = %d, want 1 (data lost)", n)
	}
	var nr int
	if err := db.sql.QueryRowContext(
		ctx,
		`SELECT needs_review FROM entry WHERE id = ?`,
		"00000000-0000-0000-0000-000000000001",
	).Scan(&nr); err != nil {
		t.Fatalf("read needs_review of legacy row: %v", err)
	}
	if nr != 0 {
		t.Fatalf("legacy row needs_review = %d after migration, want 0 (DEFAULT 0)", nr)
	}
}

// TestSchema_NeedsReviewColumn_LegacyMigration pins AC1's
// existing-DB-without-the-column path: a pre-M5.6.a file (no needs_review)
// must gain the column and the partial index on the first openAt by the new
// binary, with existing data surviving intact and the legacy row defaulting
// to needs_review = 0.
func TestSchema_NeedsReviewColumn_LegacyMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.sqlite")
	ctx := context.Background()

	buildLegacyNotebook(ctx, t, path)
	assertUpgraded(ctx, t, path)
}
