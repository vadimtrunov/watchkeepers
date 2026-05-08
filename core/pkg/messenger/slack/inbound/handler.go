package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
)

// Slack signs every inbound webhook with a (signature, timestamp)
// pair carried on these two headers. Names are pinned constants so a
// future header re-key from Slack (unlikely in v0) is a one-line
// change here that the handler + tests pick up via the compiler.
const (
	headerSlackSignature = "X-Slack-Signature"
	headerSlackTimestamp = "X-Slack-Request-Timestamp"
)

// Route paths exposed by the handler. Hoisted so the [http.ServeMux]
// registration site and any future operator dashboard share a single
// source of truth.
const (
	routeEvents       = "POST /v1/slack/events"
	routeInteractions = "POST /v1/slack/interactions"
)

// defaultMaxBodyBytes caps inbound request bodies at 1 MB. Slack's
// own platform limits are below this (Events API ~3KB; Interactivity
// payloads ~30KB in practice); 1 MB leaves comfortable headroom for
// future block-kit expansions while bounding memory pressure on the
// handler. Configurable via [WithMaxBodyBytes].
const defaultMaxBodyBytes int64 = 1 << 20 // 1 MB

// auditEventReceived / auditEventRejected are the keepers_log
// event_type values the handler emits. Pinned per AC8.
const (
	auditEventReceived = "slack_webhook_received"
	auditEventRejected = "slack_webhook_rejected"
)

// rejection reasons emitted on the audit row's `reason` field. Closed
// vocabulary; the handler maps each negative branch onto exactly one
// of these values so a downstream Recall query can group failures
// without parsing free-form strings.
const (
	reasonMissingHeader  = "missing_header"
	reasonStaleTimestamp = "stale_timestamp"
	reasonBadSignature   = "bad_signature"
	reasonMalformedJSON  = "malformed_json"
	reasonOversizeBody   = "oversize_body"
	reasonReadBody       = "read_body"
)

// envelopeKindURLVerification / envelopeKindEventCallback are the
// `type` values the Events API envelope carries. Pinned to avoid magic
// strings spread across the dispatch switch.
const (
	envelopeKindURLVerification = "url_verification"
	envelopeKindEventCallback   = "event_callback"
)

// payload key constants for the audit row's `data` field. Snake_case
// per the project convention; centralised so the assertion-side fakes
// and the production payload builders share names.
const (
	payloadKeyRoute     = "route"
	payloadKeyEventType = "event_type"
	payloadKeyRequestID = "request_id"
	payloadKeyReason    = "reason"
)

// AuditAppender is the minimal subset of [keeperslog.Writer] the
// handler consumes — only Append. Defined locally so unit tests can
// substitute a tiny fake without standing up the full keepclient
// stack. Mirrors the spawn / cron / lifecycle subset-interface
// pattern documented in `docs/LESSONS.md`.
type AuditAppender interface {
	Append(ctx context.Context, evt keeperslog.Event) (string, error)
}

// Compile-time assertion: the production [*keeperslog.Writer]
// satisfies [AuditAppender]. Pins the integration shape against
// future drift in keeperslog.
var _ AuditAppender = (*keeperslog.Writer)(nil)

// ErrInvalidConfig is returned by [NewHandler] when a required option
// (currently: signing secret) is missing or empty. The handler fails
// closed at construction time rather than serving requests it cannot
// verify.
var ErrInvalidConfig = errors.New("inbound: invalid handler config")

// Option configures a handler at construction time. Pass options to
// [NewHandler]; later options override earlier ones for the same field.
type Option func(*handlerConfig)

// handlerConfig is the internal mutable bag the [Option] callbacks
// populate. Held in a separate type so the [http.Handler] returned
// from [NewHandler] is immutable after construction.
type handlerConfig struct {
	signingSecret []byte
	tsWindow      time.Duration
	maxBodyBytes  int64
	eventDisp     EventDispatcher
	intDisp       InteractionDispatcher
	audit         AuditAppender
	clock         func() time.Time
	requestIDGen  func() (string, error)
}

// WithSigningSecret wires the Slack app's signing-secret bytes the
// verifier consults. REQUIRED; [NewHandler] returns [ErrInvalidConfig]
// when this option is missing or supplies an empty slice. The bytes
// are NOT copied — callers must treat the slice as read-only after
// passing it in.
func WithSigningSecret(secret []byte) Option {
	return func(c *handlerConfig) {
		if len(secret) > 0 {
			c.signingSecret = secret
		}
	}
}

// WithTimestampWindow overrides the replay-attack guard window the
// verifier consults. Defaults to [defaultTimestampWindow] (5 min) per
// Slack's published guidance. Non-positive values are ignored.
func WithTimestampWindow(d time.Duration) Option {
	return func(c *handlerConfig) {
		if d > 0 {
			c.tsWindow = d
		}
	}
}

// WithMaxBodyBytes overrides the inbound body-size cap. Bodies larger
// than the cap return HTTP 413 without touching the dispatchers.
// Defaults to [defaultMaxBodyBytes] (1 MB). Non-positive values are
// ignored.
func WithMaxBodyBytes(n int64) Option {
	return func(c *handlerConfig) {
		if n > 0 {
			c.maxBodyBytes = n
		}
	}
}

// WithEventDispatcher wires the [EventDispatcher] consulted on every
// successfully-decoded `event_callback`. A nil dispatcher is ignored;
// handlers without an explicit dispatcher fall back to a no-op
// (M6.3.a scaffolding default).
func WithEventDispatcher(d EventDispatcher) Option {
	return func(c *handlerConfig) {
		if d != nil {
			c.eventDisp = d
		}
	}
}

// WithInteractionDispatcher wires the [InteractionDispatcher]
// consulted on every successfully-decoded Interactivity payload. A
// nil dispatcher is ignored; handlers without an explicit dispatcher
// fall back to a no-op.
func WithInteractionDispatcher(d InteractionDispatcher) Option {
	return func(c *handlerConfig) {
		if d != nil {
			c.intDisp = d
		}
	}
}

// WithAuditAppender wires the [keeperslog.Writer]-shaped audit sink
// consulted on every request. A nil appender is ignored; handlers
// without an explicit appender skip audit emission entirely (M6.3.a
// AC8 mandates emission, so production callers MUST wire it; the nil
// fallback exists so the unit tests for the verifier can drive the
// handler without an audit fake).
func WithAuditAppender(a AuditAppender) Option {
	return func(c *handlerConfig) {
		if a != nil {
			c.audit = a
		}
	}
}

// WithClock overrides the wall-clock function used by the timestamp
// verifier. Defaults to [time.Now]. Tests inject deterministic clocks
// to drive the stale-timestamp negative path.
func WithClock(now func() time.Time) Option {
	return func(c *handlerConfig) {
		if now != nil {
			c.clock = now
		}
	}
}

// WithRequestIDGenerator overrides the per-request id generator the
// audit payload's `request_id` field consumes. Defaults to UUID v7
// (matches keeperslog's default correlation generator). A nil
// generator is ignored.
func WithRequestIDGenerator(g func() (string, error)) Option {
	return func(c *handlerConfig) {
		if g != nil {
			c.requestIDGen = g
		}
	}
}

// NewHandler returns an [http.Handler] mounting the Events API and
// Interactivity routes per the package documentation. Returns
// [ErrInvalidConfig] when the signing secret is missing.
func NewHandler(opts ...Option) (http.Handler, error) {
	cfg := handlerConfig{
		tsWindow:     defaultTimestampWindow,
		maxBodyBytes: defaultMaxBodyBytes,
		eventDisp:    noopEventDispatcher{},
		intDisp:      noopInteractionDispatcher{},
		clock:        time.Now,
		requestIDGen: defaultRequestID,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if len(cfg.signingSecret) == 0 {
		return nil, fmt.Errorf("%w: signing secret required", ErrInvalidConfig)
	}

	h := &handler{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc(routeEvents, h.serveEvents)
	mux.HandleFunc(routeInteractions, h.serveInteractions)
	return mux, nil
}

// handler is the package-internal [http.Handler] backing
// [NewHandler]. Held in a struct so the per-route HandlerFuncs can
// share configuration without re-binding state in closures.
type handler struct {
	cfg handlerConfig
}

// serveEvents handles `POST /v1/slack/events`. Resolution order:
//
//  1. Read + cap the raw body (HTTP 413 on overflow).
//  2. Verify the Slack signature (HTTP 401 on miss/stale/bad).
//  3. Decode the envelope (HTTP 400 on malformed JSON).
//  4. Branch on `type`: url_verification → echo challenge as
//     text/plain; event_callback → dispatch + ACK 200.
//  5. Emit the `slack_webhook_received` audit row on success or
//     `slack_webhook_rejected` on failure.
//
//nolint:contextcheck // intentional: ctx is derived from r.Context() via keeperslog.ContextWithCorrelationID; the helper is a context.WithValue wrapper but contextcheck does not introspect through it. Same project pattern as keep-server middleware.
func (h *handler) serveEvents(w http.ResponseWriter, r *http.Request) {
	requestID := h.mintRequestID()
	ctx := h.requestContext(r, requestID)
	body, ok := h.readVerified(ctx, w, r, requestID, routeEvents, "")
	if !ok {
		return
	}

	var envelope eventsEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		h.reject(ctx, w, http.StatusBadRequest, requestID, routeEvents, envelope.Type, reasonMalformedJSON)
		return
	}

	switch envelope.Type {
	case envelopeKindURLVerification:
		h.appendReceived(ctx, requestID, routeEvents, envelopeKindURLVerification)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, envelope.Challenge)
	case envelopeKindEventCallback:
		ev := Event{
			TeamID:    envelope.TeamID,
			APIAppID:  envelope.APIAppID,
			EventID:   envelope.EventID,
			EventTime: envelope.EventTime,
			Type:      envelope.Event.Type,
			Inner:     envelope.Event.Raw,
		}
		_ = h.cfg.eventDisp.DispatchEvent(ctx, ev)
		h.appendReceived(ctx, requestID, routeEvents, envelopeKindEventCallback)
		w.WriteHeader(http.StatusOK)
	default:
		// Unknown but signature-valid envelope: ACK 200 (Slack will
		// otherwise retry indefinitely) but record the audit row so
		// the operator notices the unhandled type.
		h.appendReceived(ctx, requestID, routeEvents, envelope.Type)
		w.WriteHeader(http.StatusOK)
	}
}

// serveInteractions handles `POST /v1/slack/interactions`. Slack
// posts the payload as `payload=<json>` form-encoded, so the handler
// reads the raw body for the signature check, then re-parses the
// form locally (NOT via [http.Request.ParseForm], which would consume
// r.Body again).
//
//nolint:contextcheck // intentional: ctx is derived from r.Context() via keeperslog.ContextWithCorrelationID; the helper is a context.WithValue wrapper but contextcheck does not introspect through it. Same project pattern as keep-server middleware.
func (h *handler) serveInteractions(w http.ResponseWriter, r *http.Request) {
	requestID := h.mintRequestID()
	ctx := h.requestContext(r, requestID)
	body, ok := h.readVerified(ctx, w, r, requestID, routeInteractions, "")
	if !ok {
		return
	}

	form, err := url.ParseQuery(string(body))
	if err != nil {
		h.reject(ctx, w, http.StatusBadRequest, requestID, routeInteractions, "", reasonMalformedJSON)
		return
	}
	payload := form.Get("payload")
	if payload == "" {
		h.reject(ctx, w, http.StatusBadRequest, requestID, routeInteractions, "", reasonMalformedJSON)
		return
	}

	var iEnv interactionEnvelope
	if err := json.Unmarshal([]byte(payload), &iEnv); err != nil {
		h.reject(ctx, w, http.StatusBadRequest, requestID, routeInteractions, "", reasonMalformedJSON)
		return
	}

	teamID := iEnv.TeamID
	if teamID == "" {
		teamID = iEnv.Team.ID
	}
	p := Interaction{
		Type:     iEnv.Type,
		TeamID:   teamID,
		APIAppID: iEnv.APIAppID,
		Raw:      json.RawMessage(payload),
	}
	_ = h.cfg.intDisp.DispatchInteraction(ctx, p)
	h.appendReceived(ctx, requestID, routeInteractions, iEnv.Type)
	w.WriteHeader(http.StatusOK)
}

// mintRequestID returns a fresh per-request id stamped onto every
// audit row so a Recall query can correlate `received` and `rejected`
// rows for a single inbound HTTP exchange. Falls back to the empty
// string on generator error; the keeperslog writer mints a fresh
// correlation id internally when none is on the ctx.
func (h *handler) mintRequestID() string {
	id, err := h.cfg.requestIDGen()
	if err != nil {
		return ""
	}
	return id
}

// requestContext returns the request context with the correlation id
// threaded onto it for keeperslog. Empty `id` is a no-op (matches
// [keeperslog.ContextWithCorrelationID]'s contract). The returned ctx
// is a [context.WithValue] wrapper around r.Context(); contextcheck
// does not introspect through the helper, so the two HTTP handlers
// using the result carry a function-level nolint:contextcheck rationale
// (audit-row correlation id is the documented project pattern; same as
// keep server middleware threading via [keeperslog.ContextWithCorrelationID]).
func (h *handler) requestContext(r *http.Request, id string) context.Context {
	return keeperslog.ContextWithCorrelationID(r.Context(), id)
}

// readVerified runs the body-size cap, signature verification, and
// returns the raw body bytes when both pass. Failures emit the audit
// row + HTTP response and return ok=false.
//
// AC5 ("read raw body ONCE") is honoured here: the body is consumed
// via [http.MaxBytesReader] + [io.ReadAll] exactly once; the caller
// receives the bytes slice and decodes from memory. r.Body is left
// drained — no downstream code re-reads it.
func (h *handler) readVerified(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	requestID string,
	route string,
	eventTypeForAudit string,
) ([]byte, bool) {
	limited := http.MaxBytesReader(w, r.Body, h.cfg.maxBodyBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.reject(ctx, w, http.StatusRequestEntityTooLarge, requestID, route, eventTypeForAudit, reasonOversizeBody)
			return nil, false
		}
		h.reject(ctx, w, http.StatusBadRequest, requestID, route, eventTypeForAudit, reasonReadBody)
		return nil, false
	}

	sig := r.Header.Get(headerSlackSignature)
	ts := r.Header.Get(headerSlackTimestamp)
	if err := verifySignature(h.cfg.signingSecret, sig, ts, body, h.cfg.tsWindow, h.cfg.clock); err != nil {
		reason := reasonBadSignature
		switch {
		case errors.Is(err, ErrMissingHeader):
			reason = reasonMissingHeader
		case errors.Is(err, ErrStaleTimestamp):
			reason = reasonStaleTimestamp
		}
		h.reject(ctx, w, http.StatusUnauthorized, requestID, route, eventTypeForAudit, reason)
		return nil, false
	}
	return body, true
}

// reject writes the negative-path response + audit row for a single
// failure. Centralised so every negative branch in serveEvents /
// serveInteractions emits the SAME shape (AC8: each negative branch
// emits exactly one slack_webhook_rejected with reason field).
func (h *handler) reject(
	ctx context.Context,
	w http.ResponseWriter,
	status int,
	requestID, route, eventType, reason string,
) {
	if h.cfg.audit != nil {
		_, _ = h.cfg.audit.Append(ctx, keeperslog.Event{
			EventType: auditEventRejected,
			Payload: map[string]any{
				payloadKeyRoute:     route,
				payloadKeyEventType: eventType,
				payloadKeyRequestID: requestID,
				payloadKeyReason:    reason,
			},
		})
	}
	w.WriteHeader(status)
}

// appendReceived emits the happy-path `slack_webhook_received` audit
// row. The payload carries route + event_type + request_id; the body
// is NEVER attached (PII redaction discipline — AC8).
func (h *handler) appendReceived(ctx context.Context, requestID, route, eventType string) {
	if h.cfg.audit == nil {
		return
	}
	_, _ = h.cfg.audit.Append(ctx, keeperslog.Event{
		EventType: auditEventReceived,
		Payload: map[string]any{
			payloadKeyRoute:     route,
			payloadKeyEventType: eventType,
			payloadKeyRequestID: requestID,
		},
	})
}

// eventsEnvelope is the subset of the Events API envelope the handler
// decodes. The inner `event` carries its own RawMessage so M6.3.b/c
// can decode the inner shape lazily without paying the generic-decode
// tax twice.
type eventsEnvelope struct {
	Type      string            `json:"type"`
	Challenge string            `json:"challenge"`
	TeamID    string            `json:"team_id"`
	APIAppID  string            `json:"api_app_id"`
	EventID   string            `json:"event_id"`
	EventTime int64             `json:"event_time"`
	Event     eventInnerWrapper `json:"event"`
}

// eventInnerWrapper carries the inner event JSON alongside its `type`
// hoisted for the dispatcher's convenience. The custom UnmarshalJSON
// keeps the raw bytes in lock-step with the decoded type field.
type eventInnerWrapper struct {
	Type string
	Raw  json.RawMessage
}

// UnmarshalJSON captures the raw inner JSON before extracting the
// `type` field — gives the dispatcher both the decoded discriminator
// AND the untouched bytes for downstream re-decode.
func (e *eventInnerWrapper) UnmarshalJSON(data []byte) error {
	e.Raw = append(e.Raw[:0], data...)
	var probe struct {
		Type string `json:"type"`
	}
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	e.Type = probe.Type
	return nil
}

// interactionEnvelope is the subset of the Interactivity payload the
// handler decodes for routing. Slack's payload uses two shapes for
// the team identifier (legacy top-level `team_id`, modern nested
// `team.id`); the handler accepts both.
type interactionEnvelope struct {
	Type     string `json:"type"`
	TeamID   string `json:"team_id"`
	APIAppID string `json:"api_app_id"`
	Team     struct {
		ID string `json:"id"`
	} `json:"team"`
}

// defaultRequestID returns a fresh UUID v7 string. Hoisted so
// [NewHandler] wires it once and the per-request path stays cheap.
func defaultRequestID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}
