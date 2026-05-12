// Doc-block at file head documenting the seam contract.
//
// resolution order: nil-dep check (panic) → input.Validate (sentinel
// pass-through; runs BEFORE ctx.Err deliberately — validation is a
// pure function and cheaper than a syscall, so the caller's
// validation-vs-cancel error classification is stable) → ctx.Err
// pre-resolver (ctx.Err pass-through) → IdentityResolver
// (ErrIdentityResolution / ErrEmptyResolvedIdentity /
// ErrInvalidProposerID) → Clock.Now → IDGenerator.NewUUID → defensive
// deep-copy → ctx.Err pre-publish (refuses to land a "phantom"
// proposal when the caller has already cancelled) → publish with a
// cancel-detached child ctx via [context.WithoutCancel] so the
// durable-record contract on [ErrPublishToolProposed] is not racing
// the caller's mid-flight cancel against the eventbus's
// queue-depth-dependent send fast-path. The order is deliberate:
// validation fail-fasts BEFORE any side effects (no spurious identity
// resolution on bad input); the identity resolver runs BEFORE the
// clock + id mint so a tenant-rotation failure does not waste a
// process-monotonic correlation id.
//
// audit discipline: the [Proposer] never imports `keeperslog` and
// never calls `.Append(` (see source-grep AC). The audit log entry
// for `tool_proposed` lives in the M9.7 audit subscriber that
// observes [TopicToolProposed] events.
//
// PII discipline: the optional [Logger] is invoked with the
// proposal id + tool name + proposer id ONLY — never with
// [ProposalInput.CodeDraft], [ProposalInput.Purpose], or
// [ProposalInput.PlainLanguageDescription] bodies. The same
// metadata-only boundary protects the [TopicToolProposed] payload
// (see [ToolProposed] godoc).

package approval

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Publisher is the [eventbus.Bus] subset [Proposer.Submit] consumes
// — only the [Publisher.Publish] method. Defined in this package so
// production code never has to import the concrete `*eventbus.Bus`
// and tests can substitute a hand-rolled fake (mirrors
// `toolregistry.Publisher` from M9.1.a).
type Publisher interface {
	Publish(ctx context.Context, topic string, event any) error
}

// Clock is the time seam. Production wiring uses [ClockFunc] wrapping
// [time.Now]; tests pin a deterministic value so timestamps and
// correlation ids are reproducible. Same shape as
// `toolregistry.Clock`.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a plain `func() time.Time` to [Clock]. The
// `time.Now` wrapper is the production default; tests pass in a
// closure capturing a `*time.Time` they advance manually.
type ClockFunc func() time.Time

// Now implements [Clock].
func (f ClockFunc) Now() time.Time { return f() }

// IDGenerator is the seam for minting [Proposal.ID] values. The
// production default is [UUIDv7Generator]; tests substitute a
// fake returning deterministic UUIDs so assertions on the emitted
// event payload do not need to ignore the id field.
type IDGenerator interface {
	NewUUID() (uuid.UUID, error)
}

// UUIDv7Generator is the production [IDGenerator]: it returns
// time-ordered UUIDv7 values so the proposal ids sort
// chronologically in any store / log indexer.
type UUIDv7Generator struct{}

// NewUUID implements [IDGenerator].
func (UUIDv7Generator) NewUUID() (uuid.UUID, error) {
	return uuid.NewV7()
}

// IdentityResolver is the per-call seam for resolving the proposing
// agent's identity from the request context. Same lesson as
// M9.1.a's `AuthSecretResolver`: a function shape (not an
// interface) keeps the surface minimal, and per-call invocation
// avoids pinning a tenant-scoped value as a process-global static.
//
// Contract:
//
//   - Return a non-empty identifier on success.
//   - Return `("", <cause>)` on resolution failure (e.g. missing
//     header, expired token). [Proposer.Submit] wraps the returned
//     error with [ErrIdentityResolution] for the caller —
//     implementers MUST NOT pre-wrap with [ErrIdentityResolution]
//     themselves (double-wrap chains break `errors.Is` triage).
//   - Returning `("", nil)` is a programmer error caught by
//     [Proposer.Submit] as [ErrEmptyResolvedIdentity] (same fail-loud
//     discipline as `toolregistry.ErrEmptyResolvedAuth`).
//
// Shape discipline: the resolved identifier MUST be at most
// [MaxProposerIDLength] bytes — [Proposer.Submit] enforces this with
// [ErrInvalidProposerID] so a buggy resolver cannot leak an
// unbounded bearer-token-shaped string onto the
// [TopicToolProposed] event payload or the publish-failure log
// entry. Implementations SHOULD return a stable public identifier
// (e.g. an agent uuid in canonical form), NOT a credential or
// session token.
type IdentityResolver func(ctx context.Context) (string, error)

// Logger is the optional structured-log seam. Same shape as
// `toolregistry.Logger`. Implementations MUST NOT include
// reference-typed [ProposalInput] fields (`CodeDraft`, `Purpose`,
// `PlainLanguageDescription`) in `kv`; the proposer never passes
// them in but the discipline is documented for any future caller.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// ProposerDeps bundles the required dependencies for [New]. Every
// field is required — passing a nil [Publisher] / [Clock] /
// [IDGenerator] / [IdentityResolver] panics in [New] with a
// named-field message. Optional dependencies (currently
// [Logger]) are listed last.
type ProposerDeps struct {
	// Publisher emits [TopicToolProposed] events. Required.
	Publisher Publisher

	// Clock stamps [Proposal.ProposedAt] / [ToolProposed.ProposedAt].
	// Required.
	Clock Clock

	// IDGenerator mints [Proposal.ID]. Required.
	IDGenerator IDGenerator

	// IdentityResolver resolves [Proposal.ProposerID] from the
	// per-call ctx. Required.
	IdentityResolver IdentityResolver

	// Logger receives diagnostic log entries from [Proposer.Submit].
	// Optional; a nil [Logger] silently discards entries (no panic).
	Logger Logger
}

// Proposer is the M9.4.a authoring orchestrator: it validates a
// [ProposalInput], resolves the proposing agent identity, mints a
// durable [Proposal.ID], stamps the timestamp, and emits a
// [TopicToolProposed] event. The constructor panics on nil
// dependencies (mirror M9.1.a's `New` discipline).
type Proposer struct {
	deps ProposerDeps
}

// New constructs a [Proposer]. Panics with a named-field message
// when any required dependency in `deps` is nil; the panic discipline
// mirrors `toolregistry.New` (M9.1.a Pattern 2: seam-driven design,
// fail-loud on missing wiring).
func New(deps ProposerDeps) *Proposer {
	if deps.Publisher == nil {
		panic("approval: New: deps.Publisher must not be nil")
	}
	if deps.Clock == nil {
		panic("approval: New: deps.Clock must not be nil")
	}
	if deps.IDGenerator == nil {
		panic("approval: New: deps.IDGenerator must not be nil")
	}
	if deps.IdentityResolver == nil {
		panic("approval: New: deps.IdentityResolver must not be nil")
	}
	return &Proposer{deps: deps}
}

// Submit is the agent-facing entry point. It runs the resolution
// order documented at the top of this file:
//
//  1. [ProposalInput.Validate] — fail-fast BEFORE any side effects
//     (identity resolver call, clock read, id mint, publish).
//  2. ctx.Err pre-resolver — refuse a pre-cancelled ctx.
//  3. [IdentityResolver] — wrap resolver errors via
//     [ErrIdentityResolution]; refuse empty-on-success via
//     [ErrEmptyResolvedIdentity]; refuse over-long via
//     [ErrInvalidProposerID].
//  4. [Clock.Now] + [IDGenerator.NewUUID] — stamp the proposal.
//  5. Defensive deep-copy of [ProposalInput.Capabilities] so the
//     returned [Proposal] is independent of caller-side mutation.
//  6. ctx.Err pre-publish — refuse to land a phantom proposal
//     when the caller has cancelled mid-flight.
//  7. [Publisher.Publish] of [TopicToolProposed] using
//     [context.WithoutCancel] so the durable-record contract on
//     [ErrPublishToolProposed] is not racing the caller's cancel
//     against the eventbus's queue-depth-dependent send fast-path.
//     Failures wrap [ErrPublishToolProposed] (see godoc for
//     asymmetry vs `toolregistry`'s `ErrPublishAfterSwap`).
//
// On success, returns the populated [Proposal]. On failure,
// returns a zero [Proposal] and the sentinel-wrapped error.
func (p *Proposer) Submit(ctx context.Context, input ProposalInput) (Proposal, error) {
	// Pure-function validation runs BEFORE ctx.Err deliberately:
	// validation is cheaper than a syscall and gives callers a
	// stable validation-vs-cancel error-class boundary regardless of
	// whether ctx was cancelled pre-call.
	if err := input.Validate(); err != nil {
		return Proposal{}, err
	}
	if err := ctx.Err(); err != nil {
		return Proposal{}, err
	}
	proposerID, err := p.deps.IdentityResolver(ctx)
	if err != nil {
		return Proposal{}, fmt.Errorf("%w: %w", ErrIdentityResolution, err)
	}
	if proposerID == "" {
		return Proposal{}, ErrEmptyResolvedIdentity
	}
	if len(proposerID) > MaxProposerIDLength {
		return Proposal{}, fmt.Errorf("%w: %d bytes (max %d)", ErrInvalidProposerID, len(proposerID), MaxProposerIDLength)
	}
	now := p.deps.Clock.Now()
	id, err := p.deps.IDGenerator.NewUUID()
	if err != nil {
		return Proposal{}, fmt.Errorf("approval: id generator: %w", err)
	}
	prop := Proposal{
		ID:         id,
		ProposerID: proposerID,
		Input:      cloneProposalInput(input),
		ProposedAt: now,
		// CorrelationID is the canonical string form of ProposalID
		// (UUIDv7) — time-ordered AND unique by construction, so
		// downstream subscribers joining lifecycle events by
		// CorrelationID never collide unrelated proposals. Distinct
		// from `toolregistry.SourceSynced.CorrelationID` (which is
		// `time.Now().UnixNano()` because that package has no
		// per-event durable id to derive from); here we DO have one.
		CorrelationID: id.String(),
	}
	event := newToolProposedEvent(prop)
	// Refuse to land a phantom proposal on a cancelled ctx — the
	// proposer-internal work above is cheap to discard, and a caller
	// who has cancelled the request is signalling they no longer
	// want the proposal to land.
	if err := ctx.Err(); err != nil {
		return Proposal{}, err
	}
	// context.WithoutCancel inherits values for tracing / logging
	// propagation but detaches cancellation so the durable-record
	// publish completes deterministically regardless of mid-flight
	// caller cancel. See the M9.4.a iter-1 fix discussion for why
	// the eventbus's queue-depth-dependent send fast-path made the
	// race observable.
	publishCtx := context.WithoutCancel(ctx)
	if err := p.deps.Publisher.Publish(publishCtx, TopicToolProposed, event); err != nil {
		if p.deps.Logger != nil {
			p.deps.Logger.Log(
				ctx, "approval: publish tool_proposed failed",
				"proposal_id", prop.ID,
				"tool_name", prop.Input.Name,
				"proposer_id", prop.ProposerID,
				"err_type", fmt.Sprintf("%T", err),
			)
		}
		return Proposal{}, fmt.Errorf("%w: %w", ErrPublishToolProposed, err)
	}
	return prop, nil
}
