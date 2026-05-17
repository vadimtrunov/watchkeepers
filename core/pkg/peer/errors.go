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

// ErrInvalidConversationID is returned by [Tool.Reply] when the
// supplied conversation id is the zero UUID. The validator runs
// BEFORE the capability gate so a malformed id fails fast without
// burning a broker round-trip.
var ErrInvalidConversationID = errors.New("peer: invalid conversation id")
