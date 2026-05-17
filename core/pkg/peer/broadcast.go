// broadcast.go ships the M1.3.d `peer.Broadcast` fan-out primitive.
// Composes the M1.3.d [RoleFilter] resolver (over M1.2
// `keepclient.ListPeers`) with M1.3.a [Tool.Ask] in fire-and-forget
// mode. Every target receives an independent ask under a bounded
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
//	         [DefaultBroadcastConcurrency]) calling [Tool.Ask] with
//	         the supplied body and a non-zero per-target timeout
//	         (fire-and-forget at the broadcast layer — Ask's reply
//	         is collected into [BroadcastResultTarget.ConversationID]
//	         but the reply payload is discarded; a future M1.3.e may
//	         surface them) → collect per-target outcomes and return
//	         the aggregate [BroadcastResult].
//
// Fire-and-forget semantics: the roadmap AC names "timeout=0
// (fire-and-forget; no reply collection)". The peer-tool layer's
// M1.3.a `Ask` rejects a non-positive timeout via [ErrInvalidTimeout]
// because a blocking primitive without a bound is a caller bug; the
// broadcast layer therefore demands an explicit
// [BroadcastParams.PerTargetTimeout] and forwards it verbatim. The
// caller is responsible for sizing the bound — a typical operator
// broadcast uses 30s so a slow Slack adapter does not wedge the fan-
// out. The reply value from [Tool.Ask] is captured into the per-
// target outcome but the reply BODY is dropped (the broadcast layer
// is not a reply aggregator; a future M1.3.e may add one).
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
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
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
	// broker rejects the token. Note: the broadcast layer's capability
	// gate is INDEPENDENT of the per-target Ask's [CapabilityAsk]
	// gate; production wiring issues a separate token for each scope.
	CapabilityToken string

	// AskCapabilityToken is the per-call capability token forwarded to
	// every per-target [Tool.Ask]. Required (non-empty); a fan-out
	// without an Ask token would fail-fast inside Ask anyway, but
	// surfacing it as a separate field makes the wiring obvious.
	AskCapabilityToken string

	// Filter selects the target set over the [Lister.ListPeers] active
	// snapshot. Required (non-empty per [RoleFilter.validate]); the
	// tool fail-fasts via [ErrPeerRoleFilterEmpty] otherwise.
	Filter RoleFilter

	// Subject is the operator-facing free-text label persisted onto
	// every minted [k2k.Conversation.Subject]. Required (non-empty
	// after whitespace-trim); the tool fail-fasts via
	// [ErrInvalidSubject] otherwise.
	Subject string

	// Body is the opaque request payload forwarded verbatim to every
	// per-target [AskParams.Body]. Required (non-empty); the tool
	// fail-fasts via [ErrInvalidBody] otherwise. Defensively deep-
	// copied ONCE before the fan-out; the inner [Tool.Ask] performs
	// its own deep-copy so each target sees an independent slice.
	Body []byte

	// PerTargetTimeout caps each per-target Ask's WaitForReply
	// blocking window. Required (positive). The roadmap describes
	// the layer as "fire-and-forget; no reply collection" — the
	// reply payload is dropped at the broadcast layer but the
	// underlying Ask still needs a positive timeout because Ask
	// itself blocks. Callers driving the M1.6 escalation auto-broadcast
	// typically pass 30s.
	PerTargetTimeout time.Duration

	// Concurrency caps the worker-pool's in-flight Ask count. Zero
	// means "use [DefaultBroadcastConcurrency]". Negative values are
	// rejected via [ErrInvalidBroadcastConcurrency].
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
//     [ErrInvalidSubject], [ErrInvalidBody], [ErrInvalidTimeout],
//     [ErrInvalidBroadcastConcurrency], [ErrPeerRoleFilterEmpty]).
//   - Capability-broker rejection (broadcast scope) →
//     [ErrPeerCapabilityDenied] chained with the underlying
//     [capability.Err*] sentinel. The per-target Ask's capability
//     check runs INSIDE Ask; its rejection lands in the matching
//     [BroadcastResultTarget.Err] without aborting siblings.
//   - [FilterResolver.Resolve] non-empty error → wrapped through as
//     a top-level error (a resolver failure aborts the fan-out
//     because there is no target set to fall back on).
//   - Empty resolved set → [ErrPeerNoTargets] top-level.
//   - ctx cancellation BEFORE the fan-out starts → ctx.Err().
//   - ctx cancellation DURING the fan-out: in-flight Ask calls
//     observe the cancel via their own ctx; their results land in
//     the matching target's [Err] as `context.Canceled` /
//     `context.DeadlineExceeded`. The aggregate [Broadcast] return is
//     NOT a top-level error so the caller can inspect which targets
//     completed before the cancel.
//
// Per-target failures (Ask timeout, Ask capability deny, target
// resolution miss inside Ask) are observable on
// [BroadcastResultTarget.Err]. Callers walking the result set may
// branch on `errors.Is(target.Err, peer.ErrPeerTimeout)`,
// `errors.Is(target.Err, peer.ErrPeerCapabilityDenied)`, etc.
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
			res, askErr := t.Ask(ctx, AskParams{
				ActingWatchkeeperID: params.ActingWatchkeeperID,
				OrganizationID:      params.OrganizationID,
				CapabilityToken:     params.AskCapabilityToken,
				Target:              target.WatchkeeperID,
				Subject:             params.Subject,
				Body:                bodyCopy,
				Timeout:             params.PerTargetTimeout,
				CorrelationID:       params.CorrelationID,
			})
			if askErr != nil {
				results[i].Err = askErr
				return
			}
			results[i].ConversationID = res.ConversationID
			// reply body intentionally discarded — fire-and-forget
			// semantics at the broadcast layer.
		}()
	}
	wg.Wait()
	return BroadcastResult{Targets: results}, nil
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
	if params.PerTargetTimeout <= 0 {
		return ErrInvalidTimeout
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
