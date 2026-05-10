package saga

import "github.com/google/uuid"

// PurgeRowKeepingIdempotencyForTest deletes the row keyed by `id`
// from the in-memory store while LEAVING the idempotency-key index
// pointing at it. The function is exported only via this `_test.go`
// file (so it is invisible to non-test consumers of the package) and
// exists solely to reproduce the
// [ErrIdempotencyIndexInconsistent] branch in
// [MemorySpawnSagaDAO.InsertIfAbsent] without requiring a contrived
// DAO subclass. Production callers cannot reach this branch through
// the public surface; the test pins the typed sentinel so a future
// Postgres adapter that surfaces the same inconsistency under FK
// race conditions stays compatible with `errors.Is`.
func PurgeRowKeepingIdempotencyForTest(dao *MemorySpawnSagaDAO, id uuid.UUID) {
	dao.mu.Lock()
	defer dao.mu.Unlock()
	delete(dao.rows, id)
}
