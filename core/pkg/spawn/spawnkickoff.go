// spawnkickoff.go is the M7.1.b production implementation of the
// approval-package SpawnKickoff seam. The kickoffer is the bridge
// between the Slack approval card and the M7.1.a saga runner: when
// the admin clicks Approve on a `propose_spawn` row, the dispatcher
// hands a kickoffer the freshly-minted saga id, the manifest_version
// id, the freshly-minted watchkeeper id, the Watchmaster's claim, and
// the approval token; the kickoffer composes the audit
// `manifest_approved_for_spawn` event, persists the saga row via
// [saga.SpawnSagaDAO.Insert], seeds a [saga.SpawnContext] on the
// per-call `context.Context`, and runs the saga via [saga.Runner.Run]
// with the construction-time configured step list (M7.1.c.c
// registration; concrete steps land in M7.1.c–.e).
package spawn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// ErrInvalidKickoffArgs is returned by [SpawnKickoffer.Kickoff] when
// the supplied per-call arguments fail synchronous shape validation —
// currently: zero `sagaID`, zero `manifestVersionID`, zero
// `watchkeeperID`. The fail-fast guard runs BEFORE the audit-emit /
// state-write side effects so a malformed kickoff leaves NO orphan
// `manifest_approved_for_spawn` row and NO orphan saga row.
// Matchable via [errors.Is].
var ErrInvalidKickoffArgs = errors.New("spawn: invalid kickoff args")

// kickoffLogAppender is the minimal subset of [keeperslog.Writer] the
// kickoffer consumes — only [keeperslog.Writer.Append]. Re-declared
// locally (rather than reusing [keepersLogAppender] from slack_app.go)
// so a reviewer reading spawnkickoff.go in isolation sees the contract
// without cross-file lookup. The two interfaces are structurally
// identical; both are satisfied by [*keeperslog.Writer] at the seam.
type kickoffLogAppender interface {
	Append(ctx context.Context, evt keeperslog.Event) (string, error)
}

// Compile-time assertion: every value satisfying the package-internal
// [keepersLogAppender] also satisfies [kickoffLogAppender]; pins the
// two seams against future drift.
var _ kickoffLogAppender = keepersLogAppender(nil)

// EventTypeManifestApprovedForSpawn is the M7.1.b audit event type the
// kickoffer emits AFTER persisting a freshly-inserted saga row via
// [saga.SpawnSagaDAO.InsertIfAbsent]. Hoisted to a constant so the
// payload-shape regression test pins the wire vocabulary AND so a
// downstream consumer can match by string equality without a typo
// risk.
//
// Distinct prefix (`manifest_`) so it does NOT collide with the
// `llm_turn_cost_*` family established in M6.3.e or the `saga_*`
// family established in M7.1.a.
const EventTypeManifestApprovedForSpawn = "manifest_approved_for_spawn"

// EventTypeManifestApprovalReplayedForSpawn is the M7.3.a audit event
// type the kickoffer emits when the supplied approval_token's
// idempotency_key already names a row (the upstream approval was
// resubmitted, e.g. Slack retry / double-click) AND the prior saga
// has already advanced beyond `pending`. Distinct from
// `manifest_approved_for_spawn` so a downstream consumer can branch
// on the wire `event_type` and surface "approval replayed; no second
// saga, no second Slack App" without parsing payload structure.
//
// The payload carries the SAME closed-set keys as
// `manifest_approved_for_spawn` plus the FIRST-call's `saga_id`,
// `watchkeeper_id`, and `previous_status` keys (codex iter-1 Major:
// the replay event MUST authoritatively name the existing saga, not
// the discarded second-call candidate). All ids are sourced from
// [saga.IdempotentInsertResult.Existing], not the second-call args.
const EventTypeManifestApprovalReplayedForSpawn = "manifest_approval_replayed_for_spawn"

// EventTypeManifestRejectedAfterSpawnFailure is the M7.3.b audit event
// type the kickoffer emits AFTER [saga.Runner.Run] returns non-nil.
// The row is the operator-visible "we tried to spawn this manifest,
// the saga aborted, the rollback chain ran" signal. A downstream
// consumer (e.g. a future Manifest-status reconciler) treats the row
// as the trigger to mark the Manifest rejected; M7.3.b ships the
// emit only — the Manifest-DAO write is deferred to a follow-up so
// this PR's surface stays narrow per the roadmap's "no concrete
// Compensate impls — foundation for M7.3.c" wording.
//
// Distinct prefix (`manifest_`) so it does NOT collide with the
// `saga_*` family established in M7.1.a or the `llm_turn_cost_*`
// family established in M6.3.e. Distinct event_type from the M7.3.a
// `manifest_approval_replayed_for_spawn` row so an operator can
// branch on the wire vocabulary without parsing payload structure.
//
// The payload carries the FIRST-call's `saga_id` /
// `manifest_version_id` / `watchkeeper_id` (sourced from
// [saga.IdempotentInsertResult.Existing] so a catch-up rejection
// names the original saga, not the discarded second-call
// candidate) plus `agent_id` (the bot that emitted the row) and
// `approval_token_prefix` (the M6.3.b token-prefix-display lesson —
// never the full bearer token).
const EventTypeManifestRejectedAfterSpawnFailure = "manifest_rejected_after_spawn_failure"

// Closed-set audit payload keys. Hoisted to constants so the
// payload-shape regression test pins the wire vocabulary (M2b.7 PII
// discipline). The kickoffer is the SOLE composer of this payload.
const (
	kickoffPayloadKeyManifestVersionID   = "manifest_version_id"
	kickoffPayloadKeyApprovalTokenPrefix = "approval_token_prefix"
	kickoffPayloadKeyAgentID             = "agent_id"
	kickoffPayloadKeySagaID              = "saga_id"
	kickoffPayloadKeyWatchkeeperID       = "watchkeeper_id"
	kickoffPayloadKeyPreviousStatus      = "previous_status"
	approvalTokenPrefixLen               = 6
	approvalTokenPrefixPrefix            = "tok-"
)

// SpawnKickoffer is the production implementation of the
// approval-package SpawnKickoff seam. Construct via [NewSpawnKickoffer];
// the zero value is NOT usable.
//
// The `Spawn` prefix on a type in package `spawn` is deliberate: AC4
// pins the exported name as `spawn.SpawnKickoffer` so the production
// wiring in `core/cmd/keep/main.go` reads as a literal seam pair with
// the `approval.SpawnKickoff` interface. Renaming to `Kickoffer` would
// force every downstream consumer to alias on import.
//
// Concurrency: safe for concurrent use after construction. Holds only
// immutable configuration; per-call state lives on the goroutine
// stack.
//
//nolint:revive // AC4 pins the exported name; see comment above.
type SpawnKickoffer struct {
	// logger is the audit-emit seam. Typed as [kickoffLogAppender]
	// (declared above in this file) so a reviewer reading the field
	// sees the 1-method contract inline. The package-internal
	// [keepersLogAppender] from slack_app.go is structurally
	// identical; both are satisfied by [*keeperslog.Writer].
	logger  kickoffLogAppender
	dao     saga.SpawnSagaDAO
	runner  *saga.Runner
	agentID string
	// steps is the M7.1.c.c step list registered at construction
	// time. May be nil / empty — the runner treats nil as an empty
	// slice and the saga completes immediately with a single
	// `saga_completed` audit event (matches the M7.1.b zero-step
	// behaviour). Production wiring populates this with
	// CreateApp + OAuthInstall + BotProfile + (M7.1.d Notebook) +
	// (M7.1.e Runtime launch) over the M7.1.c–.e milestones.
	steps []saga.Step
}

// SpawnKickoffDeps is the construction-time bag wired into
// [NewSpawnKickoffer]. Held in a struct so a future addition (e.g. a
// clock, a tracer) lands as a new field without breaking the
// constructor signature.
//
//nolint:revive // AC4 pins the exported `SpawnKickoffer` family name.
type SpawnKickoffDeps struct {
	// Logger is the audit-emit seam. Required; a nil Logger is
	// rejected at construction. Typed as [kickoffLogAppender] —
	// declared in this file — so the contract is visible inline.
	// [*keeperslog.Writer] satisfies the seam in production.
	Logger kickoffLogAppender

	// DAO is the saga-persistence seam. Required; a nil DAO is
	// rejected at construction. Production wiring composes
	// [saga.MemorySpawnSagaDAO] today; a Postgres-backed adapter is
	// deferred per the M6.3.b "ship in-memory DAO with consumer"
	// lesson.
	DAO saga.SpawnSagaDAO

	// Runner is the saga-runner seam. Required; a nil Runner is
	// rejected at construction. The kickoffer calls
	// [saga.Runner.Run] with the construction-time-configured
	// [SpawnKickoffDeps.Steps] slice (M7.1.c.c) seeded with a
	// per-saga [saga.SpawnContext] on `ctx`.
	Runner *saga.Runner

	// AgentID is the bot's stable agent identifier emitted on every
	// `manifest_approved_for_spawn` audit row. Empty values are
	// rejected at construction so a downstream consumer's `agent_id`
	// query never silently returns rows with no owner.
	AgentID string

	// Steps is the M7.1.c.c-introduced step list the kickoffer hands
	// to [saga.Runner.Run] on every Kickoff. Optional — a nil / empty
	// slice keeps the M7.1.b zero-step behaviour (the saga completes
	// immediately with a single `saga_completed` audit event). The
	// kickoffer takes a defensive copy at construction time so a
	// post-construction mutation of the caller's slice does not
	// affect saga runs.
	//
	// Production wiring populates this with the M7.1.c.a CreateApp
	// step + M7.1.c.b.b OAuthInstall step + M7.1.c.c BotProfile step
	// (in that order), with M7.1.d Notebook + M7.1.e Runtime launch
	// landing in their own milestones.
	Steps []saga.Step
}

// NewSpawnKickoffer constructs a [SpawnKickoffer] with the supplied
// [SpawnKickoffDeps]. Logger, DAO, Runner, and AgentID are required;
// a nil/empty value for any of them panics with a clear message —
// matches the panic discipline of [keeperslog.New] and
// [saga.NewRunner]. Steps is optional (nil / empty produces a
// zero-step saga matching the M7.1.b behaviour).
func NewSpawnKickoffer(deps SpawnKickoffDeps) *SpawnKickoffer {
	if deps.Logger == nil {
		panic("spawn: NewSpawnKickoffer: deps.Logger must not be nil")
	}
	if deps.DAO == nil {
		panic("spawn: NewSpawnKickoffer: deps.DAO must not be nil")
	}
	if deps.Runner == nil {
		panic("spawn: NewSpawnKickoffer: deps.Runner must not be nil")
	}
	if deps.AgentID == "" {
		panic("spawn: NewSpawnKickoffer: deps.AgentID must not be empty")
	}
	steps := append([]saga.Step(nil), deps.Steps...)
	return &SpawnKickoffer{
		logger:  deps.Logger,
		dao:     deps.DAO,
		runner:  deps.Runner,
		agentID: deps.AgentID,
		steps:   steps,
	}
}

// Kickoff seeds the spawn saga and runs it through the
// construction-time-registered step list, OR short-circuits with a
// `manifest_approval_replayed_for_spawn` audit event when the supplied
// `approvalToken` already names a persisted saga that has advanced
// past `pending` (the M7.3.a idempotency replay path), OR resumes a
// `pending` saga whose original audit-append failed mid-flight (the
// M7.3.a catch-up path — codex iter-1 Critical fix).
//
// Sequence (load-bearing — the order is pinned by an ordering test):
//
//  1. Fail-fast validation rejects `uuid.Nil` sagaID /
//     manifestVersionID / watchkeeperID and an empty / whitespace-only
//     `approvalToken` with [ErrInvalidKickoffArgs] BEFORE any
//     audit-emit / state-write side effect. An empty approval token
//     would silently bypass the idempotency dedup at the DAO layer
//     ([saga.ErrEmptyIdempotencyKey]).
//  2. Call [saga.SpawnSagaDAO.InsertIfAbsent] with `approvalToken`
//     as the idempotency_key, persisting `watchkeeperID` on the
//     fresh-insert row so the M7.3.a replay-payload contract can
//     emit the FIRST-call's id (codex iter-1 Major).
//  3. On INSERT (`result.Inserted == true`): emit
//     `manifest_approved_for_spawn` (the original M7.1.b audit row)
//     using the new-call's args, seed the per-call
//     [saga.SpawnContext] on `ctx`, and call [saga.Runner.Run].
//  4. On REPLAY of an ALREADY-RUN saga
//     (`result.Inserted == false` AND `Existing.Status !=
//     SagaStatePending`): emit
//     `manifest_approval_replayed_for_spawn` carrying the FIRST-call's
//     `saga_id` / `manifest_version_id` / `watchkeeper_id` /
//     `previous_status`, then return nil. No second
//     `manifest_approved_for_spawn` row, no second
//     [saga.Runner.Run].
//  5. On REPLAY of a STUCK saga (`result.Inserted == false` AND
//     `Existing.Status == SagaStatePending`): the original Kickoff's
//     audit-append succeeded the InsertIfAbsent BUT failed the
//     `manifest_approved_for_spawn` emit OR crashed before reaching
//     [saga.Runner.Run]. Catch-up: emit
//     `manifest_approved_for_spawn` (using the FIRST-call's ids
//     via `Existing`), seed [saga.SpawnContext] with
//     `Existing.ManifestVersionID` + `Existing.WatchkeeperID` +
//     the new call's `claim`, and run [saga.Runner.Run] for the
//     existing saga id. The audit chain catches up to the original
//     intent rather than parking the saga in `pending` forever.
//
// The catch-up branch (step 5) is the codex iter-1 Critical fix:
// without it, a transient keeperslog outage between
// `InsertIfAbsent` and the original `manifest_approved_for_spawn`
// emit would create a row stuck in `pending`, the operator's retry
// would fall into the replay branch and silently no-op, and the
// approval_token would be locked in `pending_approvals` (resolved
// state) so no future flow could complete the saga. Resume on
// `pending` is safe because the runner is not yet running; no step
// state has been emitted; the catch-up path emits exactly the
// audit chain a successful first call would have produced.
//
// Errors are wrapped with the `spawn:` prefix; the underlying
// keeperslog / saga sentinels remain matchable via [errors.Is]
// through the wrap chain.
func (k *SpawnKickoffer) Kickoff(
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
	// Whitespace-only tokens normalise to empty for the
	// idempotency_key contract — see [saga.ErrEmptyIdempotencyKey].
	if strings.TrimSpace(approvalToken) == "" {
		return fmt.Errorf("%w: empty approvalToken", ErrInvalidKickoffArgs)
	}

	result, err := k.dao.InsertIfAbsent(ctx, sagaID, manifestVersionID, watchkeeperID, approvalToken)
	if err != nil {
		return fmt.Errorf("spawn: kickoff: insert saga: %w", err)
	}

	if !result.Inserted && result.Existing.Status != saga.SagaStatePending {
		if _, replayErr := k.logger.Append(ctx, keeperslog.Event{
			EventType: EventTypeManifestApprovalReplayedForSpawn,
			Payload: manifestApprovalReplayedPayload(
				result.Existing.ID,
				result.Existing.ManifestVersionID,
				result.Existing.WatchkeeperID,
				approvalToken,
				k.agentID,
				result.Existing.Status,
			),
		}); replayErr != nil {
			return fmt.Errorf("spawn: kickoff: append manifest_approval_replayed_for_spawn: %w", replayErr)
		}
		return nil
	}

	// Insert path OR catch-up-on-pending path: in both cases the saga
	// is about to (or originally was about to) run, so emit
	// `manifest_approved_for_spawn` and continue to Runner.Run. The
	// catch-up branch sources its ids from `result.Existing` so the
	// audit row authoritatively names the FIRST-call's saga.
	emitSagaID := result.Existing.ID
	emitManifestVersionID := result.Existing.ManifestVersionID
	emitWatchkeeperID := result.Existing.WatchkeeperID
	if _, err := k.logger.Append(ctx, keeperslog.Event{
		EventType: EventTypeManifestApprovedForSpawn,
		Payload:   manifestApprovedPayload(emitManifestVersionID, approvalToken, k.agentID),
	}); err != nil {
		return fmt.Errorf("spawn: kickoff: append manifest_approved_for_spawn: %w", err)
	}

	ctx = saga.WithSpawnContext(ctx, saga.SpawnContext{
		ManifestVersionID: emitManifestVersionID,
		AgentID:           emitWatchkeeperID,
		Claim:             claim,
	})

	if err := k.runner.Run(ctx, emitSagaID, k.steps); err != nil {
		// M7.3.b: the saga aborted (a step's Execute returned
		// non-nil; the runner has already emitted `saga_failed` AND
		// run the reverse-rollback chain over the previously-
		// successful steps). Emit the operator-visible
		// `manifest_rejected_after_spawn_failure` row so a
		// downstream consumer can mark the Manifest rejected and
		// surface the failure — best-effort: a non-nil append does
		// NOT shadow the original Run error (the operator's "what
		// failed?" question is answered by the saga's wrap chain,
		// not by the rejection-emit's wrap chain). Ids are sourced
		// from `result.Existing` so the catch-up-on-pending path
		// names the FIRST-call's saga, mirroring the M7.3.a
		// replay-payload contract.
		if _, appendErr := k.logger.Append(ctx, keeperslog.Event{
			EventType: EventTypeManifestRejectedAfterSpawnFailure,
			Payload: manifestRejectedAfterSpawnFailurePayload(
				emitSagaID,
				emitManifestVersionID,
				emitWatchkeeperID,
				approvalToken,
				k.agentID,
			),
		}); appendErr != nil {
			// Best-effort emit: a non-nil append does NOT shadow the
			// original Run error — but the kickoffer's call site MUST
			// surface the dropped rejection row to ops so an operator
			// debugging "why is the rejection row missing from
			// keepers_log for this saga_id" has a structured signal at
			// the call boundary, NOT just the keeperslog writer's
			// internal diagnostic sink (which is opaque to the
			// kickoffer's interface seam). slog.Warn picks up the
			// configured slog.Default() handler in production; in
			// unit tests with no handler wired the entry is silently
			// formatted to stderr and ignored — defense-in-depth
			// against a future regression that masks all rejection-
			// row drops.
			slog.WarnContext(
				ctx, "spawn: kickoff: append manifest_rejected_after_spawn_failure failed",
				"saga_id", emitSagaID.String(),
				"agent_id", k.agentID,
				"err_class", "manifest_rejected_after_spawn_failure_emit_dropped",
			)
		}
		return fmt.Errorf("spawn: kickoff: run saga: %w", err)
	}
	return nil
}

// manifestApprovedPayload composes the closed-set
// `manifest_approved_for_spawn` payload. PII guard: this function is
// the SOLE composer of the payload; if a future change adds a key,
// code review picks it up here and the wire-shape regression test
// pins it.
//
// The `approval_token` is rendered as the `tok-<first-6-chars>` prefix
// (the M6.3.b token-prefix-display lesson) so the full bearer token
// never lands on the audit chain.
func manifestApprovedPayload(manifestVersionID uuid.UUID, approvalToken, agentID string) map[string]any {
	return map[string]any{
		kickoffPayloadKeyManifestVersionID:   manifestVersionID.String(),
		kickoffPayloadKeyApprovalTokenPrefix: approvalTokenPrefix(approvalToken),
		kickoffPayloadKeyAgentID:             agentID,
	}
}

// manifestApprovalReplayedPayload composes the closed-set
// `manifest_approval_replayed_for_spawn` payload. Carries the prior
// saga's id, target watchkeeper id, manifest version, and status so a
// downstream consumer can distinguish "replayed mid-flight" from
// "replayed after terminal state" AND correlate the replay row to
// the saga's persisted bot id without a follow-up DAO read.
//
// All ids are sourced from [saga.IdempotentInsertResult.Existing] —
// codex iter-1 Major fix: pre-iter-1 the payload accepted the
// SECOND-call's `manifestVersionID` and emitted it verbatim, which
// could produce a self-contradictory row when a retried approval
// supplied a different `manifestVersionID` than the original (the
// DAO discards the second-call value but the audit emit kept it).
//
// PII guard: this function is the SOLE composer of the payload; the
// closed-set keys mirror `manifest_approved_for_spawn` plus three
// replay-only fields (`saga_id`, `watchkeeper_id`, `previous_status`).
// NEVER carries the full approval token, the original step's params,
// or any error string.
func manifestApprovalReplayedPayload(
	existingSagaID uuid.UUID,
	existingManifestVersionID uuid.UUID,
	existingWatchkeeperID uuid.UUID,
	approvalToken, agentID string,
	previousStatus saga.SagaState,
) map[string]any {
	return map[string]any{
		kickoffPayloadKeySagaID:              existingSagaID.String(),
		kickoffPayloadKeyManifestVersionID:   existingManifestVersionID.String(),
		kickoffPayloadKeyWatchkeeperID:       existingWatchkeeperID.String(),
		kickoffPayloadKeyApprovalTokenPrefix: approvalTokenPrefix(approvalToken),
		kickoffPayloadKeyAgentID:             agentID,
		kickoffPayloadKeyPreviousStatus:      string(previousStatus),
	}
}

// manifestRejectedAfterSpawnFailurePayload composes the closed-set
// `manifest_rejected_after_spawn_failure` payload. Carries the saga's
// id, manifest version, target watchkeeper, the bot's agent_id, and
// the truncated approval-token prefix so a downstream consumer can
// correlate the rejection row to the saga's other audit rows
// (`saga_failed`, `saga_step_compensated`*, `saga_compensated`)
// without a follow-up DAO read.
//
// All ids MUST be sourced from [saga.IdempotentInsertResult.Existing]
// at the call site — codex iter-1 lesson held forward from M7.3.a:
// the catch-up-on-pending path's caller-supplied
// `manifestVersionID` / `watchkeeperID` are the SECOND call's args
// (which the DAO discarded); only `Existing.*` carries the FIRST
// call's authoritative ids. Pre-iter-1 the kickoffer wired the
// new-call args into this payload directly; the M7.3.b implementation
// reuses the `emitManifestVersionID` / `emitWatchkeeperID` /
// `emitSagaID` locals already populated from `result.Existing` for
// the `manifest_approved_for_spawn` emit, so the rejection row stays
// consistent with the approval row by construction.
//
// PII guard: this function is the SOLE composer of the payload; the
// closed-set keys mirror `manifest_approval_replayed_for_spawn`
// MINUS the `previous_status` key (the rejection row is emitted
// post-Run on a saga that the runner has already transitioned to
// `failed`, so a `previous_status` would always be `failed` and
// adds no signal). NEVER carries the full approval token, the
// underlying step error, or any step-internal params — the saga's
// own `saga_failed` row carries the `last_error_class` sentinel.
func manifestRejectedAfterSpawnFailurePayload(
	sagaID uuid.UUID,
	manifestVersionID uuid.UUID,
	watchkeeperID uuid.UUID,
	approvalToken, agentID string,
) map[string]any {
	return map[string]any{
		kickoffPayloadKeySagaID:              sagaID.String(),
		kickoffPayloadKeyManifestVersionID:   manifestVersionID.String(),
		kickoffPayloadKeyWatchkeeperID:       watchkeeperID.String(),
		kickoffPayloadKeyApprovalTokenPrefix: approvalTokenPrefix(approvalToken),
		kickoffPayloadKeyAgentID:             agentID,
	}
}

// approvalTokenPrefix returns the operator-visible token prefix
// `tok-<first-6-chars>`. Tokens shorter than 6 runes are returned in
// full (still prefixed) so a defensive fallback never panics.
//
// Distinct from the M6.3.b cards-package `tokenPrefix` helper which
// appends an ellipsis for visual truncation; the audit-row variant
// emits a stable string (no trailing `…`) so a downstream regex
// matcher can pin the exact prefix shape.
func approvalTokenPrefix(token string) string {
	runes := []rune(token)
	if len(runes) <= approvalTokenPrefixLen {
		return approvalTokenPrefixPrefix + token
	}
	return approvalTokenPrefixPrefix + string(runes[:approvalTokenPrefixLen])
}
