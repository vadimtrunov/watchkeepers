// Doc-block at file head documenting the seam contract.
//
// resolution order: nil-dep check (panic) → http body read (MaxBytesReader-
// capped) → header presence (X-Watchkeeper-Signature-256 +
// X-Watchkeeper-Request-Timestamp) → secret resolution
// (ErrWebhookSecretResolution / ErrWebhookEmptySecret) → HMAC-SHA256
// constant-time compare (ErrWebhookBadSignature) → timestamp window
// (ErrWebhookStaleTimestamp) → JSON decode (ErrWebhookMalformedPayload)
// → proposal lookup (ErrProposalNotFound pass-through; HTTP 404) →
// branch on action ∈ {approved, rejected} → on approved:
// SourceForTarget(target) → SchedulerSyncer.SyncOnce(source) →
// ctx.Err pre-publish → Publisher.Publish(TopicToolApproved) → on
// rejected: ctx.Err pre-publish → Publisher.Publish(TopicToolRejected).
//
// audit discipline: the webhook never imports `keeperslog` and never
// calls `.Append(` (see source-grep AC). The audit log entry for the
// `tool_approved` / `tool_rejected` decision lives in the M9.7 audit
// subscriber that observes the eventbus topics; the webhook itself
// emits the event AND surfaces the HTTP response, with no audit
// side-effect at this layer.
//
// PII discipline: the webhook payload carries opaque identifiers only
// (proposal id, action, optional pr url, optional merged sha, approver
// id, timestamp). The optional [Logger] is invoked with the request id
// + proposal id + decision route — never with the [Proposal] body
// (`Purpose`, `PlainLanguageDescription`, `CodeDraft`). The HMAC
// compare uses [hmac.Equal] (constant-time) to avoid leaking timing
// information about the secret-bound digest. The signing-secret bytes
// are resolved per-call so a per-tenant rotation takes effect on the
// next webhook delivery; the resolved value is held only on the
// function-local stack.

package approval

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Webhook request headers. Mirror Slack's
// `X-Slack-Signature` / `X-Slack-Request-Timestamp` discipline; the
// `watchkeeper`-prefixed names exist so a future co-installation of
// the platform on the same ingress URL does not collide with a
// concurrently-mounted Slack handler. The `-256` suffix on the
// signature header explicitly names the digest algorithm so a future
// migration to SHA-384 / SHA-512 is additive (a new
// `X-Watchkeeper-Signature-384` header alongside, never replacing).
const (
	headerWebhookSignature = "X-Watchkeeper-Signature-256"
	headerWebhookTimestamp = "X-Watchkeeper-Request-Timestamp"
)

// webhookSignaturePrefix is the literal `sha256=` prefix every
// `X-Watchkeeper-Signature-256` header value carries. Aligns with the
// GitHub `X-Hub-Signature-256` convention so an operator who has
// configured a GitHub workflow to post the webhook can re-use the
// platform-standard signer.
const webhookSignaturePrefix = "sha256="

// webhookSignatureBaseSeparator is the colon glue between the
// timestamp and the body in the signing string `<ts>:<raw_body>`.
// Hoisted so the algorithm reads like the spec.
const webhookSignatureBaseSeparator = ":"

// defaultWebhookTimestampWindow is the maximum tolerated absolute
// drift between the X-Watchkeeper-Request-Timestamp header and the
// local clock. 5 minutes matches Slack's published replay-attack
// guidance and the [slack/inbound.defaultTimestampWindow].
const defaultWebhookTimestampWindow = 5 * time.Minute

// defaultWebhookMaxBodyBytes caps inbound webhook bodies at 1 MiB.
// Webhook payloads are JSON metadata (proposal id, action, pr url,
// merged sha, approver, timestamp) — well under this cap in practice.
// 1 MiB leaves headroom for future payload extensions while bounding
// memory pressure on the handler.
const defaultWebhookMaxBodyBytes int64 = 1 << 20

// Closed-set webhook action vocabulary. The receiver branches on this
// string; a new action requires a new branch in [Webhook.serveHTTP]
// and a new audit-row reason — so the operator MUST update both sides
// together.
const (
	webhookActionApproved = "approved"
	webhookActionRejected = "rejected"
)

// SchedulerSyncer is the seam the webhook consumes to drive the
// [toolregistry.Scheduler.SyncOnce] re-sync after a `tool_approved`
// decision. Defined as a single-method interface so tests substitute a
// hand-rolled fake without standing up the full scheduler.
//
// Contract:
//
//   - Return nil on success — the source has been re-synced and the
//     M9.1.b registry rebuilder will observe the resulting
//     `source_synced` event.
//   - Return any error on failure — the webhook receiver wraps with
//     [ErrSchedulerSync] so callers can match via [errors.Is]. A sync
//     failure DOES NOT abort the publish: the approval is durable on
//     the event stream regardless of whether the sync succeeded; the
//     M9.7 audit subscriber records the failure for operator triage.
type SchedulerSyncer interface {
	SyncOnce(ctx context.Context, sourceName string) error
}

// WebhookSecretResolver is the per-call seam for resolving the
// HMAC-signing secret bytes from the request context. Mirrors
// [IdentityResolver]'s "function shape" + per-call invocation
// discipline (M9.1.a / M9.4.a pattern): a per-tenant rotation takes
// effect on the next webhook delivery without a process restart, and
// a per-deployment secret never becomes a process-global static.
//
// Contract:
//
//   - Return a non-empty byte slice on success. The returned bytes are
//     consumed for a single HMAC compare and discarded; the resolver
//     MAY return a fresh slice on each call.
//   - Return `(nil, <cause>)` on resolution failure (e.g. secret
//     store unavailable). The webhook receiver wraps with
//     [ErrWebhookSecretResolution].
//   - Returning `(nil, nil)` or `(empty-slice, nil)` is a programmer
//     bug caught as [ErrWebhookEmptySecret].
type WebhookSecretResolver func(ctx context.Context) ([]byte, error)

// SourceForTarget is the seam the webhook consumes to map a
// [TargetSource] enum value onto a [toolregistry.SourceConfig.Name]
// suitable for [SchedulerSyncer.SyncOnce]. The resolver runs per-call
// because the per-deployment source mapping is operator config —
// pinning it as a process-global static would couple webhook delivery
// to operator-config reload semantics. Production wiring satisfies
// the seam via a closure that consults the loaded
// [toolregistry.SourceConfig] slice.
//
// Contract:
//
//   - Return the resolved source name on success.
//   - Return `("", <cause>)` on resolution failure (no source
//     configured for the requested target). The webhook receiver
//     wraps with [ErrSourceMappingFailed].
//   - Returning `("", nil)` is a programmer bug also caught as
//     [ErrSourceMappingFailed].
type SourceForTarget func(ctx context.Context, target TargetSource) (string, error)

// WebhookDeps bundles the required dependencies for [NewWebhook].
// Every field except `Logger` is required — passing a nil panics in
// [NewWebhook] with a named-field message. Mirrors [ProposerDeps].
type WebhookDeps struct {
	// SecretResolver resolves the HMAC-signing secret bytes per-call.
	// Required.
	SecretResolver WebhookSecretResolver

	// Lookup resolves the stored [Proposal] by id. Required.
	Lookup ProposalLookup

	// Decisions is the [DecisionRecorder] seam that claims the
	// `tool_approved` decision exactly once per proposal id. Required;
	// guards against duplicate webhook delivery (GitHub retries on
	// 5xx, network glitches replay).
	Decisions DecisionRecorder

	// Syncer triggers a [toolregistry.Scheduler.SyncOnce] for the
	// approved source. Required.
	Syncer SchedulerSyncer

	// SourceResolver maps a [TargetSource] to a
	// [toolregistry.SourceConfig.Name]. Required.
	SourceResolver SourceForTarget

	// Publisher emits [TopicToolApproved] / [TopicToolRejected]
	// events. Required.
	Publisher Publisher

	// Clock stamps [ToolApproved.ApprovedAt] / [ToolRejected.RejectedAt].
	// Required.
	Clock Clock

	// IDGenerator mints the per-decision correlation id. Required.
	IDGenerator IDGenerator

	// Logger receives diagnostic log entries from the webhook.
	// Optional; a nil [Logger] silently discards entries.
	Logger Logger
}

// WebhookOption configures a [Webhook] at construction time.
type WebhookOption func(*webhookConfig)

// webhookConfig is the internal mutable bag the [WebhookOption]
// callbacks populate. Held in a separate type so the [*Webhook] is
// immutable after [NewWebhook] returns.
type webhookConfig struct {
	tsWindow     time.Duration
	maxBodyBytes int64
}

// WithWebhookTimestampWindow overrides the replay-attack guard window
// the verifier consults. Defaults to [defaultWebhookTimestampWindow]
// (5 min). Non-positive values are ignored.
func WithWebhookTimestampWindow(d time.Duration) WebhookOption {
	return func(c *webhookConfig) {
		if d > 0 {
			c.tsWindow = d
		}
	}
}

// WithWebhookMaxBodyBytes overrides the inbound body-size cap.
// Defaults to [defaultWebhookMaxBodyBytes] (1 MiB). Bodies larger
// than the cap return HTTP 413 without touching the dispatchers.
// Non-positive values are ignored.
func WithWebhookMaxBodyBytes(n int64) WebhookOption {
	return func(c *webhookConfig) {
		if n > 0 {
			c.maxBodyBytes = n
		}
	}
}

// Webhook is the M9.4.b git-pr approval webhook receiver. Construct
// via [NewWebhook]; the zero value is not usable. The handler is safe
// for concurrent use across goroutines.
type Webhook struct {
	deps WebhookDeps
	cfg  webhookConfig
}

// NewWebhook constructs a [*Webhook]. Panics with a named-field
// message when any required dependency in `deps` is nil; the panic
// discipline mirrors [New] (M9.4.a authoring proposer).
func NewWebhook(deps WebhookDeps, opts ...WebhookOption) *Webhook {
	if deps.SecretResolver == nil {
		panic("approval: NewWebhook: deps.SecretResolver must not be nil")
	}
	if deps.Lookup == nil {
		panic("approval: NewWebhook: deps.Lookup must not be nil")
	}
	if deps.Decisions == nil {
		panic("approval: NewWebhook: deps.Decisions must not be nil")
	}
	if deps.Syncer == nil {
		panic("approval: NewWebhook: deps.Syncer must not be nil")
	}
	if deps.SourceResolver == nil {
		panic("approval: NewWebhook: deps.SourceResolver must not be nil")
	}
	if deps.Publisher == nil {
		panic("approval: NewWebhook: deps.Publisher must not be nil")
	}
	if deps.Clock == nil {
		panic("approval: NewWebhook: deps.Clock must not be nil")
	}
	if deps.IDGenerator == nil {
		panic("approval: NewWebhook: deps.IDGenerator must not be nil")
	}
	cfg := webhookConfig{
		tsWindow:     defaultWebhookTimestampWindow,
		maxBodyBytes: defaultWebhookMaxBodyBytes,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Webhook{deps: deps, cfg: cfg}
}

// webhookPayload is the JSON shape the receiver expects. A
// platform-side GitHub Action (or any signer following the same
// contract) posts this body after a PR merge. The shape is
// deliberately minimal — the platform identifier `pr_url` /
// `merged_sha` are optional public-identifier strings (no PII), so
// downstream subscribers can correlate without a GitHub API round
// trip.
type webhookPayload struct {
	ProposalID string `json:"proposal_id"`
	Action     string `json:"action"`
	PRURL      string `json:"pr_url"`
	MergedSHA  string `json:"merged_sha"`
	Approver   string `json:"approver"`
	Timestamp  int64  `json:"timestamp"`
}

// ServeHTTP implements [http.Handler]. The resolution order is
// documented at the top of this file. Per-stage logic is delegated to
// `authenticate` (HMAC + replay window), `decodePayload` (strict JSON
// + uuid parse + nil-uuid guard), and `fetchProposal` (store lookup +
// ctx-cancel branch) so the top-level handler stays under the
// project's cyclomatic-complexity ceiling.
func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, ok := h.readBody(ctx, w, r)
	if !ok {
		return
	}
	if !h.authenticate(ctx, w, r, body) {
		return
	}
	payload, proposalID, ok := h.decodePayload(ctx, w, body)
	if !ok {
		return
	}
	proposal, ok := h.fetchProposal(ctx, w, proposalID)
	if !ok {
		return
	}
	switch payload.Action {
	case webhookActionApproved:
		h.handleApproved(ctx, w, payload, proposal)
	case webhookActionRejected:
		h.handleRejected(ctx, w, payload, proposal)
	default:
		// validateWebhookPayload already constrained `Action` to the
		// closed set; this branch is unreachable on a validated
		// payload but is retained as a defence in depth in case the
		// validator is loosened without updating the dispatch.
		w.WriteHeader(http.StatusBadRequest)
	}
}

// authenticate verifies the signing headers + HMAC + replay window.
// Returns true on success; on failure the response has already been
// written.
func (h *Webhook) authenticate(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte) bool {
	sig := r.Header.Get(headerWebhookSignature)
	tsHeader := r.Header.Get(headerWebhookTimestamp)
	if sig == "" || tsHeader == "" {
		h.logErr(ctx, "webhook missing header")
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	secret, err := h.resolveSecret(ctx, w)
	if err != nil {
		return false // resolveSecret has already written the response
	}
	if err := verifyWebhookSignature(secret, sig, tsHeader, body, h.cfg.tsWindow, h.deps.Clock.Now); err != nil {
		switch {
		case errors.Is(err, ErrWebhookStaleTimestamp):
			h.logErr(ctx, "webhook stale timestamp")
		case errors.Is(err, ErrWebhookBadSignature):
			h.logErr(ctx, "webhook bad signature")
		default:
			h.logErr(ctx, "webhook signature verification failed")
		}
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

// decodePayload runs strict JSON decode + payload validation + uuid
// parse + nil-uuid guard. Returns (payload, proposalID, true) on
// success; on failure the response has already been written.
func (h *Webhook) decodePayload(ctx context.Context, w http.ResponseWriter, body []byte) (webhookPayload, uuid.UUID, bool) {
	// Strict JSON decode: an unknown field surfaces as 400 instead of
	// silently flowing through. Symmetric with [DecodeManifest]'s
	// `KnownFields(true)` discipline.
	var payload webhookPayload
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		h.logErr(ctx, "webhook malformed payload")
		w.WriteHeader(http.StatusBadRequest)
		return webhookPayload{}, uuid.Nil, false
	}
	if err := validateWebhookPayload(payload); err != nil {
		h.logErr(ctx, "webhook payload validation failed")
		w.WriteHeader(http.StatusBadRequest)
		return webhookPayload{}, uuid.Nil, false
	}
	proposalID, err := uuid.Parse(payload.ProposalID)
	if err != nil {
		h.logErr(ctx, "webhook proposal_id not a UUID")
		w.WriteHeader(http.StatusBadRequest)
		return webhookPayload{}, uuid.Nil, false
	}
	if proposalID == uuid.Nil {
		// The nil UUID is a valid UUID literal but is reserved as a
		// "no proposal" marker. Symmetric with
		// [DecodeApprovalActionID]'s nil-uuid rejection on the
		// slack-native callback path; rejecting at the webhook
		// boundary surfaces a malformed-payload 400 rather than
		// flowing through to the lookup as a 404.
		h.logErr(ctx, "webhook proposal_id is nil uuid", "proposal_id", payload.ProposalID)
		w.WriteHeader(http.StatusBadRequest)
		return webhookPayload{}, uuid.Nil, false
	}
	return payload, proposalID, true
}

// fetchProposal resolves the stored [Proposal] by id and maps the
// store-side error sentinels onto the per-failure HTTP status. Returns
// (proposal, true) on success; on failure the response has already
// been written.
func (h *Webhook) fetchProposal(ctx context.Context, w http.ResponseWriter, proposalID uuid.UUID) (Proposal, bool) {
	proposal, err := h.deps.Lookup.Lookup(ctx, proposalID)
	if err != nil {
		switch {
		case errors.Is(err, ErrProposalNotFound):
			h.logErr(ctx, "webhook proposal not found", "proposal_id", proposalID)
			w.WriteHeader(http.StatusNotFound)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			h.logErr(ctx, "webhook lookup ctx-cancelled", "proposal_id", proposalID)
			w.WriteHeader(http.StatusRequestTimeout)
		default:
			h.logErr(ctx, "webhook lookup failed", "proposal_id", proposalID)
			w.WriteHeader(http.StatusInternalServerError)
		}
		return Proposal{}, false
	}
	return proposal, true
}

// handleApproved runs the post-validation sequence for an `approved`
// payload. Resolution order:
//
//  1. ctx.Err pre-claim gate — refuse to claim the decision when the
//     caller has already cancelled (the http client gave up).
//  2. MarkDecided(approved) — atomic idempotency claim. A duplicate
//     delivery (GitHub retry, replay) observes the claim and the
//     handler silent-200s without re-publishing. The full claim →
//     publish window runs under [context.WithoutCancel] so a
//     mid-flight caller cancel does NOT split the claim from the
//     side-effects.
//  3. Source mapping resolves AFTER the claim so an unresolvable
//     target (e.g. operator removed the source between proposal time
//     and approval time) rolls the claim back via UnmarkDecided.
//  4. SyncOnce runs on the detached ctx; failure DOES NOT abort the
//     publish — the event is the durable record. The audit subscriber
//     records the sync failure for operator triage.
//  5. Publisher.Publish runs on the detached ctx. On failure the
//     decision is unmark-rolled back so a retry can re-claim and
//     re-publish.
//
// HTTP 202 is returned on a successful publish when the sync failed
// (the operator MUST consult the audit row for sync outcome); 200 OK
// otherwise.
func (h *Webhook) handleApproved(ctx context.Context, w http.ResponseWriter, payload webhookPayload, proposal Proposal) {
	if err := ctx.Err(); err != nil {
		// Pre-claim ctx-check: refuse to claim a decision the caller
		// has already abandoned. Mirrors [Proposer.Submit]'s
		// "ctx.Err before any side effects" discipline.
		h.logErr(ctx, "webhook ctx-cancelled pre-claim", "proposal_id", proposal.ID)
		w.WriteHeader(http.StatusRequestTimeout)
		return
	}

	// The full claim→publish window runs under a cancel-detached
	// child ctx so a caller-side cancel (ingress timeout, http
	// client disconnect) does not split the claim from the
	// side-effects. Same pattern as M9.4.a iter-1 M1 lesson.
	detachedCtx := context.WithoutCancel(ctx)

	firstTime, err := h.deps.Decisions.MarkDecided(detachedCtx, proposal.ID, DecisionApproved)
	if err != nil {
		if errors.Is(err, ErrDecisionConflict) {
			h.logErr(ctx, "webhook decision conflict", "proposal_id", proposal.ID)
			w.WriteHeader(http.StatusConflict)
			return
		}
		h.logErr(ctx, "webhook mark decided failed", "proposal_id", proposal.ID)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !firstTime {
		// Idempotent replay: the decision was already claimed by an
		// earlier delivery of the same kind. Silent 200 — the
		// downstream event is already in flight (or landed); a
		// re-publish would produce duplicate `tool_approved` events
		// and downstream registry reloads.
		h.logErr(ctx, "webhook duplicate approved replay", "proposal_id", proposal.ID)
		w.WriteHeader(http.StatusOK)
		return
	}

	sourceName, err := h.deps.SourceResolver(detachedCtx, proposal.Input.TargetSource)
	if err != nil || sourceName == "" {
		_ = h.deps.Decisions.UnmarkDecided(detachedCtx, proposal.ID, DecisionApproved)
		h.logErr(ctx, "webhook source mapping failed", "proposal_id", proposal.ID)
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	syncErr := h.deps.Syncer.SyncOnce(detachedCtx, sourceName)
	if syncErr != nil {
		h.logErr(ctx, "webhook tool source sync failed", "proposal_id", proposal.ID)
		// Continue to publish — the event is the durable record.
	}

	corrID, err := h.newCorrelationID(proposal)
	if err != nil {
		_ = h.deps.Decisions.UnmarkDecided(detachedCtx, proposal.ID, DecisionApproved)
		h.logErr(ctx, "webhook id generator failed", "proposal_id", proposal.ID)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	event := ToolApproved{
		ProposalID:    proposal.ID,
		ToolName:      proposal.Input.Name,
		ApproverID:    payload.Approver,
		Route:         RouteGitPR,
		TargetSource:  proposal.Input.TargetSource,
		SourceName:    sourceName,
		PRURL:         payload.PRURL,
		MergedSHA:     payload.MergedSHA,
		ApprovedAt:    h.deps.Clock.Now(),
		CorrelationID: corrID,
	}

	if err := h.deps.Publisher.Publish(detachedCtx, TopicToolApproved, event); err != nil {
		// Roll the claim back so a retried delivery can re-publish
		// rather than observing the orphan claim and silent-200ing.
		_ = h.deps.Decisions.UnmarkDecided(detachedCtx, proposal.ID, DecisionApproved)
		h.logErr(ctx, "webhook publish tool_approved failed", "proposal_id", proposal.ID)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if syncErr != nil {
		// HTTP 202 Accepted communicates "approval landed but
		// downstream sync needs operator attention". The operator
		// resolves via the M9.7 audit row.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleRejected runs the post-validation sequence for a `rejected`
// payload: ctx.Err pre-claim → MarkDecided(rejected) → Publish.
// Resolution order mirrors [handleApproved] absent the source mapping
// + SyncOnce (a rejection does not touch the registry).
func (h *Webhook) handleRejected(ctx context.Context, w http.ResponseWriter, payload webhookPayload, proposal Proposal) {
	if err := ctx.Err(); err != nil {
		h.logErr(ctx, "webhook ctx-cancelled pre-claim", "proposal_id", proposal.ID)
		w.WriteHeader(http.StatusRequestTimeout)
		return
	}

	detachedCtx := context.WithoutCancel(ctx)

	firstTime, err := h.deps.Decisions.MarkDecided(detachedCtx, proposal.ID, DecisionRejected)
	if err != nil {
		if errors.Is(err, ErrDecisionConflict) {
			h.logErr(ctx, "webhook decision conflict", "proposal_id", proposal.ID)
			w.WriteHeader(http.StatusConflict)
			return
		}
		h.logErr(ctx, "webhook mark decided failed", "proposal_id", proposal.ID)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !firstTime {
		h.logErr(ctx, "webhook duplicate rejected replay", "proposal_id", proposal.ID)
		w.WriteHeader(http.StatusOK)
		return
	}

	corrID, err := h.newCorrelationID(proposal)
	if err != nil {
		_ = h.deps.Decisions.UnmarkDecided(detachedCtx, proposal.ID, DecisionRejected)
		h.logErr(ctx, "webhook id generator failed", "proposal_id", proposal.ID)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	event := ToolRejected{
		ProposalID:    proposal.ID,
		ToolName:      proposal.Input.Name,
		RejecterID:    payload.Approver,
		Route:         RouteGitPR,
		RejectedAt:    h.deps.Clock.Now(),
		CorrelationID: corrID,
	}

	if err := h.deps.Publisher.Publish(detachedCtx, TopicToolRejected, event); err != nil {
		_ = h.deps.Decisions.UnmarkDecided(detachedCtx, proposal.ID, DecisionRejected)
		h.logErr(ctx, "webhook publish tool_rejected failed", "proposal_id", proposal.ID)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// readBody caps + reads the request body. Returns (body, true) on
// success; on failure the response has already been written and the
// caller MUST return.
func (h *Webhook) readBody(ctx context.Context, w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	limited := http.MaxBytesReader(w, r.Body, h.cfg.maxBodyBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.logErr(ctx, "webhook oversize body")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return nil, false
		}
		h.logErr(ctx, "webhook read body failed")
		w.WriteHeader(http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

// resolveSecret runs the per-call [WebhookSecretResolver]. On failure
// the response has already been written and the caller MUST return.
func (h *Webhook) resolveSecret(ctx context.Context, w http.ResponseWriter) ([]byte, error) {
	secret, err := h.deps.SecretResolver(ctx)
	if err != nil {
		h.logErr(ctx, "webhook secret resolution failed")
		w.WriteHeader(http.StatusInternalServerError)
		return nil, fmt.Errorf("%w: %w", ErrWebhookSecretResolution, err)
	}
	if len(secret) == 0 {
		h.logErr(ctx, "webhook secret resolver returned empty value")
		w.WriteHeader(http.StatusInternalServerError)
		return nil, ErrWebhookEmptySecret
	}
	return secret, nil
}

// newCorrelationID returns a correlation id for the decision event.
// Prefers [Proposal.CorrelationID] (the UUIDv7 from authoring time)
// so the proposal-id → decision-id chain is trivial to join; falls
// back to a fresh UUIDv7 from the configured generator when the
// proposal's correlation id is empty (lookup returned a stale or
// pre-M9.4.a record).
func (h *Webhook) newCorrelationID(p Proposal) (string, error) {
	if p.CorrelationID != "" {
		return p.CorrelationID, nil
	}
	id, err := h.deps.IDGenerator.NewUUID()
	if err != nil {
		return "", fmt.Errorf("approval: webhook id generator: %w", err)
	}
	return id.String(), nil
}

// logErr emits a single-line diagnostic via the optional logger. The
// helper centralises the nil-check so every call site stays a
// one-liner.
//
// PII discipline: callers MUST pass opaque identifier kv pairs only
// (proposal_id, request_id) — never `Proposal.Input.Purpose`,
// `PlainLanguageDescription`, `CodeDraft`, or the raw request body.
// The PII-canary tests in `webhook_test.go` pin this contract: any
// caller that leaks a body-field substring into kv fails the canary.
func (h *Webhook) logErr(ctx context.Context, msg string, kv ...any) {
	if h.deps.Logger != nil {
		h.deps.Logger.Log(ctx, "approval: "+msg, kv...)
	}
}

// validateWebhookPayload enforces the shape contract on a decoded
// payload. Required fields: proposal_id (non-empty string),
// action ∈ {approved, rejected}, timestamp > 0. The PR url / merged
// sha / approver fields are optional — empty values flow through to
// the published event verbatim.
func validateWebhookPayload(p webhookPayload) error {
	if strings.TrimSpace(p.ProposalID) == "" {
		return fmt.Errorf("%w: proposal_id required", ErrWebhookMalformedPayload)
	}
	switch p.Action {
	case webhookActionApproved, webhookActionRejected:
		// ok
	default:
		return fmt.Errorf("%w: action %q outside closed set", ErrWebhookMalformedPayload, p.Action)
	}
	if p.Timestamp <= 0 {
		return fmt.Errorf("%w: timestamp must be positive unix seconds", ErrWebhookMalformedPayload)
	}
	return nil
}

// verifyWebhookSignature implements the watchkeeper webhook v0
// signature algorithm:
//
//  1. timestamp := strconv.ParseInt(tsHeader, 10, 64)
//  2. if |now() - timestamp| > window → ErrWebhookStaleTimestamp
//  3. base := tsHeader + ":" + raw_body
//  4. expected := "sha256=" + hex(hmac_sha256(secret, base))
//  5. hmac.Equal([]byte(expected), []byte(sigHeader)) ? nil : ErrWebhookBadSignature
//
// Domain separation: the algorithm-version label is carried on the
// HEADER NAME (`X-Watchkeeper-Signature-256`) rather than as a
// literal prefix on the signing base — this aligns with the GitHub
// `X-Hub-Signature-256` convention, so an operator who configured a
// GitHub webhook signer can re-use the platform-standard sender
// without injecting a watchkeeper-specific version token into the
// signed bytes. A future migration to a stronger digest (SHA-384 /
// SHA-512) is additive: a new `X-Watchkeeper-Signature-384` header
// rides alongside, and this verifier can branch on header presence.
// This DIFFERS from `slack/inbound`'s `v0:<ts>:<body>` base — Slack's
// own algorithm embeds the version token in the signed string; the
// watchkeeper webhook deliberately mirrors GitHub instead.
//
// The compare uses [hmac.Equal] rather than `bytes.Equal` to avoid
// leaking timing information about the secret-bound HMAC value
// (constant-time compare is a documented security obligation in every
// signed-payload integration and a Phase-1 LESSON inherited from
// M6.3.a).
//
// `now` is the [Clock.Now] callback; tests inject deterministic
// clocks to drive the stale-timestamp negative path.
func verifyWebhookSignature(
	secret []byte,
	sigHeader, tsHeader string,
	body []byte,
	window time.Duration,
	now func() time.Time,
) error {
	if sigHeader == "" || tsHeader == "" {
		return ErrWebhookMissingHeader
	}

	tsUnix, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		// Non-integer timestamps are bucketed under "stale" rather
		// than "bad signature": the timestamp gate fails BEFORE the
		// HMAC compare runs, and stale captures every "I cannot trust
		// this timestamp" branch (same discipline as slack/inbound).
		return ErrWebhookStaleTimestamp
	}
	if window <= 0 {
		window = defaultWebhookTimestampWindow
	}
	delta := now().Sub(time.Unix(tsUnix, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > window {
		return ErrWebhookStaleTimestamp
	}

	if !strings.HasPrefix(sigHeader, webhookSignaturePrefix) {
		return ErrWebhookBadSignature
	}

	expected := computeWebhookSignature(secret, tsHeader, body)
	if !hmac.Equal([]byte(expected), []byte(sigHeader)) {
		return ErrWebhookBadSignature
	}
	return nil
}

// computeWebhookSignature renders the canonical
// `sha256=<hex>` signature for the supplied (secret, timestamp, body)
// tuple. Hoisted out of [verifyWebhookSignature] so the test fixtures
// can reuse it via [SignWebhook] without leaking internal state.
//
// The sequence is timestamp + `:` + raw_body, hashed under
// hmac_sha256(secret), hex-encoded, prefixed with `sha256=`.
func computeWebhookSignature(secret []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts))
	mac.Write([]byte(webhookSignatureBaseSeparator))
	mac.Write(body)
	return webhookSignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// SignWebhook returns the `X-Watchkeeper-Signature-256` header value
// for the supplied (secret, timestamp, body) tuple per the v0
// algorithm. Exported so test harnesses and integration fixtures can
// construct correctly-signed requests using the SAME HMAC code path
// the verifier consults — same AC6 discipline from M6.3.a
// ([slack/inbound.Sign]).
//
// Production code MUST NOT call SignWebhook: the production signer is
// the platform-side GitHub Action / webhook poster. The function
// exists for tests and bootstrap fixtures only.
func SignWebhook(secret []byte, ts string, body []byte) string {
	return computeWebhookSignature(secret, ts, body)
}
