// subscribe.go ships the M1.3.c `peer.Subscribe` event-stream primitive.
// Composes the M1.3.c [EventBus.Subscribe] seam with the capability-
// broker gate. The built-in returns a read-only delivery channel +
// [CancelFunc]; the caller drains until the channel closes (ctx cancel
// or CancelFunc).
//
// resolution order:
//
//	Subscribe → ctx.Err → validate inputs → capability gate
//	         (peer:subscribe, per-tenant) → self-subscription gate
//	         (target == "" OR target == acting wk id) →
//	         EventBus.Subscribe(filter) → return (chan, CancelFunc, nil)
//
// Self-subscription gate (M1.3.c scope): the caller may either (a)
// subscribe to every event in its tenant (target=""), or (b) subscribe
// to events about itself (target=ActingWatchkeeperID). Cross-peer
// subscription requires a richer participant-membership gate which is
// the M1.4 audit subscriber's responsibility — at M1.3.c the seam is
// in place but the cross-peer surface stays denied via
// [ErrPeerSubscriptionPermission] so a future LLM hallucinating a peer
// id fails closed.
//
// audit discipline: this file does NOT import `keeperslog` and does NOT
// call `.Append(`. The K2K event taxonomy is owned by the M1.4 audit
// subscriber; this file is the call surface, not the audit sink. A
// source-grep AC test pins this.

package peer

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
)

// CapabilitySubscribe is the capability scope [Tool.Subscribe] gates
// against. Mirrors [CapabilityAsk] / [CapabilityReply] /
// [CapabilityClose]; the acting agent's Manifest must declare the
// capability under its `capabilities:` block and the runtime mints a
// per-call token scoped to this string via
// [capability.Broker.IssueForOrg].
const CapabilitySubscribe = "peer:subscribe"

// SubscribeParams is the closed-set input shape [Tool.Subscribe]
// accepts. Hoisted to a struct so a future addition (e.g. an explicit
// `Since` cursor for replay) lands as a new field rather than a
// breaking signature change. Mirrors the [AskParams] / [ReplyParams] /
// [CloseParams] discipline.
type SubscribeParams struct {
	// ActingWatchkeeperID is the id of the watchkeeper invoking the
	// subscribe. Required (non-empty after whitespace-trim); the tool
	// fail-fasts via [ErrInvalidActingWatchkeeperID] otherwise. Used as
	// the principal for the self-subscription gate.
	ActingWatchkeeperID string

	// OrganizationID is the verified tenant the acting watchkeeper
	// belongs to. Required (non-zero); the tool fail-fasts via
	// [k2k.ErrEmptyOrganization] otherwise.
	OrganizationID uuid.UUID

	// CapabilityToken is the per-call capability token bound to scope
	// [CapabilitySubscribe] + [OrganizationID]. Required (non-empty);
	// the tool fail-fasts via [ErrPeerCapabilityDenied] when the broker
	// rejects the token.
	CapabilityToken string

	// Target is the optional watchkeeper id filter. Empty = subscribe
	// to every event in the tenant. Non-empty MUST match
	// [ActingWatchkeeperID] (self-subscription only at M1.3.c);
	// cross-peer subscription surfaces [ErrPeerSubscriptionPermission].
	Target string

	// EventTypes is the optional event-type filter. `nil` / empty =
	// every event type. Each entry must be non-empty after whitespace-
	// trim; the tool fail-fasts via [ErrInvalidEventTypes] otherwise.
	// The slice is defensively deep-copied before forwarding to the
	// [EventBus] so caller-side mutation cannot bleed.
	EventTypes []string
}

// Subscribe runs the M1.3.c event-stream primitive. See the file-level
// doc-block for the resolution order; see [SubscribeParams] for the
// input shape.
//
// Failure modes:
//
//   - Validation failures surface their typed sentinel
//     ([ErrInvalidActingWatchkeeperID], [k2k.ErrEmptyOrganization],
//     [ErrInvalidEventTypes]).
//   - Capability-broker rejection → [ErrPeerCapabilityDenied] chained
//     with the underlying [capability.Err*] sentinel.
//   - Non-self target → [ErrPeerSubscriptionPermission].
//   - [EventBus.Subscribe] error → wrapped through.
//   - ctx cancellation → ctx.Err().
//
// The returned channel is closed when:
//
//   - the supplied `ctx` cancels;
//   - the returned [CancelFunc] is invoked;
//   - the underlying [EventBus] tears the subscription down (e.g. a
//     connection loss in the Postgres adapter).
//
// The caller MUST drain (or cancel) the subscription — a leaked
// goroutine inside the bus is a documented leak under the M1.3.c
// "cancel-leak test pins zero goroutines after 100 cycles" AC.
func (t *Tool) Subscribe(ctx context.Context, params SubscribeParams) (<-chan Event, CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(params.ActingWatchkeeperID) == "" {
		return nil, nil, ErrInvalidActingWatchkeeperID
	}
	if params.OrganizationID == uuid.Nil {
		return nil, nil, k2k.ErrEmptyOrganization
	}
	for _, et := range params.EventTypes {
		if strings.TrimSpace(et) == "" {
			return nil, nil, ErrInvalidEventTypes
		}
	}

	// Capability gate BEFORE any EventBus side effect (LISTEN acquire,
	// goroutine spawn). Mirrors `peer.Ask` / `peer.Reply` / `peer.Close`'s
	// fail-fast discipline.
	if err := t.deps.Capability.ValidateForOrg(
		ctx, params.CapabilityToken, CapabilitySubscribe, params.OrganizationID.String(),
	); err != nil {
		return nil, nil, translateCapabilityError(err)
	}

	// Self-subscription gate. Empty target subscribes to every event in
	// the tenant (the M1.4 audit subscriber consumes the stream this
	// way). Non-empty target MUST match ActingWatchkeeperID at M1.3.c.
	if params.Target != "" && params.Target != params.ActingWatchkeeperID {
		return nil, nil, ErrPeerSubscriptionPermission
	}

	if t.deps.EventBus == nil {
		// Defensive — production wiring panics at NewTool when EventBus
		// is nil under the M1.3.c surface, but a transitional caller may
		// pass nil during a partial migration. The constructor's nil
		// guard catches this; the runtime guard here is a belt-and-
		// braces.
		return nil, nil, ErrPeerEventBusUnavailable
	}

	filter := SubscribeFilter{
		OrganizationID:      params.OrganizationID,
		TargetWatchkeeperID: params.Target,
	}
	if len(params.EventTypes) > 0 {
		filter.EventTypes = make([]string, len(params.EventTypes))
		copy(filter.EventTypes, params.EventTypes)
	}

	ch, cancel, err := t.deps.EventBus.Subscribe(ctx, filter)
	if err != nil {
		return nil, nil, err
	}
	return ch, cancel, nil
}
