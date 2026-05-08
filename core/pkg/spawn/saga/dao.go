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
	// `manifestVersionID`, and zero values for the remaining mutable
	// fields. Returns a wrapped error on duplicate `id` (caller bug —
	// the [Runner] mints a fresh UUID per saga).
	Insert(ctx context.Context, id uuid.UUID, manifestVersionID uuid.UUID) error

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
