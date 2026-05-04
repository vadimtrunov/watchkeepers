package keeperslog

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// LocalKeepClient is the minimal subset of the keepclient surface that
// [Writer] consumes — only the [keepclient.Client.LogAppend] method is
// touched. Defined as an interface in this package so tests can
// substitute a hand-rolled fake without standing up an HTTP server, and
// so production code never imports the concrete `*keepclient.Client`
// type at all. `*keepclient.Client` satisfies the interface as-is; the
// compile-time assertion lives in `writer_test.go`, mirroring the
// lifecycle.LocalKeepClient + cron.LocalPublisher one-way
// import-cycle-break pattern documented in `docs/LESSONS.md`.
type LocalKeepClient interface {
	LogAppend(ctx context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error)
}

// Logger is the optional diagnostic sink wired in via [WithLogger]. The
// shape mirrors the secrets / capability / cron / lifecycle Logger
// interfaces: a single `Log(ctx, msg, kv...)` so callers can substitute
// structured loggers (slog, zap wrapper, etc.) without losing type
// compatibility.
//
// IMPORTANT (redaction discipline): implementations MUST NOT log raw
// [Event.Payload] data. The writer never passes the payload through the
// logger; only metadata (event_type, correlation_id, event_id) appears
// in log entries. Callers wrapping their own Logger should preserve
// this invariant.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// IDGenerator returns a freshly-minted opaque identifier. Used for
// `event_id` (always) and for `correlation_id` (when none is present on
// the ctx). The default generator returns UUID v7 strings so ids are
// time-sortable. Tests substitute deterministic generators via
// [WithIDGenerator] / [WithCorrelationIDGenerator].
type IDGenerator func() (string, error)

// Event is the input shape for [Writer.Append]. Event captures the
// caller's contribution; the writer stamps the rest (event_id,
// timestamp, trace fields) on top.
type Event struct {
	// EventType is the row's event_type column. Required. Empty values
	// return [ErrInvalidEvent] synchronously without a network round-trip.
	// The keepclient surface treats event_type as a free-form string;
	// the project convention is `<noun>_<verb>` past tense
	// (e.g. `watchkeeper_spawned`, `manifest_proposed`).
	EventType string

	// CausationID is the optional id of the parent event that caused
	// this one. Distinct from CorrelationID (which groups a chain) —
	// causation captures the immediate parent in the chain. Phase 1
	// callers may leave this empty; later milestones (saga
	// orchestration, multi-step approvals) populate it.
	CausationID string

	// Payload is the caller's domain-specific data attached to the
	// event. Encoded under the `data` envelope key. Nil payloads are
	// fine — the `data` key is omitted entirely. Marshalling failure
	// surfaces as a wrapped error from [Writer.Append]; callers that
	// need stricter typing should validate before calling Append.
	//
	// IMPORTANT: payloads must NOT carry infrastructure metadata
	// (`deployment_id`, `environment`, `host`, …) per the M2 design
	// constraint. The writer does not enforce this — code review does.
	Payload any
}

// Option configures a [Writer] at construction time. Pass options to
// [New]; later options override earlier ones for the same field.
type Option func(*config)

// config is the internal mutable bag the [Option] callbacks populate.
// Held in a separate type so [Writer] itself stays immutable after
// [New] returns.
type config struct {
	clock      func() time.Time
	logger     Logger
	eventIDGen IDGenerator
	corrIDGen  IDGenerator
}

// WithClock overrides the wall-clock function used to stamp the
// `timestamp` envelope field. Defaults to [time.Now]. A nil function
// is a no-op so callers can always pass through whatever they have.
func WithClock(c func() time.Time) Option {
	return func(cfg *config) {
		if c != nil {
			cfg.clock = c
		}
	}
}

// WithLogger wires a diagnostic sink onto the returned [*Writer]. When
// set, the writer emits a `keeperslog: appended` entry on every
// successful Append carrying `event_type`, `correlation_id`, and
// `event_id`. Failures emit `keeperslog: append failed` with the same
// metadata plus an `err_type` field (the type, never the value, per
// the redaction discipline established by M3.4.b). A nil logger is a
// no-op so callers can always pass through whatever they have.
//
// IMPORTANT: log entries NEVER carry [Event.Payload] data. Only
// metadata (event_type, correlation_id, event_id) is logged.
func WithLogger(l Logger) Option {
	return func(cfg *config) {
		if l != nil {
			cfg.logger = l
		}
	}
}

// WithIDGenerator overrides the generator used to mint per-event
// `event_id` values. Defaults to UUID v7 via [google/uuid]. Tests
// supply a deterministic generator so the encoded payload can be
// asserted byte-for-byte. A nil generator is a no-op so callers can
// always pass through whatever they have.
func WithIDGenerator(g IDGenerator) Option {
	return func(cfg *config) {
		if g != nil {
			cfg.eventIDGen = g
		}
	}
}

// WithCorrelationIDGenerator overrides the generator used to mint a
// fresh correlation id when none is present on the request ctx.
// Defaults to UUID v7 via [google/uuid]. Tests supply a deterministic
// generator. A nil generator is a no-op so callers can always pass
// through whatever they have.
func WithCorrelationIDGenerator(g IDGenerator) Option {
	return func(cfg *config) {
		if g != nil {
			cfg.corrIDGen = g
		}
	}
}

// Writer is the structured Keeper's Log writer. Construct via [New];
// the zero value is not usable. Methods are safe for concurrent use
// across goroutines: the writer holds only immutable configuration
// after [New] returns; per-call state lives on the goroutine stack.
type Writer struct {
	keepClient LocalKeepClient
	logger     Logger
	clock      func() time.Time
	eventIDGen IDGenerator
	corrIDGen  IDGenerator
}

// New constructs a [Writer] backed by the supplied [LocalKeepClient].
// `client` is required; passing a nil client is a programmer error and
// panics with a clear message — matches the panic discipline of
// [lifecycle.New], [cron.New], and [keepclient.WithBaseURL]. A Writer
// with no client cannot do anything useful, and silently no-oping every
// call would mask the bug.
//
// The defaults are: clock = [time.Now], logger = nil (diagnostics
// disabled), eventIDGen = [uuid.NewV7], corrIDGen = [uuid.NewV7].
// Supplied options override them.
func New(client LocalKeepClient, opts ...Option) *Writer {
	if client == nil {
		panic("keeperslog: New: client must not be nil")
	}
	cfg := config{
		clock:      time.Now,
		eventIDGen: defaultUUIDv7,
		corrIDGen:  defaultUUIDv7,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Writer{
		keepClient: client,
		logger:     cfg.logger,
		clock:      cfg.clock,
		eventIDGen: cfg.eventIDGen,
		corrIDGen:  cfg.corrIDGen,
	}
}

// Append composes a structured envelope, ships it to the configured
// [LocalKeepClient.LogAppend], and returns the persisted row id on
// success. Resolution order:
//
//  1. Validate `event.EventType != ""` → otherwise [ErrInvalidEvent].
//  2. Resolve correlation id: ctx-stored (via [ContextWithCorrelationID])
//     wins; otherwise mint a fresh UUID v7 via the configured
//     [IDGenerator].
//  3. Mint a fresh `event_id` via the configured event-id [IDGenerator].
//  4. Read the OTel span context off ctx via
//     [trace.SpanContextFromContext]; embed `trace_id` and `span_id`
//     when valid, omit otherwise.
//  5. Marshal the envelope and forward to the keepclient.
//
// Errors are wrapped with the `keeperslog:` prefix; the underlying
// keepclient sentinels (`keepclient.ErrUnauthorized`, etc.) remain
// matchable via [errors.Is] through the wrap chain.
func (w *Writer) Append(ctx context.Context, event Event) (string, error) {
	if event.EventType == "" {
		return "", ErrInvalidEvent
	}

	correlationID, ok := CorrelationIDFromContext(ctx)
	if !ok {
		generated, err := w.corrIDGen()
		if err != nil {
			return "", fmt.Errorf("keeperslog: generate correlation id: %w", err)
		}
		correlationID = generated
	}

	eventID, err := w.eventIDGen()
	if err != nil {
		return "", fmt.Errorf("keeperslog: generate event id: %w", err)
	}

	payload, err := w.buildPayload(ctx, eventID, event)
	if err != nil {
		return "", err
	}

	resp, appendErr := w.keepClient.LogAppend(ctx, keepclient.LogAppendRequest{
		EventType:     event.EventType,
		CorrelationID: correlationID,
		Payload:       payload,
	})
	if appendErr != nil {
		w.log(
			ctx, "keeperslog: append failed",
			"event_type", event.EventType,
			"correlation_id", correlationID,
			"event_id", eventID,
			"err_type", fmt.Sprintf("%T", appendErr),
		)
		return "", fmt.Errorf("keeperslog: append: %w", appendErr)
	}

	w.log(
		ctx, "keeperslog: appended",
		"event_type", event.EventType,
		"correlation_id", correlationID,
		"event_id", eventID,
		"row_id", resp.ID,
	)
	return resp.ID, nil
}

// buildPayload assembles the JSON envelope persisted to the
// `keepers_log.payload` column. Field ordering is irrelevant on the
// wire; the encoded shape is documented in package godoc.
func (w *Writer) buildPayload(ctx context.Context, eventID string, event Event) (json.RawMessage, error) {
	envelope := map[string]any{
		"event_id":  eventID,
		"timestamp": w.clock().UTC().Format(time.RFC3339Nano),
	}
	if event.CausationID != "" {
		envelope["causation_id"] = event.CausationID
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		envelope["trace_id"] = sc.TraceID().String()
		envelope["span_id"] = sc.SpanID().String()
	}
	if event.Payload != nil {
		envelope["data"] = event.Payload
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("keeperslog: marshal payload: %w", err)
	}
	return encoded, nil
}

// log forwards a diagnostic message to the optional [Logger]. Nil-logger
// safe: a Writer constructed without [WithLogger] silently drops.
func (w *Writer) log(ctx context.Context, msg string, kv ...any) {
	if w.logger == nil {
		return
	}
	w.logger.Log(ctx, msg, kv...)
}

// defaultUUIDv7 is the production [IDGenerator] used by [New] when
// [WithIDGenerator] / [WithCorrelationIDGenerator] are not supplied.
// Returns a UUID v7 (time-ordered) so log entries inserted in close
// succession sort lexicographically by id.
func defaultUUIDv7() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}
