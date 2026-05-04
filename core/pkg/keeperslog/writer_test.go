package keeperslog

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// fixedTime is a deterministic timestamp used throughout the test suite
// so the timestamp field encoded into the payload can be asserted
// exactly.
var fixedTime = time.Date(2026, time.May, 4, 12, 30, 45, 0, time.UTC)

// fixedClock returns a closure honouring the [WithClock] contract.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// fixedIDGen returns a deterministic [IDGenerator] yielding `id`.
func fixedIDGen(id string) IDGenerator {
	return func() (string, error) { return id, nil }
}

// recordingLogger is the same redaction-discipline-friendly logger used
// across the M3 packages: a single `Log` method storing entries for
// later inspection.
type recordingLogger struct {
	mu      sync.Mutex
	entries []logEntry
}

type logEntry struct {
	msg string
	kv  []any
}

func (r *recordingLogger) Log(_ context.Context, msg string, kv ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]any, len(kv))
	copy(cp, kv)
	r.entries = append(r.entries, logEntry{msg: msg, kv: cp})
}

func (r *recordingLogger) snapshot() []logEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]logEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

// makeSpanContext builds a valid OTel [trace.SpanContext] from the
// supplied hex strings; t.Fatals on parse error so callers can keep tests
// linear.
func makeSpanContext(t *testing.T, traceHex, spanHex string) trace.SpanContext {
	t.Helper()
	traceBytes, err := hex.DecodeString(traceHex)
	if err != nil {
		t.Fatalf("decode trace hex: %v", err)
	}
	spanBytes, err := hex.DecodeString(spanHex)
	if err != nil {
		t.Fatalf("decode span hex: %v", err)
	}
	var tid trace.TraceID
	copy(tid[:], traceBytes)
	var sid trace.SpanID
	copy(sid[:], spanBytes)
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
}

// TestNew_NilClient_Panics asserts the constructor panics on a nil
// LocalKeepClient — same discipline as `lifecycle.New` and `cron.New`.
func TestNew_NilClient_Panics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic, got none")
		}
	}()
	_ = New(nil)
}

// TestAppend_HappyPath asserts the writer forwards EventType to keepclient
// unchanged, embeds the supplied payload data into a structured envelope
// with event_id + timestamp, and returns the keepclient ID untouched.
func TestAppend_HappyPath(t *testing.T) {
	t.Parallel()

	fake := &fakeKeepClient{resp: &keepclient.LogAppendResponse{ID: "row-1"}}
	w := New(
		fake,
		WithClock(fixedClock(fixedTime)),
		WithIDGenerator(fixedIDGen("11111111-1111-7111-8111-111111111111")),
		WithCorrelationIDGenerator(fixedIDGen("22222222-2222-7222-8222-222222222222")),
	)

	id, err := w.Append(context.Background(), Event{
		EventType: "watchkeeper_spawned",
		Payload:   map[string]any{"watchkeeper_id": "wk-7"},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if id != "row-1" {
		t.Errorf("returned id = %q, want row-1", id)
	}

	calls := fake.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	got := calls[0]
	if got.EventType != "watchkeeper_spawned" {
		t.Errorf("EventType = %q, want watchkeeper_spawned", got.EventType)
	}
	if got.CorrelationID != "22222222-2222-7222-8222-222222222222" {
		t.Errorf("CorrelationID = %q, want generated UUID", got.CorrelationID)
	}

	var payload map[string]any
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["event_id"] != "11111111-1111-7111-8111-111111111111" {
		t.Errorf("event_id = %v, want generated UUID", payload["event_id"])
	}
	if payload["timestamp"] != fixedTime.Format(time.RFC3339Nano) {
		t.Errorf("timestamp = %v, want %s", payload["timestamp"], fixedTime.Format(time.RFC3339Nano))
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil || data["watchkeeper_id"] != "wk-7" {
		t.Errorf("data = %v, want map containing watchkeeper_id=wk-7", payload["data"])
	}
	// No infrastructure-metadata fields per M2 design constraint.
	for _, banned := range []string{"deployment_id", "environment", "host", "pod"} {
		if _, present := payload[banned]; present {
			t.Errorf("payload contained banned infra field %q: %+v", banned, payload)
		}
	}
}

// TestAppend_PropagatesContextCorrelationID asserts a correlation id put
// onto the context via [ContextWithCorrelationID] flows into the
// keepclient request unchanged — the writer must NOT generate a fresh id
// when one is already present.
func TestAppend_PropagatesContextCorrelationID(t *testing.T) {
	t.Parallel()

	fake := &fakeKeepClient{}
	gen := fixedIDGen("must-not-be-used")
	w := New(
		fake,
		WithClock(fixedClock(fixedTime)),
		WithIDGenerator(fixedIDGen("event-id")),
		WithCorrelationIDGenerator(gen),
	)

	const ctxCorr = "33333333-3333-7333-8333-333333333333"
	ctx := ContextWithCorrelationID(context.Background(), ctxCorr)
	if _, err := w.Append(ctx, Event{EventType: "manifest_published"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got := fake.recordedCalls()[0]
	if got.CorrelationID != ctxCorr {
		t.Errorf("CorrelationID = %q, want ctx value %q", got.CorrelationID, ctxCorr)
	}
}

// TestAppend_GeneratesCorrelationIDWhenAbsent asserts the writer mints a
// fresh correlation id when none is on the context, and that the freshly
// minted id parses as a UUID v7.
func TestAppend_GeneratesCorrelationIDWhenAbsent(t *testing.T) {
	t.Parallel()

	fake := &fakeKeepClient{}
	w := New(fake)

	if _, err := w.Append(context.Background(), Event{EventType: "x"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got := fake.recordedCalls()[0]
	if got.CorrelationID == "" {
		t.Fatal("CorrelationID is empty; want generated UUID")
	}
	parsed, err := uuid.Parse(got.CorrelationID)
	if err != nil {
		t.Fatalf("CorrelationID = %q is not a UUID: %v", got.CorrelationID, err)
	}
	if v := parsed.Version(); v != uuid.Version(7) {
		t.Errorf("CorrelationID version = %d, want 7", v)
	}
}

// TestAppend_PropagatesTraceContext asserts that when a valid OTel
// span-context is on the context, the writer embeds trace_id + span_id
// into the structured payload.
func TestAppend_PropagatesTraceContext(t *testing.T) {
	t.Parallel()

	fake := &fakeKeepClient{}
	w := New(
		fake,
		WithClock(fixedClock(fixedTime)),
		WithIDGenerator(fixedIDGen("event-id")),
	)

	const traceHex = "0102030405060708090a0b0c0d0e0f10"
	const spanHex = "1112131415161718"
	sc := makeSpanContext(t, traceHex, spanHex)
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	if _, err := w.Append(ctx, Event{EventType: "tool_invoked"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(fake.recordedCalls()[0].Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["trace_id"] != traceHex {
		t.Errorf("trace_id = %v, want %s", payload["trace_id"], traceHex)
	}
	if payload["span_id"] != spanHex {
		t.Errorf("span_id = %v, want %s", payload["span_id"], spanHex)
	}
}

// TestAppend_OmitsTraceFieldsWhenContextHasNoSpan asserts that when no
// span-context is present, the payload OMITS trace_id / span_id (no
// empty strings on the wire — the server may reject unknown keys).
func TestAppend_OmitsTraceFieldsWhenContextHasNoSpan(t *testing.T) {
	t.Parallel()

	fake := &fakeKeepClient{}
	w := New(
		fake,
		WithClock(fixedClock(fixedTime)),
		WithIDGenerator(fixedIDGen("event-id")),
	)

	if _, err := w.Append(context.Background(), Event{EventType: "x"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	body := string(fake.recordedCalls()[0].Payload)
	if strings.Contains(body, `"trace_id"`) {
		t.Errorf("payload should omit trace_id; got %s", body)
	}
	if strings.Contains(body, `"span_id"`) {
		t.Errorf("payload should omit span_id; got %s", body)
	}
}

// TestAppend_PropagatesCausationID asserts the optional causation_id
// (provided via [Event.CausationID]) flows into the payload when set
// and is omitted when empty.
func TestAppend_PropagatesCausationID(t *testing.T) {
	t.Parallel()

	t.Run("set", func(t *testing.T) {
		t.Parallel()
		fake := &fakeKeepClient{}
		w := New(
			fake,
			WithClock(fixedClock(fixedTime)),
			WithIDGenerator(fixedIDGen("event-id")),
			WithCorrelationIDGenerator(fixedIDGen("corr-id")),
		)
		const cause = "44444444-4444-7444-8444-444444444444"
		if _, err := w.Append(context.Background(), Event{
			EventType:   "watchkeeper_retired",
			CausationID: cause,
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(fake.recordedCalls()[0].Payload, &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if payload["causation_id"] != cause {
			t.Errorf("causation_id = %v, want %s", payload["causation_id"], cause)
		}
	})

	t.Run("unset", func(t *testing.T) {
		t.Parallel()
		fake := &fakeKeepClient{}
		w := New(fake)
		if _, err := w.Append(context.Background(), Event{EventType: "x"}); err != nil {
			t.Fatalf("Append: %v", err)
		}
		body := string(fake.recordedCalls()[0].Payload)
		if strings.Contains(body, `"causation_id"`) {
			t.Errorf("payload should omit causation_id; got %s", body)
		}
	})
}

// TestAppend_EmptyEventType_ReturnsErrInvalidEvent asserts synchronous
// validation: empty EventType bails before the keepclient is touched.
func TestAppend_EmptyEventType_ReturnsErrInvalidEvent(t *testing.T) {
	t.Parallel()

	fake := &fakeKeepClient{}
	w := New(fake)

	id, err := w.Append(context.Background(), Event{EventType: ""})
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("err = %v, want ErrInvalidEvent", err)
	}
	if id != "" {
		t.Errorf("id = %q, want empty", id)
	}
	if fake.callCount() != 0 {
		t.Errorf("keepclient called %d times; want 0", fake.callCount())
	}
}

// TestAppend_KeepClientErrorPropagates asserts that a keepclient error
// surfaces through Append wrapped with the package prefix and that the
// underlying error chain still matches the keepclient sentinel via
// `errors.Is`.
func TestAppend_KeepClientErrorPropagates(t *testing.T) {
	t.Parallel()

	fake := &fakeKeepClient{respErr: keepclient.ErrUnauthorized}
	w := New(fake)

	id, err := w.Append(context.Background(), Event{EventType: "x"})
	if id != "" {
		t.Errorf("id = %q, want empty on error", id)
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, keepclient.ErrUnauthorized) {
		t.Errorf("errors.Is(err, ErrUnauthorized) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "keeperslog") {
		t.Errorf("err.Error() = %q, want package prefix", err.Error())
	}
}

// TestAppend_RoundTripStructuredFields asserts a real round-trip from
// the writer all the way through to the LogAppendRequest payload — the
// payload must decode into a typed envelope with all expected fields.
func TestAppend_RoundTripStructuredFields(t *testing.T) {
	t.Parallel()

	fake := &fakeKeepClient{}
	w := New(
		fake,
		WithClock(fixedClock(fixedTime)),
		WithIDGenerator(fixedIDGen("evt-1")),
		WithCorrelationIDGenerator(fixedIDGen("55555555-5555-7555-8555-555555555555")),
	)

	const traceHex = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const spanHex = "bbbbbbbbbbbbbbbb"
	sc := makeSpanContext(t, traceHex, spanHex)
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	if _, err := w.Append(ctx, Event{
		EventType:   "manifest_proposed",
		CausationID: "66666666-6666-7666-8666-666666666666",
		Payload:     map[string]any{"manifest_version": 7, "draft": true},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got := fake.recordedCalls()[0]
	if got.EventType != "manifest_proposed" {
		t.Errorf("EventType = %q, want manifest_proposed", got.EventType)
	}
	if got.CorrelationID != "55555555-5555-7555-8555-555555555555" {
		t.Errorf("CorrelationID = %q, want generated UUID", got.CorrelationID)
	}

	type envelope struct {
		EventID     string                 `json:"event_id"`
		Timestamp   string                 `json:"timestamp"`
		TraceID     string                 `json:"trace_id,omitempty"`
		SpanID      string                 `json:"span_id,omitempty"`
		CausationID string                 `json:"causation_id,omitempty"`
		Data        map[string]interface{} `json:"data,omitempty"`
	}
	var env envelope
	if err := json.Unmarshal(got.Payload, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.EventID != "evt-1" {
		t.Errorf("EventID = %q, want evt-1", env.EventID)
	}
	if env.Timestamp != fixedTime.Format(time.RFC3339Nano) {
		t.Errorf("Timestamp = %q, want %s", env.Timestamp, fixedTime.Format(time.RFC3339Nano))
	}
	if env.TraceID != traceHex {
		t.Errorf("TraceID = %q, want %s", env.TraceID, traceHex)
	}
	if env.SpanID != spanHex {
		t.Errorf("SpanID = %q, want %s", env.SpanID, spanHex)
	}
	if env.CausationID != "66666666-6666-7666-8666-666666666666" {
		t.Errorf("CausationID = %q, want supplied id", env.CausationID)
	}
	if env.Data["manifest_version"].(float64) != 7 {
		t.Errorf("data.manifest_version = %v, want 7", env.Data["manifest_version"])
	}
	if env.Data["draft"] != true {
		t.Errorf("data.draft = %v, want true", env.Data["draft"])
	}
}

// TestCorrelationIDFromContext_Roundtrip asserts the context-helper pair
// round-trips a value through context.Value and returns ok=false when
// nothing is present.
func TestCorrelationIDFromContext_Roundtrip(t *testing.T) {
	t.Parallel()

	if _, ok := CorrelationIDFromContext(context.Background()); ok {
		t.Error("CorrelationIDFromContext on bare ctx returned ok=true")
	}

	ctx := ContextWithCorrelationID(context.Background(), "id-1")
	got, ok := CorrelationIDFromContext(ctx)
	if !ok || got != "id-1" {
		t.Errorf("CorrelationIDFromContext = (%q, %v), want (\"id-1\", true)", got, ok)
	}
}

// TestContextWithCorrelationID_EmptyValue_NoOp asserts an empty value
// does NOT pollute the ctx — callers using ContextWithCorrelationID(ctx,
// "") expect a passthrough so a downstream Append still auto-generates.
func TestContextWithCorrelationID_EmptyValue_NoOp(t *testing.T) {
	t.Parallel()

	ctx := ContextWithCorrelationID(context.Background(), "")
	if _, ok := CorrelationIDFromContext(ctx); ok {
		t.Error("empty correlation id should not be stored on ctx")
	}
}

// TestAppend_LoggerNotifiedOnSuccess asserts that when [WithLogger] is
// wired the writer emits a structured success log carrying event_type
// and correlation_id (NOT payload data, per the redaction discipline).
func TestAppend_LoggerNotifiedOnSuccess(t *testing.T) {
	t.Parallel()

	fake := &fakeKeepClient{}
	rec := &recordingLogger{}
	w := New(
		fake,
		WithLogger(rec),
		WithClock(fixedClock(fixedTime)),
		WithIDGenerator(fixedIDGen("evt-1")),
		WithCorrelationIDGenerator(fixedIDGen("corr-1")),
	)
	if _, err := w.Append(context.Background(), Event{
		EventType: "tool_invoked",
		Payload:   map[string]any{"secret_token": "should-never-leak"},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	entries := rec.snapshot()
	if len(entries) == 0 {
		t.Fatal("expected logger to be called; got 0 entries")
	}
	for _, e := range entries {
		// Format the entry through %+v so any payload bleed is caught.
		repr := joinKVs(e.kv)
		if strings.Contains(repr, "should-never-leak") {
			t.Errorf("logger entry leaked payload data: %s", repr)
		}
	}
}

// joinKVs concatenates a kv slice into a string for grep-style assertions.
func joinKVs(kv []any) string {
	parts := make([]string, 0, len(kv))
	for _, v := range kv {
		parts = append(parts, toString(v))
	}
	return strings.Join(parts, " ")
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// TestAppend_IDGeneratorError_Wrapped asserts that an ID-generator
// failure surfaces as a wrapped error and the keepclient is NOT called.
func TestAppend_IDGeneratorError_Wrapped(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("rng failure")
	fake := &fakeKeepClient{}
	w := New(
		fake,
		WithIDGenerator(func() (string, error) { return "", sentinel }),
	)
	if _, err := w.Append(context.Background(), Event{EventType: "x"}); err == nil {
		t.Fatal("expected error, got nil")
	} else if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, sentinel) = false; err = %v", err)
	}
	if fake.callCount() != 0 {
		t.Errorf("keepclient called %d times on id-gen failure; want 0", fake.callCount())
	}
}

// TestAppend_ContextCancellationPropagates asserts a cancelled context
// surfaces back through Append unchanged via the keepclient layer.
func TestAppend_ContextCancellationPropagates(t *testing.T) {
	t.Parallel()

	fake := &fakeKeepClient{respErr: context.Canceled}
	w := New(fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.Append(ctx, Event{EventType: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
	}
}

// TestKeepClientCompileTimeAssertion is the M3 lifecycle / cron
// pattern: the production keepclient.Client must satisfy the local
// LocalKeepClient interface. Lives in `_test.go` so production code
// avoids importing the concrete client type.
func TestKeepClientCompileTimeAssertion(t *testing.T) {
	t.Parallel()
	var _ LocalKeepClient = (*keepclient.Client)(nil)
}
