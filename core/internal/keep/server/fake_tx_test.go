package server

// fake_tx_test.go — minimal in-memory pgx.Tx / pgx.Row fakes for
// happy-path handler unit tests. None of this code is reachable from
// production binaries: the `package server` build tag means it only
// compiles into the `*_test` binary alongside export_test.go.
//
// QueryRow, Query and Exec are the supported surface — handlers that hit
// any other pgx.Tx method see a panic with a descriptive message so
// accidental calls surface immediately rather than silently returning
// zero values.

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeRow implements pgx.Row. The default behaviour writes id into the first
// *string destination and ignores the rest. When ScanFn is set it is
// consulted instead, letting tests stage a richer multi-column row (e.g.
// the `SELECT status FOR UPDATE` step in handleUpdateWatchkeeperStatus).
type fakeRow struct {
	id     string
	err    error
	scanFn func(dest ...any) error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.scanFn != nil {
		return r.scanFn(dest...)
	}
	if len(dest) > 0 {
		if sp, ok := dest[0].(*string); ok {
			*sp = r.id
		}
	}
	return nil
}

// fakeRows implements pgx.Rows for tests that exercise list-style handlers
// (handleListWatchkeepers). It walks the configured Scans slice on each
// Next call; each entry is a closure that writes its row's columns into
// the destination pointers.
type fakeRows struct {
	scans   []func(dest ...any) error
	idx     int
	scanErr error
	rowsErr error
	closed  bool
}

func (r *fakeRows) Next() bool {
	if r.idx >= len(r.scans) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if r.idx == 0 || r.idx > len(r.scans) {
		return errors.New("fakeRows: Scan called out of order")
	}
	return r.scans[r.idx-1](dest...)
}

func (r *fakeRows) Close()                                       { r.closed = true }
func (r *fakeRows) Err() error                                   { return r.rowsErr }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error) {
	return nil, errors.New("fakeRows: Values not implemented")
}
func (r *fakeRows) RawValues() [][]byte { return nil }
func (r *fakeRows) Conn() *pgx.Conn     { return nil }

// fakeTx implements pgx.Tx. QueryRow / Query / Exec are functional and
// configurable; the rest panic. Tests inject behaviour via the function
// fields (QueryRowFn, QueryFn, ExecFn) — the simple `id` constructor stays
// available for the existing tests that only need a happy-path RETURNING id.
type fakeTx struct {
	id       string
	queryRow func(ctx context.Context, sql string, args ...any) pgx.Row
	query    func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	exec     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func newFakeTx(id string) pgx.Tx { return &fakeTx{id: id} }

// newFakeTxWithFns builds a fakeTx with caller-supplied behaviour for the
// three exercised methods. Any nil function field falls back to the default
// (QueryRow returns the canned id; Query / Exec panic with a descriptive
// message). Used by handler tests that need to stage a multi-step tx.
func newFakeTxWithFns(
	queryRow func(ctx context.Context, sql string, args ...any) pgx.Row,
	query func(ctx context.Context, sql string, args ...any) (pgx.Rows, error),
	exec func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error),
) pgx.Tx {
	return &fakeTx{queryRow: queryRow, query: query, exec: exec}
}

func (t *fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if t.queryRow != nil {
		return t.queryRow(ctx, sql, args...)
	}
	return fakeRow{id: t.id}
}

func (t *fakeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if t.query != nil {
		return t.query(ctx, sql, args...)
	}
	panic("fakeTx: Query not implemented")
}

func (t *fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if t.exec != nil {
		return t.exec(ctx, sql, args...)
	}
	panic("fakeTx: Exec not implemented")
}

func (t *fakeTx) Begin(_ context.Context) (pgx.Tx, error) {
	panic("fakeTx: Begin not implemented")
}

func (t *fakeTx) Commit(_ context.Context) error {
	return nil
}

func (t *fakeTx) Rollback(_ context.Context) error {
	return nil
}

func (t *fakeTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	panic("fakeTx: CopyFrom not implemented")
}

func (t *fakeTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	panic("fakeTx: SendBatch not implemented")
}

func (t *fakeTx) LargeObjects() pgx.LargeObjects {
	panic("fakeTx: LargeObjects not implemented")
}

func (t *fakeTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	panic("fakeTx: Prepare not implemented")
}

func (t *fakeTx) Conn() *pgx.Conn {
	return nil
}
