// budget.go ships the M1.5 K2K token-budget enforcement [Enforcer]
// seam + production [Writer] implementation. See the package doc for
// the resolution order, audit discipline, escalation discipline, and
// PII discipline.
//
// audit discipline: this file imports `core/pkg/k2k/audit` (the M1.4
// typed-emitter seam) and NOT `core/pkg/keeperslog`. The peer-tool
// source-grep AC bans `keeperslog.` / `.Append(` references in the
// peer.\* files; this budget package is one layer below the peer-tool
// surface and the same discipline carries over — emit through the typed
// `audit.Emitter` seam, never inline keeperslog calls.
//
// PII discipline: the package observes only typed numeric primitives
// (int64 deltas, int64 budgets) + uuid ids + the acting watchkeeper id
// (a stable identifier, not free-form text). No body bytes flow through
// this layer; the persisted `k2k_messages.body` is the source of truth
// for the underlying message and a future operator audit consumer joins
// on `conversation_id` to recover it.

package budget

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k/audit"
)

// DefaultTokenBudget is the package-wide token-budget default the
// production wiring stamps onto a fresh K2K conversation when neither
// the caller nor the per-Watchkeeper Manifest override supplies a
// budget. The value is intentionally generous (100k tokens) so a normal
// peer.Ask → peer.Reply round-trip never trips the enforcement path
// while the M1.5 plumbing is exercised in tests + dev/smoke loops.
//
// Operators override via the project config (wiring-time, future M2.4
// integration) OR per-Watchkeeper Manifest (per the M3.1
// `immutable_core.cost_limits` projection — see
// `core/pkg/manifest.K2KTokenBudget`). A zero override disables
// enforcement for that scope (the [Enforcer.Charge] surface treats
// `budget == 0` as "no cap").
const DefaultTokenBudget int64 = 100_000

// escalationTriggerTimeout caps the detached-ctx window the over-budget
// escalation trigger runs under. The escalation is best-effort
// observability + workflow handoff; a 5-second cap is long enough that
// a healthy M1.6 escalation saga step completes under realistic
// backpressure but short enough that a degenerate trigger implementation
// cannot tie up the peer-tool caller's return indefinitely. Mirrors the
// `auditEmitTimeout` constant in `core/pkg/k2k/lifecycle.go` and
// `core/pkg/peer/tool.go`.
const escalationTriggerTimeout = 5 * time.Second

// auditEmitTimeout caps the detached-ctx window the over-budget audit
// emit runs under. Same rationale as [escalationTriggerTimeout]; both
// are detached so a caller-side cancellation arriving after IncTokens
// succeeded does NOT systematically drop the observability surface.
const auditEmitTimeout = 5 * time.Second

// ChargeParams is the closed-set input shape [Enforcer.Charge] accepts.
// Hoisted to a struct (rather than a long positional arg list) so a
// future addition (e.g. an explicit `MessageID` for cross-correlation
// with the persisted `k2k_messages` row) lands as a new field rather
// than a breaking signature change. Mirrors the M1.3.\* peer-tool
// `<Op>Params` shape discipline.
type ChargeParams struct {
	// ConversationID is the persisted [k2k.Conversation.ID] the charge
	// applies to. Required (non-zero); [Enforcer.Charge] surfaces
	// [ErrInvalidChargeParams] otherwise.
	ConversationID uuid.UUID

	// OrganizationID is the persisted
	// [k2k.Conversation.OrganizationID]. Required (non-zero); needed
	// for the audit payload and (in a future M1.6 escalation impl) the
	// per-tenant routing lookup.
	OrganizationID uuid.UUID

	// ActingWatchkeeperID is the id of the watchkeeper whose call site
	// triggered the charge — the peer.Ask sender for request appends,
	// the peer.Reply sender for reply appends. Required (non-empty
	// after whitespace-trim). The M1.6 escalation saga consults this
	// id to identify the over-budget conversation's responsible party.
	ActingWatchkeeperID string

	// TokenBudget is the conversation's persisted
	// [k2k.Conversation.TokenBudget]. Forwarded by the caller because
	// the conversation row was already read upstream (the peer.Ask
	// flow's k2k.Lifecycle.Open returned it; the peer.Reply flow's
	// Get returned it). Forwarding avoids a per-charge `Get` round-
	// trip while keeping the row as the source of truth. A zero value
	// disables enforcement for this conversation (no over-budget event
	// is ever emitted regardless of `TokensUsed`).
	TokenBudget int64

	// Delta is the token count to add to the conversation's running
	// counter via [k2k.Repository.IncTokens]. Required (positive);
	// [Enforcer.Charge] surfaces [ErrInvalidChargeParams] otherwise.
	// The peer-tool layer computes this from the message body — see
	// [EstimateTokensFromBody] for the M1.5 byte-derived estimator.
	Delta int64

	// CorrelationID is the optional upstream-saga / Watch Order id the
	// over-budget event payload carries. Forwarded verbatim onto the
	// keeperslog row's correlation context (when the M1.4 emitter is
	// driven through a ctx that already carries the correlation id,
	// this field is unused; M1.5 keeps it on the params struct for
	// future-proofing).
	CorrelationID uuid.UUID
}

// ChargeResult is the closed-set output shape [Enforcer.Charge]
// returns. Hoisted to a struct so the peer-tool caller can branch on
// the post-charge state without inspecting an error chain.
type ChargeResult struct {
	// TokensUsed is the post-increment running counter the underlying
	// [k2k.Repository.IncTokens] returned. Strictly greater than the
	// pre-call value by [ChargeParams.Delta].
	TokensUsed int64

	// TokenBudget echoes [ChargeParams.TokenBudget] so the caller can
	// log the post-charge state without re-reading the row. Zero means
	// "enforcement disabled for this conversation".
	TokenBudget int64

	// OverBudget reports whether the charge crossed the budget. True
	// iff `TokensUsed > TokenBudget && TokenBudget > 0` — a zero
	// budget never trips OverBudget regardless of TokensUsed.
	OverBudget bool
}

// Enforcer is the consumer-facing seam the peer-tool layer composes
// into `peer.Deps.Budget`. The interface is intentionally narrow — one
// method — so a hand-rolled fake in the consumer's test suite asserts
// the budget contract directly without implementing every keeperslog +
// escalation detail.
//
// Implementations MUST be safe for concurrent use across goroutines.
// The production [Writer] is goroutine-safe by construction (composes
// goroutine-safe k2k.Repository + audit.Emitter + EscalationTrigger).
//
// A nil-permissive contract is documented at the consumer surface
// (`peer.Deps.Budget` is OPTIONAL; nil-permissive callers stay valid).
// Implementations do NOT need to handle a nil receiver — the consumer
// gates the call site.
type Enforcer interface {
	// Charge atomically advances the conversation's `tokens_used`
	// counter by `params.Delta` via [k2k.Repository.IncTokens],
	// compares the post-increment counter against `params.TokenBudget`,
	// and on a crossing emits a `k2k_over_budget` audit row + triggers
	// the escalation seam. Returns the post-charge state.
	//
	// Validation order (fail-fast precedes IncTokens):
	//   1. ctx.Err — refuses a pre-cancelled ctx.
	//   2. params.ConversationID != uuid.Nil — [ErrInvalidChargeParams].
	//   3. params.OrganizationID != uuid.Nil — [ErrInvalidChargeParams].
	//   4. trimmed params.ActingWatchkeeperID != "" — [ErrInvalidChargeParams].
	//   5. params.Delta > 0 — [ErrInvalidChargeParams].
	//   6. params.TokenBudget >= 0 — [ErrInvalidChargeParams].
	//
	// IncTokens errors propagate verbatim (the caller branches on
	// `errors.Is(err, k2k.ErrConversationNotFound)` /
	// `errors.Is(err, k2k.ErrAlreadyArchived)`). An audit-emit or
	// escalation-trigger failure does NOT propagate — the persisted
	// counter advance is the load-bearing surface; the observability
	// + workflow surfaces are best-effort. Mirrors the M1.4 audit-emit
	// no-propagate discipline.
	Charge(ctx context.Context, params ChargeParams) (ChargeResult, error)
}

// EscalationTrigger is the seam the M1.6 escalation saga implements.
// M1.5 ships the seam + a [NoopEscalationTrigger] default so the
// budget-overage detection wiring is exercised end-to-end without
// M1.6's implementation. The production M1.6 implementation routes
// over-budget conversations to the human lead in Slack and (on lead
// unresponsive) to the Watchmaster.
//
// Implementations MUST be safe for concurrent use across goroutines.
// A non-nil error from TriggerOverBudget does NOT propagate to the
// peer-tool caller — the over-budget detection's success is gated on
// the IncTokens advance, not on the escalation surface.
type EscalationTrigger interface {
	// TriggerOverBudget notifies the escalation saga of an over-budget
	// conversation. The trigger MUST be idempotent at the saga layer —
	// a retried peer-tool charge that crosses the budget twice in
	// quick succession may invoke TriggerOverBudget twice; the M1.6
	// implementation owns the deduplication discipline.
	TriggerOverBudget(ctx context.Context, params OverBudgetTrigger) error
}

// OverBudgetTrigger is the closed-set input shape
// [EscalationTrigger.TriggerOverBudget] accepts. Carries everything the
// M1.6 saga needs to route the escalation without re-reading the row:
// the conversation id, the organization id, the acting watchkeeper id,
// the post-charge counter, and the configured budget. Hoisted to a
// struct so a future addition (an explicit `Severity` discriminator or
// an `EscalationReason` closed-set code) lands as a new field rather
// than a breaking signature change.
type OverBudgetTrigger struct {
	// ConversationID is the persisted [k2k.Conversation.ID] under
	// escalation. Required.
	ConversationID uuid.UUID

	// OrganizationID is the persisted
	// [k2k.Conversation.OrganizationID]. Required.
	OrganizationID uuid.UUID

	// ActingWatchkeeperID is the acting watchkeeper id from the
	// triggering charge. The M1.6 saga consults this id to identify
	// the conversation's responsible party.
	ActingWatchkeeperID string

	// TokensUsed is the post-charge counter that crossed the budget.
	TokensUsed int64

	// TokenBudget is the conversation's persisted budget the charge
	// exceeded.
	TokenBudget int64

	// CorrelationID echoes [ChargeParams.CorrelationID] (uuid.Nil
	// when unset).
	CorrelationID uuid.UUID

	// ObservedAt is the wall-clock time the over-budget condition was
	// observed. The [Writer] stamps this from its configured clock
	// (overridable via [WithNow]).
	ObservedAt time.Time
}

// NoopEscalationTrigger is the default [EscalationTrigger]
// implementation [NewWriter] composes when the caller does not supply
// one. Every [TriggerOverBudget] call is a silent no-op (returns nil).
// The seam exists so the M1.5 enforcement code path is unconditionally
// exercised (the budget-crossing branch calls the trigger; the trigger
// just does not do anything until M1.6 lands).
type NoopEscalationTrigger struct{}

// Compile-time assertion: [NoopEscalationTrigger] satisfies
// [EscalationTrigger]. Pins the seam shape so a future change to the
// interface surface fails the build here. Mirrors the
// `var _ audit.Emitter = (*audit.Writer)(nil)` discipline.
var _ EscalationTrigger = NoopEscalationTrigger{}

// TriggerOverBudget implements [EscalationTrigger.TriggerOverBudget].
// The no-op default returns nil without observing the params; the M1.6
// implementation will route to a real saga.
func (NoopEscalationTrigger) TriggerOverBudget(_ context.Context, _ OverBudgetTrigger) error {
	return nil
}

// Repository is the narrow seam [Writer] consumes from
// [k2k.Repository]. Pinned to the single method the production code
// actually touches so tests can inject a focused fake without
// re-implementing the full conversation lifecycle. Mirrors the
// `core/pkg/peer/tool.go` per-consumer narrow-seam discipline.
type Repository interface {
	// IncTokens matches [k2k.Repository.IncTokens] exactly so
	// production wiring passes the concrete repository verbatim.
	IncTokens(ctx context.Context, id uuid.UUID, delta int64) (int64, error)
}

// Logger is the optional diagnostic seam [Writer] consults when an
// audit emit or escalation trigger fails. Matches the minimal subset of
// the project's `*log.Logger` surface so production wiring passes the
// keep binary's structured logger verbatim; tests omit the logger
// (nil-permissive). The interface is intentionally narrow so a future
// switch to a structured-logging library (slog) does not break this
// package.
type Logger interface {
	Printf(format string, args ...any)
}

// WriterOption mutates the [Writer] configuration at construction time.
// Functional-options pattern mirroring [llm/cost.LoggingProviderOption]
// + the standard project precedent. Implementations apply the options
// in order; later options override earlier ones for the same field.
type WriterOption func(*Writer)

// WithEscalationTrigger overrides the default [NoopEscalationTrigger]
// with the supplied trigger. Wiring-time customisation — the production
// M1.6 saga wiring passes its own implementation; tests inject fakes.
// A nil trigger is a no-op (the default stays in place).
func WithEscalationTrigger(t EscalationTrigger) WriterOption {
	return func(w *Writer) {
		if t != nil {
			w.trigger = t
		}
	}
}

// WithLogger overrides the optional diagnostic logger with the supplied
// implementation. A nil logger is a no-op (the default nil stays in
// place — every diagnostic call short-circuits at the call site).
func WithLogger(l Logger) WriterOption {
	return func(w *Writer) {
		if l != nil {
			w.logger = l
		}
	}
}

// WithNow overrides the wall-clock used to stamp [OverBudgetTrigger.ObservedAt]
// and [audit.OverBudgetEvent.ObservedAt]. Defaults to [time.Now]; test
// fixtures pass a deterministic clock. A nil func is a no-op (the
// default stays in place).
func WithNow(now func() time.Time) WriterOption {
	return func(w *Writer) {
		if now != nil {
			w.now = now
		}
	}
}

// Writer is the production [Enforcer] implementation. Construct via
// [NewWriter]; the zero value is NOT usable — callers must always go
// through the constructor so the dependency invariants are enforced at
// construction time. Mirrors the `audit.Writer` + saga-step constructor
// discipline.
//
// Concurrency: safe for concurrent use after construction. Holds only
// immutable configuration; per-call state lives on the goroutine stack.
type Writer struct {
	repo    Repository
	emitter audit.Emitter
	trigger EscalationTrigger
	now     func() time.Time
	logger  Logger
}

// Compile-time assertion: [*Writer] satisfies [Enforcer]. Pins the
// seam shape so a future change to the interface surface fails the
// build here.
var _ Enforcer = (*Writer)(nil)

// NewWriter constructs a [Writer] backed by the supplied repository
// + audit emitter. Panics on a nil [Repository] or a nil [audit.Emitter]
// — both are programmer bugs at wiring time, not runtime errors to
// thread through Charge return values. The panic message names the
// offending field so an operator reading a stack trace can fix the
// wiring immediately. Mirrors the [k2k.NewLifecycle] +
// [audit.NewWriter] panic discipline.
//
// Options are applied in order; the default [EscalationTrigger] is
// [NoopEscalationTrigger]; the default clock is [time.Now]; the
// default logger is nil (every diagnostic call short-circuits).
func NewWriter(repo Repository, emitter audit.Emitter, opts ...WriterOption) *Writer {
	if repo == nil {
		panic("budget: NewWriter: repo must not be nil")
	}
	if emitter == nil {
		panic("budget: NewWriter: emitter must not be nil")
	}
	w := &Writer{
		repo:    repo,
		emitter: emitter,
		trigger: NoopEscalationTrigger{},
		now:     time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(w)
		}
	}
	return w
}

// Charge implements [Enforcer.Charge]. See the interface godoc for the
// validation order and propagation contract.
func (w *Writer) Charge(ctx context.Context, params ChargeParams) (ChargeResult, error) {
	if err := ctx.Err(); err != nil {
		return ChargeResult{}, err
	}
	if err := validateChargeParams(params); err != nil {
		return ChargeResult{}, err
	}

	tokensUsed, err := w.repo.IncTokens(ctx, params.ConversationID, params.Delta)
	if err != nil {
		return ChargeResult{}, fmt.Errorf("budget: charge: inc tokens: %w", err)
	}

	overBudget := params.TokenBudget > 0 && tokensUsed > params.TokenBudget
	result := ChargeResult{
		TokensUsed:  tokensUsed,
		TokenBudget: params.TokenBudget,
		OverBudget:  overBudget,
	}
	if !overBudget {
		return result, nil
	}

	// Crossing the budget triggers two best-effort side effects:
	//   (a) emit the `k2k_over_budget` audit row through the M1.4
	//       typed-emitter seam (the constant lives in
	//       `core/pkg/k2k/audit/events.go`);
	//   (b) notify the M1.6 escalation saga via the [EscalationTrigger]
	//       seam (the no-op default in M1.5 returns nil).
	//
	// Both side effects run under a detached `context.WithoutCancel`
	// ctx with a short timeout cap so a caller-side cancellation
	// arriving after IncTokens succeeded does NOT systematically drop
	// the observability + workflow handoff. Errors are logged via the
	// optional [Logger] seam but do NOT propagate — the IncTokens
	// advance is the load-bearing surface (mirrors the M1.4 audit-emit
	// no-propagate discipline).
	observedAt := w.now().UTC()
	w.emitOverBudget(ctx, params, tokensUsed, observedAt)
	w.triggerOverBudget(ctx, params, tokensUsed, observedAt)
	return result, nil
}

// emitOverBudget runs the detached-ctx audit emit for an over-budget
// crossing. Hoisted out of [Charge] so the orchestrator reads top-to-
// bottom as "validate → inc → branch → side effects" and stays under
// the gocyclo budget. A nil emitter is impossible by construction
// (NewWriter panics on nil); the function therefore unconditionally
// emits.
func (w *Writer) emitOverBudget(ctx context.Context, params ChargeParams, tokensUsed int64, observedAt time.Time) {
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditEmitTimeout)
	defer cancel()
	_, err := w.emitter.EmitOverBudget(auditCtx, audit.OverBudgetEvent{
		ConversationID: params.ConversationID,
		OrganizationID: params.OrganizationID,
		TokenBudget:    params.TokenBudget,
		TokensUsed:     tokensUsed,
		ObservedAt:     observedAt,
	})
	if err != nil && w.logger != nil {
		w.logger.Printf("budget: emit over-budget conversation=%s tokens_used=%d budget=%d: %v",
			params.ConversationID, tokensUsed, params.TokenBudget, err)
	}
}

// triggerOverBudget runs the detached-ctx escalation trigger for an
// over-budget crossing. Same hoist + non-propagate discipline as
// [emitOverBudget].
func (w *Writer) triggerOverBudget(ctx context.Context, params ChargeParams, tokensUsed int64, observedAt time.Time) {
	trigCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), escalationTriggerTimeout)
	defer cancel()
	err := w.trigger.TriggerOverBudget(trigCtx, OverBudgetTrigger{
		ConversationID:      params.ConversationID,
		OrganizationID:      params.OrganizationID,
		ActingWatchkeeperID: params.ActingWatchkeeperID,
		TokensUsed:          tokensUsed,
		TokenBudget:         params.TokenBudget,
		CorrelationID:       params.CorrelationID,
		ObservedAt:          observedAt,
	})
	if err != nil && w.logger != nil {
		w.logger.Printf("budget: trigger over-budget conversation=%s tokens_used=%d budget=%d: %v",
			params.ConversationID, tokensUsed, params.TokenBudget, err)
	}
}

// validateChargeParams runs every fail-fast check on [ChargeParams] so
// [Charge] stays scannable. Returns [ErrInvalidChargeParams] (wrapped
// with a human-readable field name) for any degenerate input.
func validateChargeParams(params ChargeParams) error {
	if params.ConversationID == uuid.Nil {
		return fmt.Errorf("%w: conversation_id must not be zero", ErrInvalidChargeParams)
	}
	if params.OrganizationID == uuid.Nil {
		return fmt.Errorf("%w: organization_id must not be zero", ErrInvalidChargeParams)
	}
	if strings.TrimSpace(params.ActingWatchkeeperID) == "" {
		return fmt.Errorf("%w: acting_watchkeeper_id must not be empty", ErrInvalidChargeParams)
	}
	if params.Delta <= 0 {
		return fmt.Errorf("%w: delta must be positive (got %d)", ErrInvalidChargeParams, params.Delta)
	}
	if params.TokenBudget < 0 {
		return fmt.Errorf("%w: token_budget must not be negative (got %d)", ErrInvalidChargeParams, params.TokenBudget)
	}
	return nil
}

// approxBytesPerToken is the M1.5 byte-derived token-estimate constant.
// Four bytes per token is the documented Anthropic / OpenAI rule of
// thumb for English text (Claude / GPT tokenizers average ≈4 bytes per
// token for plain prose). The estimator runs at the peer-tool layer
// over the `Body` []byte so the K2K plumbing is exercised end-to-end
// without a real tokenizer; a future M2.\* leaf can swap a tokenizer-
// driven counter behind the same call site without breaking the
// [Enforcer] contract.
const approxBytesPerToken = 4

// EstimateTokensFromBody is the M1.5 byte-derived token estimator the
// peer-tool layer uses until a real LLM tokenizer lands. Returns
// `ceil(len(body) / approxBytesPerToken)` clamped to a minimum of 1
// for a non-empty body so a single-byte payload still consumes one
// token (the lifecycle prefers "charged something" over
// "silent zero-charge" — an over-eager charge cannot hide a real
// budget breach, an under-charge can).
//
// Returns 0 for an empty / nil body so a caller that wants to skip
// the charge entirely (no body, no spend) gets a clean 0. The
// peer-tool layer fail-fasts on an empty body via [peer.ErrInvalidBody]
// upstream, so production paths never reach this branch with 0.
func EstimateTokensFromBody(body []byte) int64 {
	n := int64(len(body))
	if n == 0 {
		return 0
	}
	tokens := (n + approxBytesPerToken - 1) / approxBytesPerToken
	if tokens < 1 {
		return 1
	}
	return tokens
}

// ResolveBudget composes the configured default with an optional
// per-Watchkeeper override returned by the resolver. Three cases:
//
//   - override == 0, ok == false:  return default (no override declared).
//   - override == 0, ok == true:   return 0  (explicit "disable enforcement").
//   - override  > 0, ok == true:   return override.
//
// Negative overrides are clamped to 0 (treated as "disable") since the
// repository's [k2k.OpenParams.TokenBudget] validator rejects negative
// values via [k2k.ErrInvalidTokenBudget]; surfacing a negative through
// this helper would fail at the next step and obscure the configuration
// bug. Mirrors the `peer.RoleFilter.validate` "fail-loud at the boundary"
// discipline by clamping rather than panicking — the resolver may legitimately
// surface a zero / negative from an unset cost_limits map.
//
// Exposed for the peer-tool wiring so the `Deps.TokenBudgetResolver`
// composition is one helper call rather than a copy-pasted three-case
// switch.
func ResolveBudget(defaultBudget, override int64, ok bool) int64 {
	if !ok {
		return defaultBudget
	}
	if override < 0 {
		return 0
	}
	return override
}

// assertRepositorySubset is a compile-time check that any value
// satisfying [k2k.Repository] also satisfies the narrower [Repository]
// seam this package consumes. A future rename of `IncTokens` on either
// surface fails the build at this assignment. Hoisted into a private
// `_` assignment so the production wiring (which passes the concrete
// `*k2k.PostgresRepository`) does not have to repeat the cast.
func assertRepositorySubset(r k2k.Repository) Repository { return r }

var _ = assertRepositorySubset
