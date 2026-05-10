// retirekickoff.go is the M7.2.a production implementation of the
// retire-saga kickoff seam. The kickoffer is the bridge between the
// future retire-flow trigger (M7.2.c will wire the M6.2.c
// `retire_watchkeeper` Watchmaster tool through this seam) and the
// M7.1.a saga runner: when a retire request becomes lead-approved, a
// caller hands a kickoffer the freshly-minted saga id, the
// manifest_version id pinned by the watchkeeper's active manifest, the
// watchkeeper id targeted for retirement, the Watchmaster's claim, and
// the approval token; the kickoffer composes the audit
// `retire_approved_for_watchkeeper` event, persists the saga row via
// [saga.SpawnSagaDAO.Insert], seeds a [saga.SpawnContext] on the
// per-call `context.Context`, and runs the saga via [saga.Runner.Run]
// with the construction-time configured step list (M7.2.b
// NotebookArchive + M7.2.c MarkRetired land in their own milestones).
//
// # Audit-emit-before-state-write
//
// Mirrors the M7.1.b SpawnKickoff invariant: the
// `retire_approved_for_watchkeeper` row is the canonical "we tried"
// signal, emitted BEFORE any persistence or saga-run side effect. A
// downstream Insert / Run failure surfaces as a wrapped error, but
// the audit chain remains the authoritative event-source.
//
// # Fail-fast precedes audit (M7.1.c.c lesson)
//
// `uuid.Nil` sagaID / manifestVersionID / watchkeeperID are rejected
// with [ErrInvalidKickoffArgs] BEFORE the audit Append + DAO Insert.
// A malformed kickoff input is a programmer / wiring bug, not a
// runtime fault; leaving NO audit row and NO persisted state on this
// path keeps the audit chain honest.
//
// # PII discipline
//
// Closed-set audit payload keys: `manifest_version_id`,
// `watchkeeper_id`, `approval_token_prefix`, `agent_id`. NO full
// approval token, NO error string, NO step-internal params. The
// approval token is rendered as the `tok-<first-6-chars>` prefix
// per the M6.3.b token-prefix-display lesson.
package spawn

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// retireLogAppender is the minimal subset of [keeperslog.Writer] the
// retire kickoffer consumes — only [keeperslog.Writer.Append].
// Re-declared locally (rather than reusing [kickoffLogAppender] from
// spawnkickoff.go) so a reviewer reading retirekickoff.go in isolation
// sees the contract without cross-file lookup. Mirrors the M7.1.b
// "local seam re-declaration over cross-file unexported interface"
// lesson; the drift-pin assertion below catches signature drift
// between the sibling seams.
type retireLogAppender interface {
	Append(ctx context.Context, evt keeperslog.Event) (string, error)
}

// Compile-time drift-pins between [retireLogAppender] (this file) and
// [kickoffLogAppender] (spawnkickoff.go). Both directions are pinned
// so adding a method to either interface AS-A-SHADOW (without the
// other) breaks the build:
//
//   - retireLogAppender ⊇ kickoffLogAppender catches a NARROWING of
//     kickoffLogAppender that drops a method retire still requires.
//   - kickoffLogAppender ⊇ retireLogAppender catches an EXPANSION of
//     retireLogAppender that adds a method spawn does not yet supply.
//
// The bidirectional assertion is the M7.2.a iter-1 strengthening of
// the M7.1.b "local seam re-declaration" lesson.
var (
	_ retireLogAppender  = kickoffLogAppender(nil)
	_ kickoffLogAppender = retireLogAppender(nil)
)

// EventTypeRetireApprovedForWatchkeeper is the M7.2.a audit event type
// the retire kickoffer emits AFTER persisting a freshly-inserted saga
// row via [saga.SpawnSagaDAO.InsertIfAbsent]. Hoisted to a constant so
// the payload-shape regression test pins the wire vocabulary AND so a
// downstream consumer can match by string equality without a typo
// risk.
//
// Distinct prefix (`retire_`) so it does NOT collide with the
// `manifest_*` family established in M7.1.b, the `saga_*` family
// established in M7.1.a, the `llm_turn_cost_*` family established in
// M6.3.e, the `notebook_*` family established in M2b, or the
// `watchmaster_retire_watchkeeper_*` family established in M6.2.c
// (the existing synchronous retire tool's audit chain — a separate
// vocabulary so the M6.2.c synchronous path and the M7.2 saga path
// remain distinguishable on the audit chain).
const EventTypeRetireApprovedForWatchkeeper = "retire_approved_for_watchkeeper"

// EventTypeRetireApprovalReplayedForWatchkeeper is the M7.3.a audit
// event type the retire kickoffer emits when the supplied
// approval_token's idempotency_key already names a row (the upstream
// retire approval was resubmitted). Distinct from
// `retire_approved_for_watchkeeper` so a downstream consumer can
// branch on the wire `event_type` without parsing payload structure.
//
// The payload carries the SAME closed-set keys as
// `retire_approved_for_watchkeeper` plus a `saga_id` (the existing
// saga's id) and a `previous_status` key naming the existing saga's
// [saga.SagaState] at replay time.
const EventTypeRetireApprovalReplayedForWatchkeeper = "retire_approval_replayed_for_watchkeeper"

// Closed-set audit payload keys for the retire kickoff event. Hoisted
// to constants so the payload-shape regression test pins the wire
// vocabulary (M2b.7 PII discipline). The kickoffer is the SOLE
// composer of this payload.
const (
	retireKickoffPayloadKeyManifestVersionID   = "manifest_version_id"
	retireKickoffPayloadKeyWatchkeeperID       = "watchkeeper_id"
	retireKickoffPayloadKeyApprovalTokenPrefix = "approval_token_prefix"
	retireKickoffPayloadKeyAgentID             = "agent_id"
	retireKickoffPayloadKeySagaID              = "saga_id"
	retireKickoffPayloadKeyPreviousStatus      = "previous_status"
)

// RetireKickoffer is the production implementation of the retire-saga
// kickoff seam. Construct via [NewRetireKickoffer]; the zero value is
// NOT usable.
//
// Concurrency: safe for concurrent use after construction. Holds only
// immutable configuration; per-call state lives on the goroutine
// stack. Concurrent [RetireKickoffer.Kickoff] calls on distinct saga
// ids never block each other beyond a normal map read/write inside
// the DAO.
type RetireKickoffer struct {
	logger  retireLogAppender
	dao     saga.SpawnSagaDAO
	runner  *saga.Runner
	agentID string
	// steps is the M7.2.b/.c step list registered at construction
	// time. May be nil / empty — the runner treats nil as an empty
	// slice and the saga completes immediately with a single
	// `saga_completed` audit event (matches the M7.1.b zero-step
	// behaviour). Production wiring populates this with
	// [NotebookArchive, MarkRetired] in that order over the M7.2.b–.c
	// milestones.
	steps []saga.Step
}

// RetireKickoffDeps is the construction-time bag wired into
// [NewRetireKickoffer]. Held in a struct so a future addition (e.g. a
// clock, a tracer) lands as a new field without breaking the
// constructor signature.
type RetireKickoffDeps struct {
	// Logger is the audit-emit seam. Required; a nil Logger is
	// rejected at construction. [*keeperslog.Writer] satisfies the
	// seam in production.
	Logger retireLogAppender

	// DAO is the saga-persistence seam. Required; a nil DAO is
	// rejected at construction. Reuses the M7.1.a [saga.SpawnSagaDAO]
	// (the saga state machine is generic; the audit event_type
	// distinguishes spawn vs retire semantics, not the DAO surface).
	DAO saga.SpawnSagaDAO

	// Runner is the saga-runner seam. Required; a nil Runner is
	// rejected at construction. The kickoffer calls
	// [saga.Runner.Run] with the construction-time-configured
	// [RetireKickoffDeps.Steps] slice seeded with a per-saga
	// [saga.SpawnContext] on `ctx`.
	Runner *saga.Runner

	// AgentID is the bot's stable agent identifier emitted on every
	// `retire_approved_for_watchkeeper` audit row. Empty values are
	// rejected at construction so a downstream consumer's `agent_id`
	// query never silently returns rows with no owner.
	AgentID string

	// Steps is the saga step list the kickoffer hands to
	// [saga.Runner.Run] on every Kickoff. Optional — a nil / empty
	// slice keeps the M7.1.b zero-step behaviour (the saga completes
	// immediately with a single `saga_completed` audit event). The
	// kickoffer takes a defensive copy at construction time so a
	// post-construction mutation of the caller's slice does not
	// affect saga runs.
	//
	// Production wiring populates this with the M7.2.b NotebookArchive
	// step + M7.2.c MarkRetired step (in that order) when those land.
	Steps []saga.Step
}

// NewRetireKickoffer constructs a [RetireKickoffer] with the supplied
// [RetireKickoffDeps]. Logger, DAO, Runner, and AgentID are required;
// a nil/empty value for any of them panics with a clear message —
// matches the panic discipline of [keeperslog.New], [saga.NewRunner],
// and [NewSpawnKickoffer]. Steps is optional (nil / empty produces a
// zero-step saga matching the M7.1.b behaviour).
func NewRetireKickoffer(deps RetireKickoffDeps) *RetireKickoffer {
	if deps.Logger == nil {
		panic("spawn: NewRetireKickoffer: deps.Logger must not be nil")
	}
	if deps.DAO == nil {
		panic("spawn: NewRetireKickoffer: deps.DAO must not be nil")
	}
	if deps.Runner == nil {
		panic("spawn: NewRetireKickoffer: deps.Runner must not be nil")
	}
	if deps.AgentID == "" {
		panic("spawn: NewRetireKickoffer: deps.AgentID must not be empty")
	}
	steps := append([]saga.Step(nil), deps.Steps...)
	return &RetireKickoffer{
		logger:  deps.Logger,
		dao:     deps.DAO,
		runner:  deps.Runner,
		agentID: deps.AgentID,
		steps:   steps,
	}
}

// Kickoff seeds the retire saga and runs it through the
// construction-time-registered step list, OR short-circuits with a
// `retire_approval_replayed_for_watchkeeper` audit event when the
// supplied `approvalToken` already names a persisted saga past
// `pending` (the M7.3.a idempotency replay path), OR resumes a
// `pending` saga whose original audit-append failed mid-flight (the
// M7.3.a catch-up path; mirrors the spawn-side codex iter-1 Critical
// fix).
//
// Sequence (load-bearing — the order is pinned by an ordering test):
//
//  1. Fail-fast validation rejects `uuid.Nil` sagaID /
//     manifestVersionID / watchkeeperID and an empty / whitespace-
//     only `approvalToken` with [ErrInvalidKickoffArgs] BEFORE any
//     audit-emit / state-write side effect.
//  2. Call [saga.SpawnSagaDAO.InsertIfAbsent] with `approvalToken`
//     as the idempotency_key, persisting `watchkeeperID` so the
//     replay-payload contract can emit the FIRST-call's id.
//  3. On INSERT or CATCH-UP-ON-PENDING: emit
//     `retire_approved_for_watchkeeper` (using the FIRST-call's ids
//     via `result.Existing` — same ids as the new call on the
//     insert path), seed [saga.SpawnContext] + a fresh
//     [saga.RetireResult] outbox via [saga.WithRetireResult], and
//     call [saga.Runner.Run] for the existing saga id.
//  4. On REPLAY of an ALREADY-RUN saga: emit
//     `retire_approval_replayed_for_watchkeeper` carrying the
//     FIRST-call's `saga_id` / `manifest_version_id` /
//     `watchkeeper_id` / `previous_status`, then return nil.
//
// Errors are wrapped with the `spawn:` prefix; the underlying
// keeperslog / saga sentinels remain matchable via [errors.Is]
// through the wrap chain.
func (k *RetireKickoffer) Kickoff(
	ctx context.Context,
	sagaID uuid.UUID,
	manifestVersionID uuid.UUID,
	watchkeeperID uuid.UUID,
	claim saga.SpawnClaim,
	approvalToken string,
) error {
	if sagaID == uuid.Nil {
		return fmt.Errorf("%w: empty sagaID", ErrInvalidKickoffArgs)
	}
	if manifestVersionID == uuid.Nil {
		return fmt.Errorf("%w: empty manifestVersionID", ErrInvalidKickoffArgs)
	}
	if watchkeeperID == uuid.Nil {
		return fmt.Errorf("%w: empty watchkeeperID", ErrInvalidKickoffArgs)
	}
	if strings.TrimSpace(approvalToken) == "" {
		return fmt.Errorf("%w: empty approvalToken", ErrInvalidKickoffArgs)
	}

	result, err := k.dao.InsertIfAbsent(ctx, sagaID, manifestVersionID, watchkeeperID, approvalToken)
	if err != nil {
		return fmt.Errorf("spawn: retire kickoff: insert saga: %w", err)
	}

	if !result.Inserted && result.Existing.Status != saga.SagaStatePending {
		if _, replayErr := k.logger.Append(ctx, keeperslog.Event{
			EventType: EventTypeRetireApprovalReplayedForWatchkeeper,
			Payload: retireApprovalReplayedPayload(
				result.Existing.ID,
				result.Existing.ManifestVersionID,
				result.Existing.WatchkeeperID,
				approvalToken,
				k.agentID,
				result.Existing.Status,
			),
		}); replayErr != nil {
			return fmt.Errorf("spawn: retire kickoff: append retire_approval_replayed_for_watchkeeper: %w", replayErr)
		}
		return nil
	}

	emitSagaID := result.Existing.ID
	emitManifestVersionID := result.Existing.ManifestVersionID
	emitWatchkeeperID := result.Existing.WatchkeeperID
	if _, err := k.logger.Append(ctx, keeperslog.Event{
		EventType: EventTypeRetireApprovedForWatchkeeper,
		Payload:   retireApprovedPayload(emitManifestVersionID, emitWatchkeeperID, approvalToken, k.agentID),
	}); err != nil {
		return fmt.Errorf("spawn: retire kickoff: append retire_approved_for_watchkeeper: %w", err)
	}

	// Three "agent" identifiers flow through this kickoff and are
	// deliberately distinct (M7.2.a iter-1 disambiguation):
	//
	//   - `k.agentID` — the WATCHMASTER bot id; lands on the audit
	//     payload's `agent_id` key as the EMITTER of the row.
	//   - `emitWatchkeeperID` — the RETIRE TARGET (sourced from the
	//     persisted row so a catch-up call uses the FIRST-call's id);
	//     lands on [saga.SpawnContext.AgentID] (M7.1.c.a saga
	//     convention names the watchkeeper-being-acted-on as
	//     `AgentID`) and on the audit payload's `watchkeeper_id` key.
	//   - `claim.AgentID` — the ACTING agent id (Watchmaster's claim
	//     mint); used by downstream M7.2.b/.c steps for authority
	//     gates, not by this kickoffer.
	//
	// A retire-step author who needs "the watchkeeper to retire"
	// reads [saga.SpawnContext.AgentID]; a step that emits its own
	// audit row uses its own bot identifier (NOT
	// [saga.SpawnContext.AgentID]).
	ctx = saga.WithSpawnContext(ctx, saga.SpawnContext{
		ManifestVersionID: emitManifestVersionID,
		AgentID:           emitWatchkeeperID,
		Claim:             claim,
	})

	// Seed a fresh outbox per Kickoff so concurrent retire sagas have
	// isolated [saga.RetireResult] pointers — the M7.2.b NotebookArchive
	// step writes ArchiveURI here; the M7.2.c MarkRetired step reads
	// it back. The spawn-side kickoffer has no equivalent because no
	// spawn step needs an inter-step outbox today.
	ctx = saga.WithRetireResult(ctx, &saga.RetireResult{})

	if err := k.runner.Run(ctx, emitSagaID, k.steps); err != nil {
		return fmt.Errorf("spawn: retire kickoff: run saga: %w", err)
	}
	return nil
}

// retireApprovedPayload composes the closed-set
// `retire_approved_for_watchkeeper` payload. PII guard: this function
// is the SOLE composer of the payload; if a future change adds a key,
// code review picks it up here and the wire-shape regression test
// pins it.
//
// The `approval_token` is rendered as the `tok-<first-6-chars>` prefix
// (the M6.3.b token-prefix-display lesson) so the full bearer token
// never lands on the audit chain. Reuses [approvalTokenPrefix] from
// spawnkickoff.go — both kickoffers share the same redaction shape.
func retireApprovedPayload(
	manifestVersionID uuid.UUID,
	watchkeeperID uuid.UUID,
	approvalToken, agentID string,
) map[string]any {
	return map[string]any{
		retireKickoffPayloadKeyManifestVersionID:   manifestVersionID.String(),
		retireKickoffPayloadKeyWatchkeeperID:       watchkeeperID.String(),
		retireKickoffPayloadKeyApprovalTokenPrefix: approvalTokenPrefix(approvalToken),
		retireKickoffPayloadKeyAgentID:             agentID,
	}
}

// retireApprovalReplayedPayload composes the closed-set
// `retire_approval_replayed_for_watchkeeper` payload. Carries the
// prior saga's id, watchkeeper id, manifest version, and status so a
// downstream consumer can distinguish "replayed mid-flight" from
// "replayed after terminal state" AND correlate the replay row to
// the saga's persisted target without a follow-up DAO read.
//
// All ids are sourced from [saga.IdempotentInsertResult.Existing] —
// codex iter-1 Major fix: pre-iter-1 the payload accepted the
// SECOND-call's `manifestVersionID` and `watchkeeperID` and emitted
// them verbatim, which would produce a self-contradictory row when
// a retried approval supplied different ids than the original. The
// DAO discards the second-call values; the audit emit must too.
//
// PII guard: this function is the SOLE composer of the payload; the
// closed-set keys mirror `retire_approved_for_watchkeeper` plus the
// two replay-only fields (`saga_id`, `previous_status`). NEVER
// carries the full approval token, the original step's params, or
// any error string.
func retireApprovalReplayedPayload(
	existingSagaID uuid.UUID,
	existingManifestVersionID uuid.UUID,
	existingWatchkeeperID uuid.UUID,
	approvalToken, agentID string,
	previousStatus saga.SagaState,
) map[string]any {
	return map[string]any{
		retireKickoffPayloadKeySagaID:              existingSagaID.String(),
		retireKickoffPayloadKeyManifestVersionID:   existingManifestVersionID.String(),
		retireKickoffPayloadKeyWatchkeeperID:       existingWatchkeeperID.String(),
		retireKickoffPayloadKeyApprovalTokenPrefix: approvalTokenPrefix(approvalToken),
		retireKickoffPayloadKeyAgentID:             agentID,
		retireKickoffPayloadKeyPreviousStatus:      string(previousStatus),
	}
}
