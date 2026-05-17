// broadcast.go ships the M1.3.d `peer.Broadcast` fan-out primitive.
// Composes the M1.3.d [RoleFilter] resolver (over M1.2
// `keepclient.ListPeers`) with the M1.1.c [Lifecycle.Open] +
// M1.3.a [Repository.AppendMessage] seams in fire-and-forget mode.
// Every target receives an independent request under a bounded
// worker pool; partial failures land in the returned
// [BroadcastResult.Targets] slice without aborting siblings.
//
// resolution order:
//
//	Broadcast → ctx.Err → validate inputs → capability gate
//	         (peer:broadcast, per-tenant) → resolve targets via
//	         [FilterResolver.Resolve] (apply RoleFilter +
//	         ExcludeSelf) → empty result surfaces [ErrPeerNoTargets]
//	         → fan out per-target via bounded worker pool (default
//	         [DefaultBroadcastConcurrency]) calling
//	         [Lifecycle.Open] + [Repository.AppendMessage] directly
//	         (NO WaitForReply, NO ListPeers re-resolve — the
//	         resolver already returned concrete targets) → collect
//	         per-target outcomes and return the aggregate
//	         [BroadcastResult].
//
// Fire-and-forget semantics: the roadmap AC names "fire-and-forget;
// no reply collection". The broadcast layer DELIBERATELY does NOT
// call [Tool.Ask] — Ask's M1.3.a contract blocks on WaitForReply,
// which would (a) defeat the one-way fan-out semantic and (b) waste
// `PerTargetTimeout` on each leg even when the recipient never
// replies. Instead, the broadcast layer mints the conversation +
// appends the request message inline and returns the minted
// conversation id on [BroadcastResultTarget.ConversationID] without
// blocking. The recipient may still emit a `peer.Reply` later; an
// operator can later open the conversation to inspect any reply via
// [k2k.Repository.Get] / [k2k.Repository.WaitForReply]. A future
// M1.3.e reply-aggregator leaf can add an optional collector
// without breaking the M1.3.d return shape.
//
// Single-snapshot fan-out: the per-target worker takes the already-
// resolved [keepclient.Peer] from [FilterResolver.Resolve] and
// drives [Lifecycle.Open] directly. Routing through [Tool.Ask] would
// trigger one extra [Lister.ListPeers] round-trip PER target (Ask's
// internal `resolvePeer`), turning a single-snapshot fan-out into
// N+1 remote lookups and exposing the fan-out to active-set churn
// mid-broadcast. The direct seam keeps the fan-out atomic in
// snapshot semantics.
//
// Bounded concurrency: the worker pool is implemented with a buffered
// semaphore channel of capacity [BroadcastParams.Concurrency]
// (defaults to [DefaultBroadcastConcurrency] when zero). Each worker
// acquires a slot BEFORE launching the Ask and releases it on Ask
// return. A 100-target broadcast with `Concurrency=8` therefore has
// at most 8 in-flight Asks at any moment. The bound protects (a) the
// Slack adapter rate limits, (b) the K2K Postgres connection pool,
// (c) the LLM-side token-budget runaway risk.
//
// audit discipline: this file does NOT import `keeperslog` and does
// NOT call `.Append(`. The K2K message-sent audit taxonomy is owned
// by the M1.4 audit subscriber; this file is the call surface, not
// the audit sink. A source-grep AC test pins this.
//
// PII discipline: the `Body` payload is treated as opaque bytes; the
// implementation defensively deep-copies the input ONCE before the
// fan-out so each per-target `Ask` receives an independent copy AND
// the caller cannot bleed by mutating the slice after Broadcast
// returns. Each per-target error is wrapped via `fmt.Errorf("...:
// %w", err)` so the underlying sentinel is preserved without the
// body bytes ever reaching the diagnostic chain.

package peer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// CapabilityBroadcast is the capability scope [Tool.Broadcast] gates
// against. Mirrors [CapabilityAsk] / [CapabilityReply] /
// [CapabilityClose] / [CapabilitySubscribe]; the acting agent's
// Manifest must declare the capability under its `capabilities:` block
// and the runtime mints a per-call token scoped to this string via
// [capability.Broker.IssueForOrg].
const CapabilityBroadcast = "peer:broadcast"

// DefaultBroadcastConcurrency is the worker-pool bound applied when
// [BroadcastParams.Concurrency] is zero. Chosen as a conservative
// default: large enough that an operator broadcast across a typical
// 20-30 active peers completes in a small multiple of the per-target
// p95 latency; small enough that a 200-peer broadcast does not
// stampede the Slack adapter / K2K Postgres pool. Callers may
// override per-broadcast via [BroadcastParams.Concurrency].
const DefaultBroadcastConcurrency = 8

// BroadcastParams is the closed-set input shape [Tool.Broadcast]
// accepts. Hoisted to a struct (rather than a long positional arg
// list) so a future addition (e.g. an explicit `ResponseAggregator`
// for the M1.3.e reply-collection follow-up) lands as a new field
// rather than a breaking signature change. Mirrors the [AskParams] /
// [ReplyParams] / [CloseParams] / [SubscribeParams] discipline.
type BroadcastParams struct {
	// ActingWatchkeeperID is the id of the watchkeeper invoking the
	// broadcast. Used as the [AskParams.ActingWatchkeeperID] on every
	// per-target Ask, and as the "self" reference for
	// [RoleFilter.ExcludeSelf]. Required (non-empty after whitespace-
	// trim); the tool fail-fasts via [ErrInvalidActingWatchkeeperID]
	// otherwise.
	ActingWatchkeeperID string

	// OrganizationID is the verified tenant the acting watchkeeper
	// belongs to. Required (non-zero); the tool fail-fasts via
	// [k2k.ErrEmptyOrganization] otherwise. Used to gate the
	// capability broker AND to scope every per-target Ask's K2K
	// conversation.
	OrganizationID uuid.UUID

	// CapabilityToken is the per-call capability token bound to scope
	// [CapabilityBroadcast] + [OrganizationID]. Required (non-empty);
	// the tool fail-fasts via [ErrPeerCapabilityDenied] when the
	// broker rejects the token. The broadcast layer's capability gate
	// is the ONLY capability check on the fan-out path; per-target
	// Asks are not routed through [Tool.Ask] (see file-level
	// doc-block) so no per-target [CapabilityAsk] token is required.
	CapabilityToken string

	// Filter selects the target set over the [Lister.ListPeers] active
	// snapshot. Required (non-empty per [RoleFilter.validate]); the
	// tool fail-fasts via [ErrPeerRoleFilterEmpty] otherwise.
	Filter RoleFilter

	// Subject is the operator-facing free-text label persisted onto
	// every minted [k2k.Conversation.Subject]. Required (non-empty
	// after whitespace-trim); the tool fail-fasts via
	// [ErrInvalidSubject] otherwise.
	Subject string

	// Body is the opaque request payload appended verbatim to every
	// minted conversation as a `request`-direction [k2k.Message].
	// Required (non-empty); the tool fail-fasts via [ErrInvalidBody]
	// otherwise. Defensively deep-copied ONCE before the fan-out; each
	// per-target worker copies again so each persisted message body
	// has its own slice.
	Body []byte

	// Concurrency caps the worker-pool's in-flight per-target count.
	// Zero means "use [DefaultBroadcastConcurrency]". Negative values
	// are rejected via [ErrInvalidBroadcastConcurrency].
	Concurrency int

	// CorrelationID is an optional id linking every minted
	// conversation to an upstream saga / Watch Order. `uuid.Nil` when
	// the caller has nothing to correlate. Forwarded verbatim to each
	// per-target [AskParams.CorrelationID].
	CorrelationID uuid.UUID
}

// BroadcastResultTarget is the per-target outcome of a fan-out call.
// One entry per resolved target — successful sends carry the minted
// [k2k.Conversation.ID] in [ConversationID]; failed sends carry the
// error in [Err]. Both fields are zero-value when the other is set.
type BroadcastResultTarget struct {
	// WatchkeeperID identifies the target peer.
	WatchkeeperID string

	// ConversationID is the id of the K2K conversation minted to
	// carry this fan-out leg. Zero ([uuid.Nil]) when [Err] is set.
	ConversationID uuid.UUID

	// Err is the per-target failure (validation, capability deny,
	// Ask timeout, downstream error). Nil on success.
	Err error
}

// BroadcastResult is the closed-set output shape [Tool.Broadcast]
// returns. Hoisted to a struct (rather than positional returns) so a
// future addition (e.g. an aggregate `RepliesByTarget` for M1.3.e)
// lands as a new field rather than a breaking signature change.
type BroadcastResult struct {
	// Targets carries one entry per resolved target. The slice is
	// always non-nil on a successful return; an empty slice never
	// reaches the caller because [ErrPeerNoTargets] is surfaced as a
	// top-level error instead. Sorted by [BroadcastResultTarget.WatchkeeperID]
	// so test assertions + operator dashboards see a stable order.
	Targets []BroadcastResultTarget
}

// ErrInvalidBroadcastConcurrency is returned by [Tool.Broadcast] when
// the supplied [BroadcastParams.Concurrency] is negative. A zero value
// is allowed (means "use [DefaultBroadcastConcurrency]"). A positive
// value caps the worker pool's in-flight Ask count.
var ErrInvalidBroadcastConcurrency = errors.New("peer: invalid broadcast concurrency")

// Broadcast runs the M1.3.d fan-out primitive. See the file-level
// doc-block for the resolution order; see [BroadcastParams] for the
// input shape and [BroadcastResult] for the output shape.
//
// Failure modes:
//
//   - Validation failures surface their typed sentinel
//     ([ErrInvalidActingWatchkeeperID], [k2k.ErrEmptyOrganization],
//     [ErrInvalidSubject], [ErrInvalidBody],
//     [ErrInvalidBroadcastConcurrency], [ErrPeerRoleFilterEmpty]).
//   - Capability-broker rejection (broadcast scope) →
//     [ErrPeerCapabilityDenied] chained with the underlying
//     [capability.Err*] sentinel. The fan-out is gated solely by the
//     [CapabilityBroadcast] scope; there is no per-target
//     [CapabilityAsk] check because the broadcast layer does NOT
//     route through [Tool.Ask].
//   - [FilterResolver.Resolve] non-empty error → wrapped through as
//     a top-level error (a resolver failure aborts the fan-out
//     because there is no target set to fall back on).
//   - Empty resolved set → [ErrPeerNoTargets] top-level.
//   - ctx cancellation BEFORE the fan-out starts → ctx.Err().
//   - ctx cancellation DURING the fan-out: the dispatcher stops
//     spawning new workers and surfaces `ctx.Err()` on every
//     not-yet-dispatched target's [Err]; in-flight workers observe
//     the cancel via their own ctx (their result lands as
//     `context.Canceled` / `context.DeadlineExceeded`). The
//     aggregate [Broadcast] return is `(result, nil)` so the caller
//     can inspect which targets completed before the cancel.
//
// Per-target failures ([k2k.Lifecycle.Open] error,
// [k2k.Repository.AppendMessage] error) are observable on
// [BroadcastResultTarget.Err] with the underlying sentinel preserved
// for `errors.Is` branching.
func (t *Tool) Broadcast(ctx context.Context, params BroadcastParams) (BroadcastResult, error) {
	if err := validateBroadcastParams(ctx, params); err != nil {
		return BroadcastResult{}, err
	}

	// Capability gate BEFORE any K2K state mutation OR resolver
	// round-trip. Mirrors the M1.3.a / M1.3.b / M1.3.c fail-fast
	// discipline.
	if err := t.deps.Capability.ValidateForOrg(
		ctx, params.CapabilityToken, CapabilityBroadcast, params.OrganizationID.String(),
	); err != nil {
		return BroadcastResult{}, translateCapabilityError(err)
	}

	// Resolve targets. Defensive deep-copy of the body happens AFTER
	// resolution so we do not pay the allocation cost on a fail-fast
	// path (empty filter, validation miss).
	targets, err := t.deps.FilterResolver.Resolve(ctx, params.Filter, params.ActingWatchkeeperID)
	if err != nil {
		return BroadcastResult{}, fmt.Errorf("peer: broadcast: resolve targets: %w", err)
	}
	if len(targets) == 0 {
		return BroadcastResult{}, ErrPeerNoTargets
	}

	// Defensive deep-copy of the body once — the inner Ask deep-copies
	// again per call, but copying here cheaply isolates every per-
	// target Ask from any post-Broadcast caller-side mutation.
	bodyCopy := make([]byte, len(params.Body))
	copy(bodyCopy, params.Body)

	concurrency := resolveConcurrency(params.Concurrency, len(targets))
	sem := make(chan struct{}, concurrency)
	results := make([]BroadcastResultTarget, len(targets))
	var wg sync.WaitGroup
	wg.Add(len(targets))
	for i, target := range targets {
		i, target := i, target
		results[i].WatchkeeperID = target.WatchkeeperID
		// Acquire a slot in the caller's goroutine BEFORE spawning the
		// worker so the in-flight count is correctly bounded. If the
		// ctx cancels while we are waiting on the semaphore, populate
		// the remaining slots with the ctx error and stop spawning.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			results[i].Err = ctx.Err()
			wg.Done()
			// Mark all remaining as ctx.Err and bail.
			for j := i + 1; j < len(targets); j++ {
				results[j].WatchkeeperID = targets[j].WatchkeeperID
				results[j].Err = ctx.Err()
				wg.Done()
			}
			// Drain the in-flight workers before returning so the
			// caller observes a settled result set.
			wg.Wait()
			return BroadcastResult{Targets: results}, nil
		}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			convID, err := t.broadcastSendOne(
				ctx,
				params.ActingWatchkeeperID,
				params.OrganizationID,
				params.Subject,
				bodyCopy,
				target,
				params.CorrelationID,
			)
			if err != nil {
				results[i].Err = err
				return
			}
			results[i].ConversationID = convID
		}()
	}
	wg.Wait()
	return BroadcastResult{Targets: results}, nil
}

// broadcastSendOne mints one per-target K2K conversation + appends
// the request message. Mirrors [Tool.Ask]'s mint+append pair MINUS
// the [Repository.WaitForReply] block — the broadcast layer is
// fire-and-forget at the request-side, NOT reply-side. The
// per-target body deep-copy is done here so each persisted message
// owns its own slice (the caller's `bodyCopy` is the broadcast-
// layer-wide copy; this per-target copy isolates the message
// payload from any sibling worker holding the same slice).
//
// The function is intentionally kept narrow: no capability gate (the
// outer [CapabilityBroadcast] gate already authorized the fan-out),
// no resolver round-trip (the caller supplies the resolved
// [keepclient.Peer]), no [Lifecycle.Close] on partial failure (the
// caller is responsible for the cleanup decision — a minted
// conversation that lost its AppendMessage is still observable via
// [k2k.Repository.Get] and can be archived via `peer.Close` if the
// operator wants).
func (t *Tool) broadcastSendOne(
	ctx context.Context,
	actingID string,
	orgID uuid.UUID,
	subject string,
	body []byte,
	target keepclient.Peer,
	correlationID uuid.UUID,
) (uuid.UUID, error) {
	conv, err := t.deps.Lifecycle.Open(ctx, k2k.OpenParams{
		OrganizationID: orgID,
		Participants:   []string{actingID, target.WatchkeeperID},
		Subject:        subject,
		CorrelationID:  correlationID,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("peer: broadcast: open conversation: %w", err)
	}
	reqBody := make([]byte, len(body))
	copy(reqBody, body)
	if _, err := t.deps.Repository.AppendMessage(ctx, k2k.AppendMessageParams{
		ConversationID:      conv.ID,
		OrganizationID:      orgID,
		SenderWatchkeeperID: actingID,
		Body:                reqBody,
		Direction:           k2k.MessageDirectionRequest,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("peer: broadcast: append request: %w", err)
	}
	return conv.ID, nil
}

// validateBroadcastParams runs every fast-fail check on a
// [BroadcastParams] BEFORE the capability gate. Hoisted so the
// public [Tool.Broadcast] stays under the gocyclo limit; the order
// of checks matches the resolution order in the doc-block (ctx,
// acting id, org, subject, body, timeout, concurrency, filter).
func validateBroadcastParams(ctx context.Context, params BroadcastParams) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(params.ActingWatchkeeperID) == "" {
		return ErrInvalidActingWatchkeeperID
	}
	if params.OrganizationID == uuid.Nil {
		return k2k.ErrEmptyOrganization
	}
	if strings.TrimSpace(params.Subject) == "" {
		return ErrInvalidSubject
	}
	if len(params.Body) == 0 {
		return ErrInvalidBody
	}
	if params.Concurrency < 0 {
		return ErrInvalidBroadcastConcurrency
	}
	return params.Filter.validate()
}

// resolveConcurrency picks the effective worker-pool bound:
//   - 0 → [DefaultBroadcastConcurrency];
//   - > targets → right-size to len(targets) so a small target set
//     does not over-allocate the semaphore channel buffer
//     (harmless for correctness, tidier).
//
// Negative values are rejected upstream by
// [validateBroadcastParams]; the helper trusts the caller's
// invariant.
func resolveConcurrency(requested, targets int) int {
	if requested == 0 {
		requested = DefaultBroadcastConcurrency
	}
	if requested > targets {
		return targets
	}
	return requested
}

// resolverFromLister exposes a fallback so callers that wire only the
// M1.3.a Ask/Reply surface can opt into broadcast by satisfying the
// `Lister` dep alone. The default-mode resolver wraps the lister with
// [NewFilterResolver]; production wiring SHOULD pass an explicit
// [FilterResolver] via [Deps.FilterResolver] so the seam is
// substitutable.
//
// Used by [NewTool] when [Deps.FilterResolver] is nil. Kept private
// because the substitution rule is a wiring-time invariant, not a
// caller-facing contract.
func resolverFromLister(lister Lister) FilterResolver {
	if lister == nil {
		return nil
	}
	return &defaultFilterResolver{lister: lister}
}

// ensure interface satisfaction at compile time so a future seam
// signature change trips here rather than at a downstream test.
var _ FilterResolver = (*defaultFilterResolver)(nil)
