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
	// construction-time-configured step list. Implementations MUST
	// emit a `manifest_approved_for_spawn` audit event BEFORE
	// inserting the saga row, MUST insert the saga row BEFORE
	// calling the saga runner, and MUST seed a [saga.SpawnContext]
	// on `ctx` carrying `manifestVersionID` / `watchkeeperID` /
	// `claim` so the registered steps can resolve the saga's
	// per-call values.
	Kickoff(
		ctx context.Context,
		sagaID uuid.UUID,
		manifestVersionID uuid.UUID,
		watchkeeperID uuid.UUID,
		claim saga.SpawnClaim,
		approvalToken string,
	) error
}
