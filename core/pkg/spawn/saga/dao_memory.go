// dao_memory.go ships the in-memory [SpawnSagaDAO] implementation used
// by the M7.1.a [Runner] tests AND (intentionally) by callers that want
// a zero-dep saga store for dev / smoke runs without a Postgres-backed
// adapter wired up.
//
// The store is goroutine-safe: a single per-instance read/write mutex
// guards the underlying map. Throughput is irrelevant — production
// callers wire a real adapter; this type exists so the test surface
// and the dev-loop wiring share one implementation.
package saga

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemorySpawnSagaDAO is a process-local [SpawnSagaDAO] implementation
// backed by a `map[uuid.UUID]Saga`. Constructed via
// [NewMemorySpawnSagaDAO]; the zero value is NOT usable — callers must
// always go through the constructor so the internal map is non-nil.
//
// `now` is overridable so unit tests can drive deterministic
// `created_at` / `updated_at` / `completed_at` values without a fixture
// clock threaded through the Insert / Update / Mark call sites.
//
// Concurrency: read methods (`Get`) take an RLock so concurrent reads
// across distinct ids never block each other. Write methods
// (`Insert`, `InsertIfAbsent`, `UpdateStep`, `MarkCompleted`,
// `MarkFailed`) take the write lock for the duration of the call.
type MemorySpawnSagaDAO struct {
	mu   sync.RWMutex
	rows map[uuid.UUID]Saga
	// idempotency indexes the non-empty `IdempotencyKey` values to
	// the sagaID that owns them. Mirrors the partial UNIQUE
	// `WHERE idempotency_key IS NOT NULL` index defined in
	// `deploy/migrations/023_spawn_sagas_idempotency_key.sql` — empty
	// keys are NEVER inserted into this map (legacy [Insert]-path
	// rows have empty `IdempotencyKey` so they cannot collide). All
	// reads + writes are guarded by `mu` so a concurrent
	// [InsertIfAbsent] race exactly mirrors the SQL UNIQUE behaviour
	// (one INSERT wins, the rest read the existing row).
	idempotency map[string]uuid.UUID
	now         func() time.Time
}

// NewMemorySpawnSagaDAO returns an empty in-memory store. The optional
// `now` argument overrides the wall-clock used to stamp `created_at` /
// `updated_at` / `completed_at`; pass nil to use [time.Now].
func NewMemorySpawnSagaDAO(now func() time.Time) *MemorySpawnSagaDAO {
	if now == nil {
		now = time.Now
	}
	return &MemorySpawnSagaDAO{
		rows:        make(map[uuid.UUID]Saga),
		idempotency: make(map[string]uuid.UUID),
		now:         now,
	}
}

// Compile-time assertion: [*MemorySpawnSagaDAO] satisfies
// [SpawnSagaDAO] (AC6). Pins the integration shape so a future change
// to the interface surface fails the build here.
var _ SpawnSagaDAO = (*MemorySpawnSagaDAO)(nil)

// Insert satisfies [SpawnSagaDAO.Insert]. Returns a wrapped error on
// duplicate `id` (caller bug — the [Runner] mints a fresh UUID per
// saga).
func (m *MemorySpawnSagaDAO) Insert(_ context.Context, id uuid.UUID, manifestVersionID uuid.UUID) error {
	if id == uuid.Nil {
		return fmt.Errorf("saga: insert: empty id")
	}
	if manifestVersionID == uuid.Nil {
		return fmt.Errorf("saga: insert: empty manifest_version_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.rows[id]; exists {
		return fmt.Errorf("saga: insert: duplicate id %q", id)
	}
	now := m.now().UTC()
	m.rows[id] = Saga{
		ID:                id,
		ManifestVersionID: manifestVersionID,
		Status:            SagaStatePending,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	return nil
}

// InsertIfAbsent satisfies [SpawnSagaDAO.InsertIfAbsent]. Mirrors the
// partial-UNIQUE-WHERE-NOT-NULL semantics of the
// `spawn_sagas_idempotency_key_uniq` index: a concurrent collision on
// `idempotencyKey` resolves to the existing row (no error); a concurrent
// collision on `id` paired with a DIFFERENT `idempotencyKey` is a
// caller wiring bug and surfaces as a wrapped error.
//
// Whitespace-only `idempotencyKey` values normalise to empty before
// the [ErrEmptyIdempotencyKey] check, so a `"   "` sentinel cannot
// smuggle a bypass past the dedup index. The DAO does NOT silently
// trim the key for storage — a non-whitespace key is stored verbatim.
func (m *MemorySpawnSagaDAO) InsertIfAbsent(
	_ context.Context,
	id uuid.UUID,
	manifestVersionID uuid.UUID,
	watchkeeperID uuid.UUID,
	idempotencyKey string,
) (IdempotentInsertResult, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return IdempotentInsertResult{}, ErrEmptyIdempotencyKey
	}
	if id == uuid.Nil {
		return IdempotentInsertResult{}, fmt.Errorf("saga: insert if absent: empty id")
	}
	if manifestVersionID == uuid.Nil {
		return IdempotentInsertResult{}, fmt.Errorf("saga: insert if absent: empty manifest_version_id")
	}
	if watchkeeperID == uuid.Nil {
		return IdempotentInsertResult{}, fmt.Errorf("saga: insert if absent: empty watchkeeper_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existingID, exists := m.idempotency[idempotencyKey]; exists {
		row, ok := m.rows[existingID]
		if !ok {
			// Index inconsistency — the key map points at a row that
			// no longer exists. Surface a typed sentinel rather than
			// silently re-inserting; a missing row under a held key
			// is a caller bug (e.g. mutating `rows` directly
			// bypassing the DAO surface) that the test suite pins
			// via [errors.Is].
			return IdempotentInsertResult{}, fmt.Errorf("%w: %q", ErrIdempotencyIndexInconsistent, existingID)
		}
		return IdempotentInsertResult{Inserted: false, Existing: row}, nil
	}
	if _, exists := m.rows[id]; exists {
		// Same id supplied with a fresh idempotency_key — the caller
		// is double-using a sagaID under two different dedup keys,
		// which the SQL `id` PRIMARY KEY would reject. Mirrors the
		// shape returned by [Insert] on duplicate id.
		return IdempotentInsertResult{}, fmt.Errorf("saga: insert if absent: duplicate id %q", id)
	}
	now := m.now().UTC()
	row := Saga{
		ID:                id,
		ManifestVersionID: manifestVersionID,
		Status:            SagaStatePending,
		CreatedAt:         now,
		UpdatedAt:         now,
		IdempotencyKey:    idempotencyKey,
		WatchkeeperID:     watchkeeperID,
	}
	m.rows[id] = row
	m.idempotency[idempotencyKey] = id
	return IdempotentInsertResult{Inserted: true, Existing: row}, nil
}

// Get satisfies [SpawnSagaDAO.Get]. Returns a value copy so a mutating
// caller cannot race the in-place row.
func (m *MemorySpawnSagaDAO) Get(_ context.Context, id uuid.UUID) (Saga, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	row, ok := m.rows[id]
	if !ok {
		return Saga{}, ErrSagaNotFound
	}
	return row, nil
}

// UpdateStep satisfies [SpawnSagaDAO.UpdateStep]. Transitions the row
// to `in_flight` and records the supplied step name. Returns
// [ErrSagaNotFound] when no such row exists.
func (m *MemorySpawnSagaDAO) UpdateStep(_ context.Context, id uuid.UUID, step string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[id]
	if !ok {
		return ErrSagaNotFound
	}
	row.Status = SagaStateInFlight
	row.CurrentStep = step
	row.UpdatedAt = m.now().UTC()
	m.rows[id] = row
	return nil
}

// MarkCompleted satisfies [SpawnSagaDAO.MarkCompleted]. Transitions
// the row to `completed` and stamps `CompletedAt`. Returns
// [ErrSagaNotFound] when no such row exists.
func (m *MemorySpawnSagaDAO) MarkCompleted(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[id]
	if !ok {
		return ErrSagaNotFound
	}
	now := m.now().UTC()
	row.Status = SagaStateCompleted
	row.UpdatedAt = now
	row.CompletedAt = now
	m.rows[id] = row
	return nil
}

// MarkFailed satisfies [SpawnSagaDAO.MarkFailed]. Transitions the row
// to `failed`, records `lastErr` as the failure sentinel, and stamps
// `CompletedAt`. Returns [ErrSagaNotFound] when no such row exists.
func (m *MemorySpawnSagaDAO) MarkFailed(_ context.Context, id uuid.UUID, lastErr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[id]
	if !ok {
		return ErrSagaNotFound
	}
	now := m.now().UTC()
	row.Status = SagaStateFailed
	row.LastError = lastErr
	row.UpdatedAt = now
	row.CompletedAt = now
	m.rows[id] = row
	return nil
}
