package peer

import "errors"

// ErrPeerTimeout is returned by [Tool.Ask] when no reply arrives
// before the per-call timeout elapses. Translates the underlying
// [k2k.ErrWaitForReplyTimeout] sentinel into a peer-tool-layer
// vocabulary so callers branch on errors.Is without importing the
// storage seam. The wrapped sentinel chain is preserved so
// `errors.Is(err, k2k.ErrWaitForReplyTimeout)` ALSO matches.
var ErrPeerTimeout = errors.New("peer: ask timed out")

// ErrPeerNotFound is returned by [Tool.Ask] when the supplied `target`
// (watchkeeper id or role name) does not resolve to any active peer
// over [keepclient.Client.ListPeers]. Mirrors the M1.3.a AC text
// "unknown target surfaces ErrPeerNotFound". A typo'd target, a peer
// that has gone inactive between resolver runs, or a future spawn-in-
// progress peer all surface as the same sentinel — the call surface
// is by design oblivious to the underlying not-active reason.
var ErrPeerNotFound = errors.New("peer: peer not found")

// ErrPeerCapabilityDenied is returned by [Tool.Ask] / [Tool.Reply]
// when the capability-broker gate refuses the supplied capability
// token. Wraps the underlying [capability.Err*] sentinels so the
// caller can branch on errors.Is without parsing the chain — both
// `errors.Is(err, peer.ErrPeerCapabilityDenied)` and
// `errors.Is(err, capability.ErrInvalidToken)` succeed.
var ErrPeerCapabilityDenied = errors.New("peer: capability denied")

// ErrPeerConversationClosed is returned by [Tool.Reply] when the
// conversation referenced by the supplied id has already been
// archived. Translates the underlying [k2k.ErrAlreadyArchived]
// sentinel so the peer-tool-layer caller does not depend on the
// storage seam's vocabulary; the chained sentinel keeps
// `errors.Is(err, k2k.ErrAlreadyArchived)` working.
var ErrPeerConversationClosed = errors.New("peer: conversation closed")

// ErrPeerConversationNotFound is returned by [Tool.Reply] when the
// supplied conversation id does not resolve to any persisted row.
// Translates the underlying [k2k.ErrConversationNotFound] sentinel.
var ErrPeerConversationNotFound = errors.New("peer: conversation not found")

// ErrInvalidTarget is returned by [Tool.Ask] when the supplied
// `target` is empty / whitespace-only. The validator runs BEFORE the
// capability gate so a malformed target fails fast without burning a
// broker round-trip.
var ErrInvalidTarget = errors.New("peer: invalid target")

// ErrInvalidSubject is returned by [Tool.Ask] when the supplied
// `subject` is empty / whitespace-only. The subject flows verbatim
// into [k2k.OpenParams.Subject] which the storage layer fail-fasts on
// via [k2k.ErrEmptySubject]; reproducing the check here lets the
// peer-tool-layer surface its own vocabulary without the caller
// importing the storage seam.
var ErrInvalidSubject = errors.New("peer: invalid subject")

// ErrInvalidBody is returned by [Tool.Ask] / [Tool.Reply] when the
// supplied body slice is empty. The peer-tool layer demands a
// non-empty payload — a zero-byte ask / reply is a degenerate state.
// Mirrors the [k2k.ErrEmptyMessageBody] discipline at the higher
// layer's vocabulary.
var ErrInvalidBody = errors.New("peer: invalid body")

// ErrInvalidTimeout is returned by [Tool.Ask] when the supplied
// timeout is non-positive. The M1.3.d `peer.Broadcast` fan-out passes
// `timeout=0` to mean "fire-and-forget"; M1.3.a's `Ask` requires a
// positive timeout because a blocking primitive without a bound is a
// caller bug.
var ErrInvalidTimeout = errors.New("peer: invalid timeout")

// ErrInvalidActingWatchkeeperID is returned by [Tool.Ask] / [Tool.Reply]
// when the supplied acting-watchkeeper id is empty / whitespace-only.
// The acting id is load-bearing for the M1.4 audit emission and for
// the M1.3.b participant-membership gate; an empty value at the entry
// boundary is a caller bug (a forgotten plumbing of the runtime
// identity through the tool-call stack).
var ErrInvalidActingWatchkeeperID = errors.New("peer: invalid acting watchkeeper id")

// ErrInvalidConversationID is returned by [Tool.Reply] / [Tool.Close]
// when the supplied conversation id is the zero UUID. The validator
// runs BEFORE the capability gate so a malformed id fails fast without
// burning a broker round-trip.
var ErrInvalidConversationID = errors.New("peer: invalid conversation id")

// ErrPeerClosePermission is returned by [Tool.Close] when the acting
// watchkeeper is not a participant in the target conversation. The
// participant set is the persisted [k2k.Conversation.Participants]
// list; the gate enforces that only members of the conversation may
// close it. Mirrors the closed-set acl discipline established by the
// M3.5.a `capability.Broker.ValidateForOrg` per-tenant gate, but at
// the conversation level — capability tokens express coarse "may use
// peer:close at all"; this sentinel expresses fine "is allowed to
// close THIS particular conversation". A non-participant attempt is a
// caller bug at the agent layer (an LLM hallucinating a target id) or
// a malicious cross-conversation poke; either way the call surfaces
// the typed sentinel rather than the underlying state.
var ErrPeerClosePermission = errors.New("peer: close permission denied")

// ErrInvalidEventID is returned by [EventBus.Publish] when the supplied
// [Event.ID] is the zero UUID. The id is minted by the publisher (not
// the bus) so a zero value is a caller bug; the validator runs BEFORE
// any persistence side effect.
var ErrInvalidEventID = errors.New("peer: invalid event id")

// ErrInvalidOrganizationID is returned by [EventBus.Publish] /
// [EventBus.Subscribe] when the supplied organization id is the zero
// UUID. Distinct from [k2k.ErrEmptyOrganization] because the peer-event
// layer's own seam owns its vocabulary — a future reader branching on
// the bus-level sentinel does not need to import the K2K storage seam.
// Mirrors the M1.3.a / M1.3.b "translate storage sentinels at the
// peer-tool boundary" discipline.
var ErrInvalidOrganizationID = errors.New("peer: invalid organization id")

// ErrEmptyWatchkeeperID is returned by [EventBus.Publish] when the
// supplied [Event.WatchkeeperID] is empty / whitespace-only. The
// watchkeeper id is the subscriber-side filter key; an empty value at
// the publish boundary is a caller bug.
var ErrEmptyWatchkeeperID = errors.New("peer: empty watchkeeper id")

// ErrEmptyEventType is returned by [EventBus.Publish] when the supplied
// [Event.EventType] is empty / whitespace-only. M1.3.c does not pin a
// finite enum here — downstream consumers (M1.4 audit, M5.* tool
// emitters) own their own type strings — but an empty type is a caller
// bug because the subscriber-side [SubscribeFilter.EventTypes] match
// would never succeed on an empty key.
var ErrEmptyEventType = errors.New("peer: empty event type")

// ErrPeerSubscriptionPermission is returned by [Tool.Subscribe] when
// the acting watchkeeper attempts to subscribe to events about a peer
// it is not authorised to observe. The current closed-set rule
// (M1.3.c): an empty `target` (subscribe to every event in the tenant)
// is allowed; a non-empty `target` MUST match the
// `ActingWatchkeeperID` (self-subscription only). Cross-peer
// subscription is a follow-up that requires a richer per-conversation
// participant gate (M1.4 will land it). A cross-peer attempt surfaces
// this sentinel WITHOUT leaking whether the foreign peer exists.
var ErrPeerSubscriptionPermission = errors.New("peer: subscribe permission denied")

// ErrInvalidEventTypes is returned by [Tool.Subscribe] when any entry
// in the supplied `eventTypes` slice is empty / whitespace-only. The
// caller is expected to pass canonical event-type strings (M1.4 audit
// taxonomy or M5.* tool emitter namespacing); an empty entry is a
// caller bug.
var ErrInvalidEventTypes = errors.New("peer: invalid event types")

// ErrPeerEventBusUnavailable is returned by [Tool.Subscribe] when the
// tool was constructed without an [EventBus] dep. The constructor's
// nil-guard catches this at wiring time; the runtime guard exists for
// defensive belt-and-braces during partial migrations.
var ErrPeerEventBusUnavailable = errors.New("peer: event bus unavailable")
