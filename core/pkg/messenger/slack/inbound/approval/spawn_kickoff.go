package approval

import (
	"context"

	"github.com/google/uuid"
)

// SpawnKickoff is the seam the dispatcher consults on the `approved`
// branch when the resolved tool name is `propose_spawn`. The interface
// is the spawn-saga entrypoint: the dispatcher hands the kickoffer the
// freshly-minted saga id, the manifest_version_id, and the approval
// token; the kickoffer composes the audit event, persists the saga
// row, and runs the saga (with an empty step list in M7.1.b — concrete
// steps land in M7.1.c–.e).
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
	// Kickoff seeds the spawn saga and runs it with an empty step list.
	// Implementations MUST emit a `manifest_approved_for_spawn` audit
	// event BEFORE inserting the saga row, and MUST insert the saga
	// row BEFORE calling the saga runner (M7.1.a "emit-before-state-write"
	// + "Insert-before-Run" patterns).
	Kickoff(ctx context.Context, sagaID uuid.UUID, manifestVersionID uuid.UUID, approvalToken string) error
}
