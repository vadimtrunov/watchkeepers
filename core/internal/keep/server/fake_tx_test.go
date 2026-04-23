package server

// fake_tx_test.go — minimal in-memory pgx.Tx / pgx.Row fakes for
// happy-path handler unit tests. None of this code is reachable from
// production binaries: the `package server` build tag means it only
// compiles into the `*_test` binary alongside export_test.go.
//
// Only QueryRow is meaningful. Every other pgx.Tx method panics with a
// descriptive message so accidental calls surface immediately rather
// than silently returning zero values.

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeRow implements pgx.Row. Its Scan writes fakeID into the first
// *string destination and ignores the rest.
type fakeRow struct{ id string }

func (r fakeRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if sp, ok := dest[0].(*string); ok {
			*sp = r.id
		}
	}
	return nil
}

// fakeTx implements pgx.Tx. Only QueryRow is functional; the rest panic.
type fakeTx struct{ id string }

func newFakeTx(id string) pgx.Tx { return &fakeTx{id: id} }

func (t *fakeTx) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return fakeRow{id: t.id}
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

func (t *fakeTx) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	panic("fakeTx: Exec not implemented")
}

func (t *fakeTx) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	panic("fakeTx: Query not implemented")
}

func (t *fakeTx) Conn() *pgx.Conn {
	return nil
}
