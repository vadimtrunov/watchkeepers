package peer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/capability"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// Capability scopes the peer-tool layer issues + validates capability
// tokens under. Hoisted to constants so the [BuiltinAskManifest] /
// [BuiltinReplyManifest] manifest authors, the capability-broker gate
// callers, and any test harness share one source of truth.
const (
	// CapabilityAsk gates [Tool.Ask] calls. The acting agent's Manifest
	// must declare the capability under its `capabilities:` block; the
	// runtime mints a per-call token scoped to this string via
	// [capability.Broker.IssueForOrg] and the gate validates it via
	// [capability.Broker.ValidateForOrg].
	CapabilityAsk = "peer:ask"

	// CapabilityReply gates [Tool.Reply] calls. Same shape as
	// [CapabilityAsk].
	CapabilityReply = "peer:reply"
)

// Lister is the narrow seam [Tool.Ask] consumes from
// [keepclient.Client]. The interface is the unit-test seam:
// production wiring satisfies it with `*keepclient.Client`; tests
// inject a hand-rolled fake without standing up a Keep HTTP server.
//
// Mirrors the M1.1.c [k2k.SlackChannels] discipline: the consumer
// declares the minimum surface it needs, not the supplier's full
// method set.
type Lister interface {
	// ListPeers returns the platform-wide active-peer snapshot. The
	// implementation MUST guarantee a non-nil
	// [keepclient.ListPeersResponse.Items] slice on success (per the
	// M1.2 contract).
	ListPeers(ctx context.Context, req keepclient.ListPeersRequest) (*keepclient.ListPeersResponse, error)
}

// Lifecycle is the narrow seam [Tool.Ask] / [Tool.Close] consume from
// [k2k.Lifecycle]. Pinned to a method set rather than the concrete
// type so test wiring can inject a hand-rolled fake that mints +
// archives conversations without driving real Slack channel I/O.
type Lifecycle interface {
	// Open mints a fresh K2K conversation and returns the resulting
	// [k2k.Conversation]. The production [k2k.Lifecycle] composes the
	// repository [k2k.Repository.Open] with the Slack channel
	// provisioning + invite fan-out; test wiring can short-circuit to
	// a fake that mints a [k2k.Conversation] without Slack I/O.
	Open(ctx context.Context, params k2k.OpenParams) (k2k.Conversation, error)

	// Close archives the K2K conversation identified by `id` and
	// records `reason` as the close rationale. The production
	// [k2k.Lifecycle] composes the Slack [ArchiveChannel] (idempotent
	// on `already_archived`) with the repository [k2k.Repository.Close]
	// status transition. [Tool.Close] passes [CloseLifecycleReason] as
	// the reason on every call so the M1.7 archive-on-summary writer
	// can identify peer-tool-driven closes.
	//
	// Returns [k2k.ErrAlreadyArchived] when the row is already in
	// [k2k.StatusArchived] (a saga-replay race); [Tool.Close]
	// translates this to a nil return per the idempotent-close
	// contract.
	Close(ctx context.Context, id uuid.UUID, reason string) error
}

// Repository is the narrow seam [Tool.Ask] / [Tool.Reply] consume
// from [k2k.Repository]. Pinned to the message-side method set so
// tests can inject a focused fake without re-implementing the full
// conversation lifecycle (which is exercised through the [Lifecycle]
// seam).
type Repository interface {
	// Get resolves the conversation row matching `id` so [Tool.Reply]
	// can validate the row exists + is open BEFORE the capability
	// gate burns a broker round-trip.
	Get(ctx context.Context, id uuid.UUID) (k2k.Conversation, error)

	// AppendMessage persists a new [k2k.Message] keyed off
	// `params.ConversationID`. Production wiring drives a
	// [k2k.PostgresRepository] (over a per-call [k2k.Querier]); test
	// wiring drives a [k2k.MemoryRepository].
	AppendMessage(ctx context.Context, params k2k.AppendMessageParams) (k2k.Message, error)

	// WaitForReply blocks until a `reply`-direction [k2k.Message]
	// strictly after `since` appears on the conversation, or until
	// `timeout` elapses.
	WaitForReply(ctx context.Context, conversationID uuid.UUID, since time.Time, timeout time.Duration) (k2k.Message, error)

	// SetCloseSummary writes the operator-supplied summary onto an
	// already-archived conversation row. [Tool.Close] calls this AFTER
	// [Lifecycle.Close] transitions the row to [k2k.StatusArchived];
	// the seam is here on the peer-tool surface so test wiring can
	// inject a fake that does not maintain a real persistent row.
	SetCloseSummary(ctx context.Context, id uuid.UUID, summary string) error
}

// CapabilityValidator is the narrow seam [Tool.Ask] / [Tool.Reply]
// consume from [capability.Broker]. Pinned to a single method so
// tests can inject a hand-rolled fake without standing up a real
// broker (with its CSPRNG read + reaper goroutine).
//
// Implementations MUST treat the returned error as the authoritative
// admit / deny decision; a nil return is admit, any non-nil return is
// deny.
type CapabilityValidator interface {
	// ValidateForOrg matches the [capability.Broker.ValidateForOrg]
	// signature exactly so production wiring passes the broker
	// verbatim.
	ValidateForOrg(ctx context.Context, token, scope, organizationID string) error
}

// Deps is the closed-set dependency bundle [NewTool] accepts. Hoisted
// to a struct (rather than a long positional arg list) so a future
// addition (e.g. an audit subscriber callback when M1.4 lands) lands
// as a new field rather than a breaking signature change. Mirrors the
// saga-step `<Step>StepDeps` and [k2k.LifecycleDeps] patterns.
type Deps struct {
	// PeerLister resolves `target` (watchkeeper id or role name) over
	// [keepclient.Client.ListPeers]. Required (non-nil); [NewTool]
	// panics otherwise.
	PeerLister Lister

	// Lifecycle is the K2K conversation Open() seam — typically
	// [k2k.Lifecycle] in production. Required (non-nil); [NewTool]
	// panics otherwise.
	Lifecycle Lifecycle

	// Repository is the message-side seam — typically
	// [k2k.PostgresRepository] in production and
	// [k2k.MemoryRepository] in tests. Required (non-nil); [NewTool]
	// panics otherwise.
	Repository Repository

	// Capability is the capability-broker gate — typically
	// [*capability.Broker] in production. Required (non-nil);
	// [NewTool] panics otherwise.
	Capability CapabilityValidator

	// Now overrides the wall-clock used to compute the `since` cursor
	// passed to [Repository.WaitForReply]. Defaults to [time.Now]; a
	// test fixture may pass a deterministic clock. Optional.
	Now func() time.Time
}

// Tool is the M1.3.a peer.* orchestrator. Construct via [NewTool];
// the zero value is NOT usable — callers must always go through the
// constructor so the dependency invariants are enforced at
// construction time. Mirrors the saga-step + [k2k.Lifecycle]
// discipline of "panic on nil deps; never return a degenerate value
// from the constructor".
type Tool struct {
	deps Deps
	now  func() time.Time
}

// NewTool returns a configured [Tool]. Panics on any nil dep — every
// missing seam is a programmer bug at wiring time, not a runtime error
// to thread through Ask / Reply return values. The panic message names
// the offending field so an operator reading a stack trace can fix the
// wiring immediately. Mirrors the [k2k.NewLifecycle] discipline.
func NewTool(deps Deps) *Tool {
	if deps.PeerLister == nil {
		panic("peer: NewTool: deps.PeerLister must not be nil")
	}
	if deps.Lifecycle == nil {
		panic("peer: NewTool: deps.Lifecycle must not be nil")
	}
	if deps.Repository == nil {
		panic("peer: NewTool: deps.Repository must not be nil")
	}
	if deps.Capability == nil {
		panic("peer: NewTool: deps.Capability must not be nil")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Tool{deps: deps, now: now}
}

// translateCapabilityError lifts a [capability.Broker.ValidateForOrg]
// error into the peer-tool-layer vocabulary. The returned error
// chains both [ErrPeerCapabilityDenied] AND the underlying capability
// sentinel so callers can branch on either via errors.Is.
func translateCapabilityError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, capability.ErrInvalidToken),
		errors.Is(err, capability.ErrTokenExpired),
		errors.Is(err, capability.ErrScopeMismatch),
		errors.Is(err, capability.ErrOrganizationMismatch),
		errors.Is(err, capability.ErrInvalidOrganization),
		errors.Is(err, capability.ErrClosed):
		return fmt.Errorf("%w: %w", ErrPeerCapabilityDenied, err)
	default:
		// Unknown error shape — surface the same chained sentinel so
		// the caller's errors.Is branch stays uniform; the underlying
		// error preserves the diagnostic chain for logs.
		return fmt.Errorf("%w: %w", ErrPeerCapabilityDenied, err)
	}
}

// resolvePeer resolves `target` (watchkeeper id OR role name) into
// the matching [keepclient.Peer]. The resolver consults
// [Lister.ListPeers] for the platform-wide active-peer snapshot,
// then matches in two passes:
//
//  1. Exact uuid match against `Peer.WatchkeeperID` (a target that
//     parses as a UUID is treated as an id first).
//  2. Case-insensitive match against `Peer.Role` (operator-friendly:
//     a `target` of "lead" matches role "Lead").
//
// Returns [ErrPeerNotFound] when neither pass yields a hit. The
// resolver does NOT cache — every call re-fetches the active peer
// list so a freshly-spawned peer resolves on its first try.
func (t *Tool) resolvePeer(ctx context.Context, target string) (keepclient.Peer, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return keepclient.Peer{}, ErrInvalidTarget
	}
	resp, err := t.deps.PeerLister.ListPeers(ctx, keepclient.ListPeersRequest{})
	if err != nil {
		return keepclient.Peer{}, fmt.Errorf("peer: resolve target: %w", err)
	}
	// First pass: uuid match. Match against the raw Peer.WatchkeeperID
	// string so a hypothetical M3.5 future id shape (non-uuid) still
	// matches verbatim.
	for _, p := range resp.Items {
		if p.WatchkeeperID == target {
			return p, nil
		}
	}
	// Second pass: case-insensitive role match. The role column is
	// operator-facing (`manifest.display_name`) so the operator-facing
	// vocabulary case-folds.
	lower := strings.ToLower(target)
	for _, p := range resp.Items {
		if strings.EqualFold(p.Role, target) || strings.ToLower(p.Role) == lower {
			return p, nil
		}
	}
	return keepclient.Peer{}, fmt.Errorf("%w: %s", ErrPeerNotFound, target)
}
