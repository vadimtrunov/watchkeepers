package notebook

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

// freshDB returns a [*DB] backed by a file under t.TempDir() and a context
// scoped to the test's lifetime. The temp dir is cleaned by the testing
// framework so each test starts with a pristine notebook file.
func freshDB(t *testing.T) (*DB, context.Context, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.sqlite")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db, err := openAt(ctx, path)
	if err != nil {
		t.Fatalf("openAt: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, ctx, path
}

func TestOpen_FreshAgent(t *testing.T) {
	db, ctx, _ := freshDB(t)

	var mode string
	if err := db.sql.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}

	for _, name := range []string{"entry", "entry_vec"} {
		var got string
		err := db.sql.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE name = ?`, name,
		).Scan(&got)
		if err != nil {
			t.Fatalf("missing table %q: %v", name, err)
		}
	}
}

func TestOpen_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.sqlite")
	ctx := context.Background()

	first, err := openAt(ctx, path)
	if err != nil {
		t.Fatalf("openAt 1: %v", err)
	}
	if _, err := first.sql.ExecContext(ctx,
		`INSERT INTO entry(id, category, content, created_at) VALUES (?,?,?,?)`,
		"id-1", "lesson", "remember this", int64(1),
	); err != nil {
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

	var n int
	if err := second.sql.QueryRowContext(ctx, `SELECT count(*) FROM entry`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("row count = %d, want 1 (data lost across re-open)", n)
	}
}

func TestClose_Idempotent(t *testing.T) {
	db, _, _ := freshDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close 2: %v (must be idempotent)", err)
	}
	var nilDB *DB
	if err := nilDB.Close(); err != nil {
		t.Fatalf("close nil: %v", err)
	}
}

func TestSchema_CategoryCheck(t *testing.T) {
	db, ctx, _ := freshDB(t)

	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO entry(id, category, content, created_at) VALUES (?,?,?,?)`,
		"id-bad", "banana", "x", int64(1),
	)
	if err == nil {
		t.Fatal("INSERT with bad category succeeded; CHECK constraint missing")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check") {
		t.Fatalf("expected CHECK constraint error, got: %v", err)
	}
}

func TestSchema_RequiredColumns(t *testing.T) {
	db, ctx, _ := freshDB(t)

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "missing category",
			sql:  `INSERT INTO entry(id, content, created_at) VALUES (?,?,?)`,
			args: []any{"id-1", "x", int64(1)},
		},
		{
			name: "missing content",
			sql:  `INSERT INTO entry(id, category, created_at) VALUES (?,?,?)`,
			args: []any{"id-2", "lesson", int64(1)},
		},
		{
			name: "missing created_at",
			sql:  `INSERT INTO entry(id, category, content) VALUES (?,?,?)`,
			args: []any{"id-3", "lesson", "x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.sql.ExecContext(ctx, tc.sql, tc.args...); err == nil {
				t.Fatalf("INSERT (%s) succeeded; NOT NULL missing", tc.name)
			}
		})
	}
}

func TestSchema_DefaultActiveAfter(t *testing.T) {
	db, ctx, _ := freshDB(t)

	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO entry(id, category, content, created_at) VALUES (?,?,?,?)`,
		"id-1", "lesson", "x", int64(1),
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var active int64
	if err := db.sql.QueryRowContext(ctx,
		`SELECT active_after FROM entry WHERE id = ?`, "id-1",
	).Scan(&active); err != nil {
		t.Fatalf("select: %v", err)
	}
	if active != 0 {
		t.Fatalf("active_after = %d, want 0", active)
	}
}

// allColumns mirrors the 11 user-supplied columns of the `entry` table; the
// 12th (id) is the PK. Held as a struct so the round-trip test can assert
// each field with a single helper rather than 11 inlined branches.
type allColumns struct {
	id              string
	category        string
	subject         sql.NullString
	content         string
	createdAt       int64
	lastUsedAt      sql.NullInt64
	relevanceScore  sql.NullFloat64
	supersededBy    sql.NullString
	evidenceLogRef  sql.NullString
	toolVersion     sql.NullString
	activeAfter     int64
	supersededByRow string // helper: row inserted first to satisfy FK
}

func (c allColumns) insert(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	if c.supersededByRow != "" {
		if _, err := db.sql.ExecContext(ctx,
			`INSERT INTO entry(id, category, content, created_at) VALUES (?,?,?,?)`,
			c.supersededByRow, "lesson", "newer", int64(2),
		); err != nil {
			t.Fatalf("seed superseded_by row: %v", err)
		}
	}
	if _, err := db.sql.ExecContext(ctx, `INSERT INTO entry(
		id, category, subject, content, created_at,
		last_used_at, relevance_score, superseded_by,
		evidence_log_ref, tool_version, active_after
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		c.id, c.category, c.subject, c.content, c.createdAt,
		c.lastUsedAt, c.relevanceScore, c.supersededBy,
		c.evidenceLogRef, c.toolVersion, c.activeAfter,
	); err != nil {
		t.Fatalf("insert all-columns row: %v", err)
	}
}

func (c allColumns) selectBack(ctx context.Context, t *testing.T, db *DB) allColumns {
	t.Helper()
	var got allColumns
	err := db.sql.QueryRowContext(ctx, `SELECT id, category, subject, content, created_at,
		         last_used_at, relevance_score, superseded_by,
		         evidence_log_ref, tool_version, active_after
		    FROM entry WHERE id = ?`, c.id,
	).Scan(
		&got.id, &got.category, &got.subject, &got.content, &got.createdAt,
		&got.lastUsedAt, &got.relevanceScore, &got.supersededBy,
		&got.evidenceLogRef, &got.toolVersion, &got.activeAfter,
	)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return got
}

func TestEntryRoundTrip_AllColumns(t *testing.T) {
	db, ctx, _ := freshDB(t)

	want := allColumns{
		id:              "00000000-0000-0000-0000-000000000001",
		category:        "preference",
		subject:         sql.NullString{String: "alice", Valid: true},
		content:         "i prefer X",
		createdAt:       1,
		lastUsedAt:      sql.NullInt64{Int64: 42, Valid: true},
		relevanceScore:  sql.NullFloat64{Float64: 0.875, Valid: true},
		supersededBy:    sql.NullString{String: "00000000-0000-0000-0000-000000000002", Valid: true},
		evidenceLogRef:  sql.NullString{String: "evidence-uuid", Valid: true},
		toolVersion:     sql.NullString{String: "v1.2.3", Valid: true},
		activeAfter:     99,
		supersededByRow: "00000000-0000-0000-0000-000000000002",
	}
	want.insert(ctx, t, db)
	got := want.selectBack(ctx, t, db)

	// Compare field-by-field. Avoids reflect.DeepEqual on sql.Null* whose
	// zero values shadow real "Valid=false" cases under typed scans.
	if got.id != want.id || got.category != want.category || got.content != want.content {
		t.Fatalf("base columns mismatch: %+v", got)
	}
	if got.createdAt != want.createdAt || got.activeAfter != want.activeAfter {
		t.Fatalf("ints mismatch: created_at=%d active_after=%d", got.createdAt, got.activeAfter)
	}
	if got.subject != want.subject ||
		got.lastUsedAt != want.lastUsedAt ||
		got.relevanceScore != want.relevanceScore ||
		got.supersededBy != want.supersededBy ||
		got.evidenceLogRef != want.evidenceLogRef ||
		got.toolVersion != want.toolVersion {
		t.Fatalf("nullable columns mismatch: got %+v", got)
	}
}

func TestVecExtensionLoaded(t *testing.T) {
	db, ctx, _ := freshDB(t)

	var ver string
	if err := db.sql.QueryRowContext(ctx, `SELECT vec_version()`).Scan(&ver); err != nil {
		t.Fatalf("vec_version() failed: %v", err)
	}
	if ver == "" {
		t.Fatalf("vec_version() returned empty string")
	}

	if _, err := db.sql.ExecContext(ctx,
		`CREATE VIRTUAL TABLE probe USING vec0(id TEXT PRIMARY KEY, embedding float[4])`,
	); err != nil {
		t.Fatalf("create probe vec0 table: %v", err)
	}
	rows := []struct {
		id  string
		vec []float32
	}{
		{"a", []float32{1, 0, 0, 0}},
		{"b", []float32{0, 1, 0, 0}},
		{"c", []float32{1, 1, 0, 0}},
	}
	for _, r := range rows {
		blob, err := sqlitevec.SerializeFloat32(r.vec)
		if err != nil {
			t.Fatalf("serialize %s: %v", r.id, err)
		}
		if _, err := db.sql.ExecContext(ctx,
			`INSERT INTO probe(id, embedding) VALUES (?, ?)`, r.id, blob,
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	q, err := sqlitevec.SerializeFloat32([]float32{1, 0, 0, 0})
	if err != nil {
		t.Fatalf("serialize query: %v", err)
	}
	cur, err := db.sql.QueryContext(ctx,
		`SELECT id, distance FROM probe WHERE embedding MATCH ? AND k = 3 ORDER BY distance`,
		q,
	)
	if err != nil {
		t.Fatalf("knn query: %v", err)
	}
	defer cur.Close()

	if !cur.Next() {
		t.Fatal("knn query returned no rows")
	}
	var first string
	var dist float64
	if err := cur.Scan(&first, &dist); err != nil {
		t.Fatalf("scan first: %v", err)
	}
	if first != "a" {
		t.Fatalf("nearest neighbour = %q, want %q", first, "a")
	}
	if dist != 0 {
		t.Fatalf("self-distance = %f, want 0", dist)
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("rows iter err: %v", err)
	}
}

func TestSchema_ForeignKeysEnabled(t *testing.T) {
	db, ctx, _ := freshDB(t)

	var fkOn int
	if err := db.sql.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkOn); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fkOn != 1 {
		t.Fatalf("foreign_keys = %d, want 1 (FK enforcement must be enabled)", fkOn)
	}
}

func TestSchema_SupersededByFK_Rejects(t *testing.T) {
	db, ctx, _ := freshDB(t)

	// Insert a valid parent row first so we know the FK column works when
	// the referenced id exists.
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO entry(id, category, content, created_at) VALUES (?,?,?,?)`,
		"parent-id", "lesson", "parent", int64(1),
	); err != nil {
		t.Fatalf("insert parent: %v", err)
	}

	// This INSERT references a non-existent id and must be rejected by the
	// REFERENCES entry(id) FK constraint.
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO entry(id, category, content, created_at, superseded_by) VALUES (?,?,?,?,?)`,
		"child-id", "lesson", "child", int64(2), "non-existent-uuid",
	)
	if err == nil {
		t.Fatal("INSERT with invalid superseded_by succeeded; FK constraint not enforced")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("expected FOREIGN KEY constraint error, got: %v", err)
	}
}

func TestOpen_PermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0 trick is bypassed")
	}
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unwriteable parent dir is Unix-only")
	}

	parent := t.TempDir()
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatalf("chmod 0 parent: %v", err)
	}
	t.Cleanup(func() {
		// Restore so t.TempDir cleanup can rm -rf without permission errors.
		_ = os.Chmod(parent, 0o700)
	})

	dbFile := filepath.Join(parent, "child", "agent.sqlite")
	_, err := openAt(context.Background(), dbFile)
	if err == nil {
		t.Fatal("openAt under unwriteable parent succeeded; want a wrapped error")
	}
}

func TestOpen_PathFromAgentDBPath(t *testing.T) {
	t.Setenv(envDataDir, t.TempDir())
	_, err := Open(context.Background(), "not-a-uuid")
	if !errors.Is(err, ErrInvalidAgentID) {
		t.Fatalf("Open(bad UUID): err=%v, want ErrInvalidAgentID", err)
	}

	db, err := Open(context.Background(), testAgentID)
	if err != nil {
		t.Fatalf("Open(good UUID): %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
