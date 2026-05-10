package approval

import (
	"context"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// SpawnKickoff is the seam the dispatcher consults on the `approved`
// branch when the resolved tool name is `propose_spawn`. The interface
// is the spawn-saga entrypoint: the dispatcher hands the kickoffer the
// freshly-minted saga id, the manifest_version_id, the freshly-minted
// watchkeeper id, the Watchmaster claim, and the approval token; the
// kickoffer composes the audit event, persists the saga row, seeds a
// [saga.SpawnContext] on `ctx`, and runs the saga (with the
// kickoffer's construction-time-configured step list — empty in
// M7.1.b, populated in M7.1.c–.e).
//
// Defined in the `approval` package alongside [Replayer] so the
// dispatcher can branch on tool name without pulling spawn-saga state
// into approval-package fixtures (mirrors the M6.3.b [Replayer] seam
// pattern). Production wiring composes [spawn.SpawnKickoffer] behind
// this seam.
//
// IMPORTANT: a non-nil error return DOES NOT trigger a DAO rollback.
// The dispatcher emits `approval_replay_failed` and surfaces the error
// on the returned audit chain; the operator retries via a fresh
// approval flow (mirrors the M6.3.b [Replayer] error policy).
type SpawnKickoff interface {
	// Kickoff seeds the spawn saga and runs it through the
	// construction-time-configured step list, OR short-circuits
	// when the supplied `approvalToken` already names a persisted
	// saga (the M7.3.a idempotency replay contract).
	//
	// Implementations MUST persist the saga row keyed by
	// `approvalToken` (idempotency_key) BEFORE running the saga;
	// MUST emit either `manifest_approved_for_spawn` (insert path)
	// OR `manifest_approval_replayed_for_spawn` (replay path) — but
	// NEVER both for a single kickoff; MUST seed a
	// [saga.SpawnContext] on `ctx` for the insert path AND for the
	// M7.3.a `pending`-status catch-up path; MUST NOT call the
	// saga runner on the replay path of an already-advanced saga.
	//
	// M7.1.b → M7.3.a invariant shift: pre-M7.3.a implementations
	// emitted the audit row BEFORE the persistence call so the
	// audit chain was canonical even on a transient Insert error.
	// M7.3.a inverts the dependency at the kickoffer surface
	// because the event_type depends on insert-vs-replay (which
	// only the DAO knows). A transient persistence error
	// short-circuits with a wrapped error and NO audit row; the
	// dispatcher's `approval_replay_failed` row covers the
	// operator-visible failure surface.
	Kickoff(
		ctx context.Context,
		sagaID uuid.UUID,
		manifestVersionID uuid.UUID,
		watchkeeperID uuid.UUID,
		claim saga.SpawnClaim,
		approvalToken string,
	) error
}
