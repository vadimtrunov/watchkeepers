// dao.go defines the [SpawnSagaDAO] persistence contract, the [Saga]
// row projection, and the typed sentinels callers branch on. The
// in-memory implementation lives in `dao_memory.go`; a Postgres-backed
// adapter is deferred to M7.1.b per the M6.3.b "ship in-memory DAO +
// tests with consumer" lesson.
package saga

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// SagaState is the closed-set vocabulary of states a `spawn_sagas` row
// can occupy. Hoisted to typed string constants so the DAO impl, the
// [Runner] state-machine, and the SQL CHECK constraint share a single
// source of truth.
//
// Project convention: snake_case strings on the wire and in payloads.
//
// The `Saga` prefix on a type in package `saga` is a deliberate naming
// choice tied to AC1 — the M7.1.a TASK pins the exported name as
// `SagaState`. The linter's stutter heuristic is silenced here
// because the closed-set state vocabulary is the package's
// load-bearing public surface; renaming to `State` would force every
// downstream consumer to alias on import.
//
//nolint:revive // AC1 pins the exported name; see comment above.
type SagaState string

// Closed-set values for [SagaState]. Matches the SQL CHECK constraint
// in `deploy/migrations/019_spawn_sagas.sql`.
const (
	// SagaStatePending is the initial state, set by [SpawnSagaDAO.Insert].
	// The [Runner] transitions out of this state on its first state
	// update.
	SagaStatePending SagaState = "pending"

	// SagaStateInFlight is the running state, set by
	// [SpawnSagaDAO.UpdateStep] before each [Step.Execute] call.
	SagaStateInFlight SagaState = "in_flight"

	// SagaStateCompleted is the terminal success state, set by
	// [SpawnSagaDAO.MarkCompleted] after the last step succeeds.
	SagaStateCompleted SagaState = "completed"

	// SagaStateFailed is the terminal failure state, set by
	// [SpawnSagaDAO.MarkFailed] after a step's [Step.Execute] returns
	// non-nil.
	SagaStateFailed SagaState = "failed"
)

// ErrSagaNotFound is the typed error [SpawnSagaDAO.Get] returns when
// the supplied id does not match any row. Matchable via [errors.Is].
// Distinct from the underlying SQL miss (`sql.ErrNoRows`) so the
// [Runner]'s fail-fast branch can emit `reason=unknown_saga` without
// parsing the underlying error.
var ErrSagaNotFound = errors.New("saga: spawn saga not found")

// ErrEmptyIdempotencyKey is the typed error [SpawnSagaDAO.InsertIfAbsent]
// returns when the supplied `idempotencyKey` is the empty string OR
// whitespace-only (the DAO normalises whitespace-only keys to empty
// before checking). Matchable via [errors.Is]. The empty key would
// silently bypass the partial UNIQUE-WHERE-NOT-NULL index on
// `spawn_sagas.idempotency_key` (Postgres treats multiple NULLs as
// distinct under a partial unique index) and double-create on retry;
// callers that have no idempotency_key MUST use [SpawnSagaDAO.Insert]
// instead. Mirrors the "fail-fast precedes audit / state side effect"
// discipline established in M7.1.b (see
// `core/pkg/spawn/spawnkickoff.go`).
var ErrEmptyIdempotencyKey = errors.New("saga: empty idempotency_key")

// ErrIdempotencyIndexInconsistent is the typed error
// [SpawnSagaDAO.InsertIfAbsent] returns when its idempotency-key
// index references a sagaID that no longer matches a row in the
// rows store. Surfaces a future-DAO inconsistency (e.g. a Postgres
// adapter race where an FK cleanup raced the index lookup) so the
// kickoffer's wrap chain stays typed all the way down. Matchable
// via [errors.Is]. The in-memory DAO can only reach this branch if
// a caller bypasses the public surface and mutates the rows map
// directly; production callers never see it.
var ErrIdempotencyIndexInconsistent = errors.New("saga: idempotency index references missing row")

// Saga is the projection of a single `spawn_sagas` row the DAO surface
// returns from [SpawnSagaDAO.Get]. Exported so future M7.1.b callers
// can read the persisted state without a re-query during recovery.
//
// All fields are typed (no `interface{}`) per AC6.
type Saga struct {
	// ID is the row's primary key — the same `uuid.UUID` the caller
	// supplied to [SpawnSagaDAO.Insert].
	ID uuid.UUID

	// ManifestVersionID is the manifest_version this saga is spawning.
	// Stored at insert time and never mutated by the saga; the eventual
	// runtime intro step (M7.1.e) reads it back.
	ManifestVersionID uuid.UUID

	// Status is the row's current state. One of the [SagaState]
	// constants.
	Status SagaState

	// CurrentStep is the [Step.Name] of the most recently invoked step.
	// Empty string before the first step runs and after a saga with
	// zero steps completes.
	CurrentStep string

	// LastError is the failure-reason sentinel passed into
	// [SpawnSagaDAO.MarkFailed]. Empty string when `Status` is not
	// [SagaStateFailed]. Per the PII discipline, this carries the
	// [Step.Execute]'s error class (a stable sentinel string), not
	// the underlying error message or stack trace.
	LastError string

	// CreatedAt is the wall-clock time the row was inserted; set by
	// the SQL DEFAULT now() at insert time. Populated by the DAO impl
	// from the `created_at` column.
	CreatedAt time.Time

	// UpdatedAt is the wall-clock time the row was last mutated; set
	// by the DAO on every UpdateStep / MarkCompleted / MarkFailed.
	UpdatedAt time.Time

	// CompletedAt is the wall-clock time the row reached a terminal
	// state ([SagaStateCompleted] or [SagaStateFailed]). Zero value
	// (`time.Time{}`) while the saga is still in-flight.
	CompletedAt time.Time

	// IdempotencyKey is the M7.3.a opaque dedup key supplied by the
	// caller through [SpawnSagaDAO.InsertIfAbsent]. Empty string when
	// the row was inserted via the legacy [SpawnSagaDAO.Insert] path
	// (zero-step smoke wiring, pre-M7.3.a-deployed rows). Stored on
	// the row at insert time and never mutated by subsequent state
	// transitions — the key is the saga's "we have committed to
	// running this exactly-once" marker, so a future replay
	// short-circuits regardless of the saga's terminal state. The
	// kickoffers consult `IdempotencyKey` via the DAO's idempotency
	// index, not via direct row reads; the field is exported so
	// future M7.3.b/.c compensator paths can correlate replay rows
	// to saga rows by key without an extra DAO method.
	IdempotencyKey string

	// WatchkeeperID is the M7.3.a saga's target watchkeeper id (=
	// [SpawnContext.AgentID]). Stored on the row at insert time
	// (via [SpawnSagaDAO.InsertIfAbsent]) so a replay-event payload
	// can emit the FIRST-call's watchkeeperID instead of a discarded
	// second-call candidate id. Empty (`uuid.Nil`) when the row was
	// inserted via the legacy [SpawnSagaDAO.Insert] path. Read by
	// the kickoffer on the M7.3.a `pending`-status catch-up branch
	// to seed [SpawnContext.AgentID] for the resumed saga.
	WatchkeeperID uuid.UUID
}

// IdempotentInsertResult is the typed return value of
// [SpawnSagaDAO.InsertIfAbsent]. The closed-set shape lets a caller
// branch on the `Inserted` boolean without parsing an error chain;
// the `Existing` field always carries the row that won the race
// (the freshly-inserted row when `Inserted` is true, the prior row
// when `Inserted` is false).
//
// Holding the result in a struct (rather than a `(Saga, bool, error)`
// triple) keeps the call site readable and lets a future addition
// (e.g. a `Conflicted bool` for the same-id-different-key wiring-bug
// branch) land as a new field without breaking the signature.
type IdempotentInsertResult struct {
	// Inserted is true when the call persisted a fresh row, false
	// when an existing row's `idempotency_key` matched the supplied
	// key (the replay path).
	Inserted bool

	// Existing is the row that won the race: the freshly-inserted
	// row on the insert path, the prior row on the replay path. The
	// caller branches on `Existing.Status` to decide what audit
	// event to emit (`*_approved_*` on insert, `*_replayed_*` on
	// replay).
	Existing Saga
}

// SpawnSagaDAO is the persistence seam for the spawn-saga state
// machine. The interface is the unit-test seam: the production impl
// will be a Postgres-backed adapter (deferred to M7.1.b); the M7.1.a
// [Runner] tests use the [MemorySpawnSagaDAO] in-memory implementation
// shipped alongside.
//
// All methods are safe for concurrent use across goroutines on the
// same DAO value; per-call state lives on the goroutine stack.
//
// Resolution discipline: `Insert` is idempotent on `id` only insofar
// as a duplicate insert is the caller's bug — the DAO surfaces a
// wrapped error rather than silently swallowing. `MarkCompleted` and
// `MarkFailed` do not enforce a state-machine check at this layer
// (the [Runner] is the sole caller in M7.1.a; future direct callers
// would need to read [Saga.Status] first).
type SpawnSagaDAO interface {
	// Insert persists a new row with `Status = pending`, the supplied
	// `manifestVersionID`, an empty `IdempotencyKey`, and zero values
	// for the remaining mutable fields. Returns a wrapped error on
	// duplicate `id` (caller bug — the [Runner] mints a fresh UUID
	// per saga). Callers that need idempotent retry semantics MUST
	// use [SpawnSagaDAO.InsertIfAbsent] instead.
	Insert(ctx context.Context, id uuid.UUID, manifestVersionID uuid.UUID) error

	// InsertIfAbsent persists a new row keyed by `idempotencyKey` OR
	// returns the existing row when the key is already taken. The
	// returned [IdempotentInsertResult] tells the caller which
	// branch fired:
	//
	//   - `Inserted == true`  — fresh row persisted with the supplied
	//     `id` / `manifestVersionID` / `watchkeeperID` /
	//     `idempotencyKey`. The `Existing` field carries the
	//     freshly-inserted row.
	//   - `Inserted == false` — a row with the same `idempotencyKey`
	//     already exists. The supplied `id` and `watchkeeperID` are
	//     discarded; `Existing` carries the prior row so the caller
	//     can branch on its `Status` (e.g. emit a `*_replayed_*`
	//     audit event without re-running the saga, or resume a
	//     stuck `pending` saga via the M7.3.a catch-up path).
	//
	// `idempotencyKey` MUST be non-empty AND non-whitespace; an
	// empty / whitespace-only key returns [ErrEmptyIdempotencyKey]
	// without any persistence side effect (the partial
	// UNIQUE-WHERE-NOT-NULL index treats NULLs as distinct, so an
	// empty-key bypass would silently double-create on retry).
	//
	// `watchkeeperID` MUST be non-empty; persisting an empty value
	// would defeat the M7.3.a replay-payload contract (the
	// kickoffer reads `Existing.WatchkeeperID` on the catch-up path
	// to seed [SpawnContext.AgentID]). Rejected with the same
	// `empty watchkeeperID` shape as [Insert]'s validations.
	//
	// Caller bug surface (returned wrapped, not silently swallowed):
	//   - empty `id`: rejected with the same shape as [Insert].
	//   - empty `manifestVersionID`: rejected with the same shape as
	//     [Insert].
	//   - empty `watchkeeperID`: rejected.
	//   - duplicate `id` paired with a DIFFERENT `idempotencyKey`:
	//     rejected (the caller is double-using a sagaID — a wiring
	//     bug). Same-id-with-same-key is the replay path and returns
	//     `Inserted == false` cleanly.
	InsertIfAbsent(
		ctx context.Context,
		id uuid.UUID,
		manifestVersionID uuid.UUID,
		watchkeeperID uuid.UUID,
		idempotencyKey string,
	) (IdempotentInsertResult, error)

	// Get returns the row matching `id` or [ErrSagaNotFound] when no
	// such row exists. The returned [Saga] is a value copy; mutating
	// it does not affect the persisted row.
	Get(ctx context.Context, id uuid.UUID) (Saga, error)

	// UpdateStep transitions the row to `Status = in_flight` and
	// records `step` as the new `CurrentStep`. Called by the [Runner]
	// BEFORE each [Step.Execute] so a crash mid-execute leaves the
	// row pointing at the step that was running. Returns
	// [ErrSagaNotFound] when no such row exists.
	UpdateStep(ctx context.Context, id uuid.UUID, step string) error

	// MarkCompleted transitions the row to `Status = completed` and
	// stamps `CompletedAt`. Called by the [Runner] after the last
	// step's [Step.Execute] succeeds (or immediately for a zero-step
	// saga). Returns [ErrSagaNotFound] when no such row exists.
	MarkCompleted(ctx context.Context, id uuid.UUID) error

	// MarkFailed transitions the row to `Status = failed`, records
	// `lastErr` as the failure-reason sentinel, and stamps
	// `CompletedAt`. Called by the [Runner] when a [Step.Execute]
	// returns non-nil. The `lastErr` value is the failure's stable
	// sentinel string (e.g. "step_execute_error"), NOT the underlying
	// error message — payload PII discipline lives in the [Runner],
	// the DAO is the persistence sink. Returns [ErrSagaNotFound] when
	// no such row exists.
	MarkFailed(ctx context.Context, id uuid.UUID, lastErr string) error
}
