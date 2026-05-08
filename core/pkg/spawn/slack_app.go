package spawn

import (
	"context"
	"errors"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	"github.com/vadimtrunov/watchkeepers/core/pkg/secrets"
)

// authorityActionSlackAppCreate is the action string the M6.1.a
// Watchmaster manifest seed publishes in `authority_matrix` to gate
// Slack App creation. Hoisted to a package constant so a re-key from
// `slack_app_create` (e.g., a future per-platform suffix) is a
// one-line change here that the gate AND the keepers-log payload
// pick up via the compiler.
const authorityActionSlackAppCreate = "slack_app_create"

// authorityValueLeadApproval is the M5.5.b.c.c.b enum value the
// authority_matrix carries when a privileged action requires the
// human lead's approval. Mirrors the M6.1.a seed migration
// (`017_watchmaster_manifest_seed.sql`) and the rest of the Phase 1
// authority vocabulary. The constant is defined locally rather than
// importing the runtime package because the runtime imports
// keeperslog and we want to keep the spawn package light.
const authorityValueLeadApproval = "lead_approval"

// keepersLogEventTypeRequested / keepersLogEventTypeSucceeded /
// keepersLogEventTypeFailed are the event_type values the privileged
// RPC emits on the audit trail. Pinned per AC2 / M6.1.b TASK Scope.
// Project convention: snake_case `<noun>_<verb>` past tense.
const (
	keepersLogEventTypeRequested = "watchmaster_slack_app_create_requested"
	keepersLogEventTypeSucceeded = "watchmaster_slack_app_create_succeeded"
	keepersLogEventTypeFailed    = "watchmaster_slack_app_create_failed"
)

// payloadKeyToolName is the snake_case payload key carrying the
// invoked tool name on the keepers_log row. Hoisted because the
// requested / succeeded / failed payloads share it.
const (
	payloadKeyAgentID    = "agent_id"
	payloadKeyAppName    = "app_name"
	payloadKeyToolName   = "tool_name"
	payloadKeyEventClass = "event_class"
	payloadKeyErrorClass = "error_class"

	payloadValueToolName       = "slack_app_create"
	payloadValueEventRequested = "requested"
	payloadValueEventSucceeded = "succeeded"
	payloadValueEventFailed    = "failed"
)

// secretsKeySlackAppConfigToken is the [secrets.SecretSource] key the
// privileged RPC consults for the `xoxe-*` Slack app configuration
// token `apps.manifest.create` requires. Hoisted to a package
// constant so the wiring layer (M6.1.b boot path / M6.3 deploy
// scripts) and the call site stay in sync. Future production wiring
// (Vault, AWS Secrets Manager) substitutes a different
// [secrets.SecretSource]; the key is the same.
//
//nolint:gosec // G101: env var name, not a credential
const secretsKeySlackAppConfigToken = "SLACK_APP_CONFIG_TOKEN"

// Claim captures the auth tuple the caller forwards on every
// privileged RPC call. The matrix value is consulted via [Claim.HasAuthority].
//
// The shape mirrors the runtime.Manifest projection of the same name
// (see core/pkg/runtime/runtime.go: `AuthorityMatrix map[string]string`)
// so Watchmaster wiring (M6.2/M6.3) can pass the runtime claim
// straight into the privileged RPC without restructuring. The
// OrganizationID field exists because every Phase 1 audit row is
// tenant-scoped (M3.5.a discipline).
type Claim struct {
	// OrganizationID is the tenant the call is scoped to. Required;
	// empty values fail synchronously with [ErrInvalidClaim].
	OrganizationID string

	// AgentID is the manifest-projected id of the calling agent
	// (typically Watchmaster's manifest id from M6.1.a). The RPC
	// uses it for the audit payload's `agent_id` field; it is NOT
	// consulted for authorization. Required only insofar as the
	// audit payload would otherwise carry an empty agent_id.
	AgentID string

	// AuthorityMatrix is the manifest-projected authority matrix
	// the gate consults. The action lookup is `slack_app_create`;
	// any value other than `lead_approval` fails closed with
	// [ErrUnauthorized].
	AuthorityMatrix map[string]string
}

// HasAuthority reports whether `c` carries the lead-approval entry
// for `action`. The check is a strict equality against the
// [authorityValueLeadApproval] enum value; absent entries, empty
// values, and unknown enum values all return false.
//
// Centralised here so future actions (M6.2's `watchkeeper_retire`,
// `manifest_version_bump`, etc.) reuse the same gate.
func (c Claim) HasAuthority(action string) bool {
	if c.AuthorityMatrix == nil {
		return false
	}
	return c.AuthorityMatrix[action] == authorityValueLeadApproval
}

// CreateAppRequest is the value supplied to [SlackAppRPC.CreateApp].
// Captures the caller-supplied manifest fields (forwarded to the
// underlying [messenger.Adapter.CreateApp]) plus the opaque
// [ApprovalToken] M6.3 will populate via the lead-approval saga. The
// shape is intentionally portable; the platform-specific knobs
// (`xoxe-*` config token, OAuth scopes encoding) ride through the
// configured [secrets.SecretSource] and the [messenger.AppManifest]
// Metadata bag respectively.
type CreateAppRequest struct {
	// AgentID is the calling agent's id, copied to the audit
	// payload. Required; empty fails synchronously with
	// [ErrInvalidRequest].
	AgentID string

	// AppName is the Slack app's display name. Forwarded to
	// [messenger.AppManifest.Name]. Required; empty fails
	// synchronously with [ErrInvalidRequest] (Slack would reject
	// the call anyway with the same code, but catching it
	// client-side avoids burning a tier-2 rate-limit token).
	AppName string

	// AppDescription is the Slack app's long-form description.
	// Forwarded to [messenger.AppManifest.Description]. Optional.
	AppDescription string

	// Scopes is the list of OAuth bot scopes the app requests at
	// install time. Forwarded to [messenger.AppManifest.Scopes].
	// Optional; empty produces an app with no bot scopes.
	Scopes []string

	// ApprovalToken is the opaque token the lead-approval saga
	// (M6.3) issues. The M6.1.b validation is non-empty only;
	// M6.3 owns the cryptography. Required; empty fails with
	// [ErrApprovalRequired] AFTER the authority-matrix gate
	// passes (so a forbidden caller is told `unauthorized`,
	// not `approval required`).
	ApprovalToken string
}

// CreateAppResult is the value returned from a successful
// [SlackAppRPC.CreateApp]. Carries the platform-assigned
// [messenger.AppID] verbatim; credentials ride through the configured
// [slack.CreateAppCredsSink] out of band per the M4.2.d.2 design.
type CreateAppResult struct {
	// AppID is the platform-assigned app identifier the underlying
	// adapter returned. Always populated on success.
	AppID messenger.AppID
}

// SlackAppRPC is the privileged-RPC surface the Watchmaster meta-
// agent calls (via the M5.5.d harnessrpc seam) to provision Slack
// apps. The interface exists so future wiring (M6.2 supervisor, M6.3
// operator surface) can mock the RPC at the seam without standing
// up the underlying [messenger.Adapter] + [secrets.SecretSource] +
// [keeperslog.Writer] stack.
type SlackAppRPC interface {
	// CreateApp validates the claim's authority + the request's
	// approval token, reads the Slack app-config token via the
	// configured [secrets.SecretSource], emits a
	// `watchmaster_slack_app_create_requested` keepers_log event,
	// calls the underlying [messenger.Adapter.CreateApp], and
	// emits a `watchmaster_slack_app_create_succeeded` (or
	// `_failed`) event before returning.
	//
	// Error return:
	//   - [ErrInvalidClaim]      — claim has empty OrganizationID
	//   - [ErrUnauthorized]      — claim lacks lead_approval
	//   - [ErrApprovalRequired]  — request has empty ApprovalToken
	//   - [ErrInvalidRequest]    — request has empty AgentID/AppName
	//   - underlying secrets / messenger / keeperslog errors,
	//     wrapped with `spawn:` so callers can errors.Is the
	//     specific sentinel.
	CreateApp(ctx context.Context, req CreateAppRequest, claim Claim) (CreateAppResult, error)
}

// MessengerAdapter is the minimal subset of [messenger.Adapter] the
// privileged RPC consumes — only [messenger.Adapter.CreateApp].
// Defined locally so tests can substitute a tiny fake without
// implementing the full six-method [messenger.Adapter] surface.
// Mirrors the keeperslog.LocalKeepClient / runtime.Embedder
// import-cycle-break pattern documented in `docs/LESSONS.md`.
type MessengerAdapter interface {
	CreateApp(ctx context.Context, manifest messenger.AppManifest) (messenger.AppID, error)
}

// Compile-time assertion: every [messenger.Adapter] satisfies the
// minimal [MessengerAdapter] subset by definition. Pins the
// integration shape against future drift in the messenger package.
var _ MessengerAdapter = messenger.Adapter(nil)

// keepersLogAppender is the minimal subset of [keeperslog.Writer]
// the privileged RPC consumes — only [keeperslog.Writer.Append].
// Defined locally so tests can substitute a tiny fake (the e2e tests
// use a real `*keeperslog.Writer` over a recording fake
// LocalKeepClient per AC5; the unit tests stub at this seam).
type keepersLogAppender interface {
	Append(ctx context.Context, evt keeperslog.Event) (string, error)
}

// Compile-time assertion: the production [*keeperslog.Writer]
// satisfies [keepersLogAppender]. Pins the integration shape.
var _ keepersLogAppender = (*keeperslog.Writer)(nil)

// slackAppRPC is the production [SlackAppRPC] returned from
// [NewSlackAppRPC]. Holds only immutable configuration after the
// constructor returns; safe for concurrent use across goroutines.
type slackAppRPC struct {
	adapter   MessengerAdapter
	secrets   secrets.SecretSource
	logger    keepersLogAppender
	configKey string
}

// Option configures a [SlackAppRPC] at construction time. Pass
// options to [NewSlackAppRPC]; later options override earlier ones
// for the same field.
type Option func(*slackAppRPC)

// WithSlackConfigTokenKey overrides the [secrets.SecretSource] key
// the RPC consults for the `xoxe-*` Slack app configuration token.
// Defaults to [secretsKeySlackAppConfigToken] (= `SLACK_APP_CONFIG_TOKEN`).
// An empty string is a no-op so callers can always pass through
// whatever they have.
//
// Production wiring (M6.3) typically keeps the default; tests use
// this option to drive a stub [secrets.SecretSource] without
// hard-coding the production key.
func WithSlackConfigTokenKey(key string) Option {
	return func(r *slackAppRPC) {
		if key != "" {
			r.configKey = key
		}
	}
}

// NewSlackAppRPC constructs a privileged [SlackAppRPC] backed by the
// supplied [MessengerAdapter] (typically `*slack.Client`),
// [secrets.SecretSource] (typically `*secrets.EnvSource` in dev or a
// Vault / AWS Secrets Manager source in production), and
// [*keeperslog.Writer] for the audit chain.
//
// `adapter`, `src`, and `logger` MUST be non-nil; passing a nil
// dependency is a programmer error and panics with a clear
// message — matches the panic discipline of [keeperslog.New],
// [runtime.NewToolErrorReflector], and [lifecycle.New]. An RPC with
// no adapter / secrets / writer cannot satisfy any contract, and
// silently no-oping every call would mask the bug.
func NewSlackAppRPC(
	adapter MessengerAdapter,
	src secrets.SecretSource,
	logger *keeperslog.Writer,
	opts ...Option,
) SlackAppRPC {
	if adapter == nil {
		panic("spawn: NewSlackAppRPC: adapter must not be nil")
	}
	if src == nil {
		panic("spawn: NewSlackAppRPC: secrets source must not be nil")
	}
	if logger == nil {
		panic("spawn: NewSlackAppRPC: keeperslog writer must not be nil")
	}
	r := &slackAppRPC{
		adapter:   adapter,
		secrets:   src,
		logger:    logger,
		configKey: secretsKeySlackAppConfigToken,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// CreateApp implements [SlackAppRPC.CreateApp].
//
// Resolution order (pinned by the M6.1.b acceptance criteria):
//
//  1. Validate ctx (cancellation takes precedence over input shape).
//  2. Validate claim.OrganizationID non-empty (M3.5.a discipline) →
//     [ErrInvalidClaim].
//  3. Validate claim.AuthorityMatrix["slack_app_create"] equals
//     "lead_approval" → [ErrUnauthorized] otherwise.
//  4. Validate req.AgentID + req.AppName non-empty →
//     [ErrInvalidRequest] otherwise.
//  5. Validate req.ApprovalToken non-empty → [ErrApprovalRequired]
//     otherwise. (Order matters: the gate runs BEFORE the token
//     check so a forbidden caller sees `unauthorized` rather than
//     `approval required`.)
//  6. Read the `xoxe-*` config token via the configured
//     [secrets.SecretSource]. A miss surfaces wrapped as
//     `spawn: secrets: …`.
//  7. Emit `watchmaster_slack_app_create_requested` keepers_log
//     event. Append failure surfaces wrapped — the RPC does NOT
//     proceed to call the adapter when the audit row could not be
//     written (privileged RPC contract: no privileged action
//     without a paired audit row).
//  8. Call adapter.CreateApp.
//  9. On success, emit `watchmaster_slack_app_create_succeeded` and
//     return the [CreateAppResult]. On failure, emit
//     `watchmaster_slack_app_create_failed` and return the wrapped
//     adapter error.
//
// IMPORTANT (token discipline): the resolved config token is NEVER
// embedded in any keepers_log payload, log entry, or returned value.
// Phase 1 reads it solely for its presence — production wiring will
// thread it into the underlying adapter's [TokenSource] via the same
// pattern documented in core/pkg/messenger/slack/create_app.go.
func (r *slackAppRPC) CreateApp(
	ctx context.Context,
	req CreateAppRequest,
	claim Claim,
) (CreateAppResult, error) {
	if err := ctx.Err(); err != nil {
		return CreateAppResult{}, err
	}
	if claim.OrganizationID == "" {
		return CreateAppResult{}, fmt.Errorf("%w: empty OrganizationID", ErrInvalidClaim)
	}
	if !claim.HasAuthority(authorityActionSlackAppCreate) {
		return CreateAppResult{}, ErrUnauthorized
	}
	if req.AgentID == "" {
		return CreateAppResult{}, fmt.Errorf("%w: empty AgentID", ErrInvalidRequest)
	}
	if req.AppName == "" {
		return CreateAppResult{}, fmt.Errorf("%w: empty AppName", ErrInvalidRequest)
	}
	if req.ApprovalToken == "" {
		return CreateAppResult{}, ErrApprovalRequired
	}

	// Read the platform config token through the secret source. The
	// value is intentionally discarded — Phase 1 verifies presence
	// but does not yet thread the token onto a per-call adapter
	// override. M6.3 deploy scripts will wire the token onto the
	// adapter's [TokenSource]; the RPC's job here is to fail fast
	// when no token is configured rather than burn a Slack call on
	// a known-bad request.
	if _, err := r.secrets.Get(ctx, r.configKey); err != nil {
		return CreateAppResult{}, fmt.Errorf("spawn: secrets: %w", err)
	}

	// Emit the `requested` audit event BEFORE the adapter call. A
	// failure here aborts the RPC — the privileged-action contract
	// says: no audit row, no privileged action. Mirrors the
	// M5.6.c precedent (Append BEFORE Embed so a failed embed does
	// not silently skip the audit row).
	if _, err := r.logger.Append(ctx, requestedEvent(req, claim)); err != nil {
		return CreateAppResult{}, fmt.Errorf("spawn: keepers_log requested: %w", err)
	}

	// Privileged call.
	appID, err := r.adapter.CreateApp(ctx, messenger.AppManifest{
		Name:        req.AppName,
		Description: req.AppDescription,
		Scopes:      req.Scopes,
	})
	if err != nil {
		// Best-effort emit `failed` on the audit chain. A failure
		// here is logged via the keeperslog Writer's own logger
		// (the one wired at *keeperslog.Writer construction time)
		// and does not mask the original adapter error — the
		// caller's first-class signal is the adapter's failure.
		_, _ = r.logger.Append(ctx, failedEvent(req, claim, err))
		return CreateAppResult{}, fmt.Errorf("spawn: adapter create_app: %w", err)
	}

	// Best-effort emit `succeeded`. Same rationale as the failed
	// branch: the adapter has already mutated the platform side; an
	// audit-write failure here is a pure observability loss, not a
	// privilege-boundary violation.
	_, _ = r.logger.Append(ctx, succeededEvent(req, claim, appID))

	return CreateAppResult{AppID: appID}, nil
}

// requestedEvent builds the `requested` audit row payload. Pulled
// into a helper so the keys / shape stay scannable and the
// payload-key constants live next to a single use site.
func requestedEvent(req CreateAppRequest, claim Claim) keeperslog.Event {
	return keeperslog.Event{
		EventType: keepersLogEventTypeRequested,
		Payload: map[string]any{
			payloadKeyAgentID:    pickAgentID(req, claim),
			payloadKeyAppName:    req.AppName,
			payloadKeyToolName:   payloadValueToolName,
			payloadKeyEventClass: payloadValueEventRequested,
		},
	}
}

// succeededEvent builds the `succeeded` audit row payload. Carries
// the platform-assigned app id so a downstream consumer reading the
// log without joining other tables can correlate `requested` →
// `succeeded` rows by their shared correlation_id (the writer
// stamps it from ctx) AND by the per-row app_id.
func succeededEvent(req CreateAppRequest, claim Claim, appID messenger.AppID) keeperslog.Event {
	return keeperslog.Event{
		EventType: keepersLogEventTypeSucceeded,
		Payload: map[string]any{
			payloadKeyAgentID:    pickAgentID(req, claim),
			payloadKeyAppName:    req.AppName,
			payloadKeyToolName:   payloadValueToolName,
			payloadKeyEventClass: payloadValueEventSucceeded,
			"app_id":             string(appID),
		},
	}
}

// failedEvent builds the `failed` audit row payload. Carries the
// Go type of the underlying error in the `error_class` field so a
// downstream Recall query can group by failure mode without parsing
// the free-form `error_message` string. The error VALUE is NOT
// written to the audit row — mirrors the M3.4.b redaction discipline
// (logs are bounded; authoritative error storage lives elsewhere).
func failedEvent(req CreateAppRequest, claim Claim, err error) keeperslog.Event {
	return keeperslog.Event{
		EventType: keepersLogEventTypeFailed,
		Payload: map[string]any{
			payloadKeyAgentID:    pickAgentID(req, claim),
			payloadKeyAppName:    req.AppName,
			payloadKeyToolName:   payloadValueToolName,
			payloadKeyEventClass: payloadValueEventFailed,
			payloadKeyErrorClass: classifyError(err),
		},
	}
}

// pickAgentID prefers the request's AgentID over the claim's, since
// the request is the inner-most caller-supplied value. Falls back
// to the claim's AgentID for callers that route via a Watchmaster
// claim and leave the request's AgentID empty.
func pickAgentID(req CreateAppRequest, claim Claim) string {
	if req.AgentID != "" {
		return req.AgentID
	}
	return claim.AgentID
}

// classifyError extracts a stable string suitable for the audit
// row's `error_class` slot. Prefers the wrapped error's Go type
// name; the unwrap loop reaches past the spawn-package wrap so the
// audit reflects the underlying cause (e.g.,
// `*messenger.APIError`), not the outer `*fmt.wrapError`.
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	cur := err
	for {
		next := errors.Unwrap(cur)
		if next == nil {
			return fmt.Sprintf("%T", cur)
		}
		cur = next
	}
}
