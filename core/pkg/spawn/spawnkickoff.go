// spawnkickoff.go is the M7.1.b production implementation of the
// approval-package SpawnKickoff seam. The kickoffer is the bridge
// between the Slack approval card and the M7.1.a saga runner: when
// the admin clicks Approve on a `propose_spawn` row, the dispatcher
// hands a kickoffer the freshly-minted saga id, the manifest_version
// id, and the approval token; the kickoffer composes the audit
// `manifest_approved_for_spawn` event, persists the saga row via
// [saga.SpawnSagaDAO.Insert], and runs the saga via [saga.Runner.Run]
// with an empty step list (concrete steps land in M7.1.c–.e).
package spawn

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

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
// kickoffer emits BEFORE inserting the saga row. Hoisted to a constant
// so the payload-shape regression test pins the wire vocabulary AND so
// a downstream consumer can match by string equality without a typo
// risk.
//
// Distinct prefix (`manifest_`) so it does NOT collide with the
// `llm_turn_cost_*` family established in M6.3.e or the `saga_*`
// family established in M7.1.a.
const EventTypeManifestApprovedForSpawn = "manifest_approved_for_spawn"

// Closed-set audit payload keys. Hoisted to constants so the
// payload-shape regression test pins the wire vocabulary (M2b.7 PII
// discipline). The kickoffer is the SOLE composer of this payload.
const (
	kickoffPayloadKeyManifestVersionID   = "manifest_version_id"
	kickoffPayloadKeyApprovalTokenPrefix = "approval_token_prefix"
	kickoffPayloadKeyAgentID             = "agent_id"
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
	// [saga.Runner.Run] with an empty step list in M7.1.b — concrete
	// steps land in M7.1.c–.e.
	Runner *saga.Runner

	// AgentID is the bot's stable agent identifier emitted on every
	// `manifest_approved_for_spawn` audit row. Empty values are
	// rejected at construction so a downstream consumer's `agent_id`
	// query never silently returns rows with no owner.
	AgentID string
}

// NewSpawnKickoffer constructs a [SpawnKickoffer] with the supplied
// [SpawnKickoffDeps]. Logger, DAO, Runner, and AgentID are required;
// a nil/empty value for any of them panics with a clear message —
// matches the panic discipline of [keeperslog.New] and
// [saga.NewRunner].
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
	return &SpawnKickoffer{
		logger:  deps.Logger,
		dao:     deps.DAO,
		runner:  deps.Runner,
		agentID: deps.AgentID,
	}
}

// Kickoff seeds the spawn saga and runs it with an empty step list.
//
// Sequence (load-bearing — the order is pinned by an ordering test):
//
//  1. Emit `manifest_approved_for_spawn` audit event (audit-emit
//     precedes state-write per the M6.3.e + M7.1.a pattern; the
//     audit row is the canonical "we tried" signal even when
//     state-persistence fails afterwards).
//  2. Call [saga.SpawnSagaDAO.Insert] to persist the saga row
//     (Insert MUST precede Run — the runner's first action is
//     [saga.SpawnSagaDAO.Get], which would fail without the row).
//  3. Call [saga.Runner.Run] with an empty step list. M7.1.a's
//     zero-step run completes immediately and emits a single
//     `saga_completed` event, so the production happy-path emits
//     exactly: 1 `manifest_approved_for_spawn` + 1 `saga_completed`.
//
// Errors are wrapped with the `spawn:` prefix; the underlying
// keeperslog / saga sentinels remain matchable via [errors.Is]
// through the wrap chain. A non-nil error return surfaces back to
// the dispatcher, which audits it as `approval_replay_failed`.
func (k *SpawnKickoffer) Kickoff(
	ctx context.Context,
	sagaID uuid.UUID,
	manifestVersionID uuid.UUID,
	approvalToken string,
) error {
	if _, err := k.logger.Append(ctx, keeperslog.Event{
		EventType: EventTypeManifestApprovedForSpawn,
		Payload:   manifestApprovedPayload(manifestVersionID, approvalToken, k.agentID),
	}); err != nil {
		return fmt.Errorf("spawn: kickoff: append manifest_approved_for_spawn: %w", err)
	}

	if err := k.dao.Insert(ctx, sagaID, manifestVersionID); err != nil {
		return fmt.Errorf("spawn: kickoff: insert saga: %w", err)
	}

	if err := k.runner.Run(ctx, sagaID, []saga.Step{}); err != nil {
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
