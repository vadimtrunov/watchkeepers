package notebook

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	"github.com/google/uuid"
)

// Remember inserts a new entry into both `entry` and `entry_vec` in a single
// transaction, generating a UUID v7 id when [Entry.ID] is empty and defaulting
// [Entry.CreatedAt] to `time.Now().UnixMilli()` when zero.
//
// Validation runs before any DB work; on failure [ErrInvalidEntry] is
// returned and the database is left untouched. The two INSERTs honour the
// "Sync contract" documented in the package godoc: the row in `entry_vec`
// always lands in the same transaction as the row in `entry`, so a partial
// commit cannot leave the two tables out of sync.
//
// Returns the id (auto-generated or caller-supplied) on success.
func (d *DB) Remember(ctx context.Context, e Entry) (string, error) {
	if err := validate(&e); err != nil {
		return "", err
	}

	if e.ID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return "", fmt.Errorf("notebook: generate uuid v7: %w", err)
		}
		e.ID = id.String()
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = time.Now().UnixMilli()
	}

	embeddingBlob, err := sqlitevec.SerializeFloat32(e.Embedding)
	if err != nil {
		return "", fmt.Errorf("notebook: serialize embedding: %w", err)
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("notebook: begin remember tx: %w", err)
	}
	// Rollback is a no-op after a successful commit; safe to defer
	// unconditionally.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entry(
			id, category, subject, content, created_at,
			last_used_at, relevance_score, superseded_by,
			evidence_log_ref, tool_version, active_after
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)
	`,
		e.ID,
		e.Category,
		nullableString(e.Subject),
		e.Content,
		e.CreatedAt,
		nullableInt64Ptr(e.LastUsedAt),
		nullableFloat64Ptr(e.RelevanceScore),
		nullableStringPtr(e.SupersededBy),
		nullableStringPtr(e.EvidenceLogRef),
		nullableStringPtr(e.ToolVersion),
		e.ActiveAfter,
	); err != nil {
		return "", fmt.Errorf("notebook: insert entry: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO entry_vec(id, embedding) VALUES (?, ?)`,
		e.ID, embeddingBlob,
	); err != nil {
		return "", fmt.Errorf("notebook: insert entry_vec: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("notebook: commit remember tx: %w", err)
	}

	return e.ID, nil
}

// nullableString maps the empty string to a SQL NULL and any other value to a
// non-NULL TEXT. Used for the optional `subject` column where we collapse
// empty input into NULL at the storage layer.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableStringPtr maps a nil pointer to SQL NULL and a non-nil pointer to
// the dereferenced TEXT value (including the empty string, kept distinct
// from NULL because callers explicitly opted into a pointer type).
func nullableStringPtr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// nullableInt64Ptr maps a nil pointer to SQL NULL and a non-nil pointer to
// the dereferenced INTEGER value.
func nullableInt64Ptr(i *int64) any {
	if i == nil {
		return nil
	}
	return *i
}

// nullableFloat64Ptr maps a nil pointer to SQL NULL and a non-nil pointer to
// the dereferenced REAL value.
func nullableFloat64Ptr(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// stringFromNullable maps a sql.NullString to a Go string, treating NULL as
// the empty string. Used by [DB.Recall] to project the optional `subject`
// column back into [RecallResult.Subject].
func stringFromNullable(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	return ns.String
}

// stringPtrFromNullable maps a sql.NullString to a *string, treating NULL as
// nil. Used by [DB.Recall] to preserve nullability for the columns whose API
// shape is `*string`.
func stringPtrFromNullable(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

// int64PtrFromNullable maps a sql.NullInt64 to a *int64, treating NULL as nil.
func int64PtrFromNullable(ni sql.NullInt64) *int64 {
	if !ni.Valid {
		return nil
	}
	v := ni.Int64
	return &v
}

// float64PtrFromNullable maps a sql.NullFloat64 to a *float64, treating NULL
// as nil.
func float64PtrFromNullable(nf sql.NullFloat64) *float64 {
	if !nf.Valid {
		return nil
	}
	v := nf.Float64
	return &v
}
