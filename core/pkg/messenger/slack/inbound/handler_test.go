package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
)

// signingSecret is the test fixture's stable signing-secret value. The
// helper [newSignedRequest] pairs it with a stable timestamp so the
// signature stays deterministic across re-runs. NOT a real secret —
// the repeating-letter shape keeps gitleaks's entropy filter quiet
// while still exercising the HMAC code path.
const signingSecret = "test-fixture-signing-secret-not-a-real-key" //nolint:gosec // test fixture, not a real secret

// fixedTimestamp is the deterministic event-time injected by the test
// fixtures. Pinned so the constant-time signature regression test, the
// audit-payload assertions, and the stale-timestamp negative paths all
// agree on a single epoch.
var fixedTimestamp = time.Unix(1_700_000_000, 0)

// fakeEventDispatcher is the recording [EventDispatcher] the handler
// tests use to assert dispatch happened (or did not happen) without
// reaching for a mocking framework. AC6 of M6.3.a pins this discipline.
type fakeEventDispatcher struct {
	mu       sync.Mutex
	events   []Event
	returnFn func(Event) error
}

func (f *fakeEventDispatcher) DispatchEvent(_ context.Context, ev Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	if f.returnFn != nil {
		return f.returnFn(ev)
	}
	return nil
}

func (f *fakeEventDispatcher) calls() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Event, len(f.events))
	copy(out, f.events)
	return out
}

// fakeInteractionDispatcher mirrors [fakeEventDispatcher] for the
// Interactivity surface.
type fakeInteractionDispatcher struct {
	mu      sync.Mutex
	payload []Interaction
}

func (f *fakeInteractionDispatcher) DispatchInteraction(_ context.Context, p Interaction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payload = append(f.payload, p)
	return nil
}

func (f *fakeInteractionDispatcher) calls() []Interaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Interaction, len(f.payload))
	copy(out, f.payload)
	return out
}

// fakeKeepClient is the recording [keeperslog.LocalKeepClient] the
// audit-event tests use to assert event_type + payload contents
// without standing up the full HTTP keepclient stack.
type fakeKeepClient struct {
	mu   sync.Mutex
	rows []keepclient.LogAppendRequest
}

func (f *fakeKeepClient) LogAppend(_ context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, req)
	return &keepclient.LogAppendResponse{ID: strconv.Itoa(len(f.rows))}, nil
}

func (f *fakeKeepClient) appended() []keepclient.LogAppendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]keepclient.LogAppendRequest, len(f.rows))
	copy(out, f.rows)
	return out
}

// newTestWriter builds a [keeperslog.Writer] backed by a fresh fake
// keepclient. Returns both so the test can assert on the rows the
// handler appended.
func newTestWriter(t *testing.T) (*keeperslog.Writer, *fakeKeepClient) {
	t.Helper()
	fkc := &fakeKeepClient{}
	w := keeperslog.New(fkc, keeperslog.WithClock(func() time.Time { return fixedTimestamp }))
	return w, fkc
}

// newSignedRequest constructs an HTTP request signed with the test
// fixture's signing secret per the Slack v0 algorithm. AC6 mandates
// that tests re-use the SAME HMAC code path the verifier consults;
// this helper calls [Sign], the exported wrapper around the verifier's
// internal computation.
func newSignedRequest(t *testing.T, method, target, contentType string, body []byte) *http.Request {
	t.Helper()
	tsStr := strconv.FormatInt(fixedTimestamp.Unix(), 10)
	sig := Sign([]byte(signingSecret), tsStr, body)
	req := httptest.NewRequestWithContext(context.Background(), method, target, bytes.NewReader(body))
	req.Header.Set(headerSlackSignature, sig)
	req.Header.Set(headerSlackTimestamp, tsStr)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

// newHandlerForTest assembles a handler with a frozen clock pinned to
// fixedTimestamp so the signature-verifier's window check evaluates
// deterministically. Tests inject the signing secret + dispatchers
// + audit writer.
func newHandlerForTest(
	t *testing.T,
	evd EventDispatcher,
	intd InteractionDispatcher,
	w *keeperslog.Writer,
	opts ...Option,
) http.Handler {
	t.Helper()
	all := []Option{
		WithSigningSecret([]byte(signingSecret)),
		WithEventDispatcher(evd),
		WithInteractionDispatcher(intd),
		WithAuditAppender(w),
		WithClock(func() time.Time { return fixedTimestamp }),
	}
	all = append(all, opts...)
	h, err := NewHandler(all...)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

// TestEventsAPI_URLVerification asserts the url_verification handshake
// returns the supplied challenge as text/plain, ACKs 200, and does
// NOT call the EventDispatcher (the handshake is handler-internal
// state).
func TestEventsAPI_URLVerification(t *testing.T) {
	t.Parallel()

	evd := &fakeEventDispatcher{}
	intd := &fakeInteractionDispatcher{}
	w, fkc := newTestWriter(t)
	h := newHandlerForTest(t, evd, intd, w)

	body := []byte(`{"token":"xyz","challenge":"3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P","type":"url_verification"}`)
	req := newSignedRequest(t, http.MethodPost, "/v1/slack/events", "application/json", body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain*", got)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P" {
		t.Errorf("body = %q, want challenge echo", got)
	}
	if calls := evd.calls(); len(calls) != 0 {
		t.Errorf("EventDispatcher called %d times, want 0 (url_verification path)", len(calls))
	}

	// Audit assertion: exactly ONE slack_webhook_received row.
	rows := fkc.appended()
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1; rows=%+v", len(rows), rows)
	}
	if rows[0].EventType != auditEventReceived {
		t.Errorf("audit event_type = %q, want %q", rows[0].EventType, auditEventReceived)
	}
	if bytes.Contains(rows[0].Payload, []byte("3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P")) {
		t.Errorf("audit payload leaked challenge body — PII redaction broken: %s", rows[0].Payload)
	}
}

// TestEventsAPI_EventCallback asserts the event_callback envelope is
// decoded, dispatched to the EventDispatcher, and ACKed 200 with the
// happy-path audit row.
func TestEventsAPI_EventCallback(t *testing.T) {
	t.Parallel()

	evd := &fakeEventDispatcher{}
	intd := &fakeInteractionDispatcher{}
	w, fkc := newTestWriter(t)
	h := newHandlerForTest(t, evd, intd, w)

	body := []byte(`{
		"token":"xyz",
		"team_id":"T0123456",
		"api_app_id":"A0123456",
		"type":"event_callback",
		"event_id":"Ev0123",
		"event_time":1700000000,
		"event":{"type":"message","channel":"C0","text":"hi"}
	}`)
	req := newSignedRequest(t, http.MethodPost, "/v1/slack/events", "application/json", body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	calls := evd.calls()
	if len(calls) != 1 {
		t.Fatalf("EventDispatcher calls = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.TeamID != "T0123456" {
		t.Errorf("TeamID = %q, want T0123456", got.TeamID)
	}
	if got.APIAppID != "A0123456" {
		t.Errorf("APIAppID = %q, want A0123456", got.APIAppID)
	}
	if got.EventID != "Ev0123" {
		t.Errorf("EventID = %q, want Ev0123", got.EventID)
	}
	if got.Type != "message" {
		t.Errorf("Type = %q, want message", got.Type)
	}
	if !bytes.Contains(got.Inner, []byte(`"channel":"C0"`)) {
		t.Errorf("Inner did not carry inner event JSON: %s", got.Inner)
	}

	// Audit assertion: exactly ONE slack_webhook_received row, NO body.
	rows := fkc.appended()
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	if rows[0].EventType != auditEventReceived {
		t.Errorf("audit event_type = %q, want %q", rows[0].EventType, auditEventReceived)
	}
	if bytes.Contains(rows[0].Payload, []byte(`"text":"hi"`)) {
		t.Errorf("audit payload leaked body content: %s", rows[0].Payload)
	}
}

// TestInteractivity_BlockActions asserts a block_actions payload is
// decoded from the form-encoded body, dispatched, and ACKed 200.
func TestInteractivity_BlockActions(t *testing.T) {
	t.Parallel()

	evd := &fakeEventDispatcher{}
	intd := &fakeInteractionDispatcher{}
	w, fkc := newTestWriter(t)
	h := newHandlerForTest(t, evd, intd, w)

	payload := `{"type":"block_actions","team":{"id":"T0123"},"api_app_id":"A0123","actions":[{"action_id":"approve","value":"yes"}]}`
	form := url.Values{}
	form.Set("payload", payload)
	body := []byte(form.Encode())

	req := newSignedRequest(t, http.MethodPost, "/v1/slack/interactions", "application/x-www-form-urlencoded", body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	calls := intd.calls()
	if len(calls) != 1 {
		t.Fatalf("InteractionDispatcher calls = %d, want 1", len(calls))
	}
	if calls[0].Type != "block_actions" {
		t.Errorf("Type = %q, want block_actions", calls[0].Type)
	}
	if !bytes.Contains(calls[0].Raw, []byte(`"action_id":"approve"`)) {
		t.Errorf("Raw payload missing action_id: %s", calls[0].Raw)
	}

	rows := fkc.appended()
	if len(rows) != 1 || rows[0].EventType != auditEventReceived {
		t.Fatalf("audit rows = %+v, want one slack_webhook_received", rows)
	}
}

// TestInteractivity_ViewSubmission asserts a view_submission payload
// dispatches with the correct type field.
func TestInteractivity_ViewSubmission(t *testing.T) {
	t.Parallel()

	evd := &fakeEventDispatcher{}
	intd := &fakeInteractionDispatcher{}
	w, _ := newTestWriter(t)
	h := newHandlerForTest(t, evd, intd, w)

	payload := `{"type":"view_submission","team":{"id":"T1"},"api_app_id":"A1","view":{"id":"V1"}}`
	form := url.Values{}
	form.Set("payload", payload)
	body := []byte(form.Encode())

	req := newSignedRequest(t, http.MethodPost, "/v1/slack/interactions", "application/x-www-form-urlencoded", body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	calls := intd.calls()
	if len(calls) != 1 || calls[0].Type != "view_submission" {
		t.Fatalf("dispatcher calls = %+v, want one view_submission", calls)
	}
}

// TestNegative_MissingSignature asserts a request without the
// X-Slack-Signature header returns 401, does not invoke the
// dispatcher, and emits a slack_webhook_rejected audit row with
// reason=missing_header.
func TestNegative_MissingSignature(t *testing.T) {
	t.Parallel()

	evd := &fakeEventDispatcher{}
	intd := &fakeInteractionDispatcher{}
	w, fkc := newTestWriter(t)
	h := newHandlerForTest(t, evd, intd, w)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/slack/events", strings.NewReader(`{"type":"url_verification"}`))
	req.Header.Set(headerSlackTimestamp, strconv.FormatInt(fixedTimestamp.Unix(), 10))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assertRejected(t, rr, fkc, evd, intd, http.StatusUnauthorized, reasonMissingHeader)
}

// TestNegative_MissingTimestamp asserts a request without the
// X-Slack-Request-Timestamp header returns 401 with
// reason=missing_header.
func TestNegative_MissingTimestamp(t *testing.T) {
	t.Parallel()

	evd := &fakeEventDispatcher{}
	intd := &fakeInteractionDispatcher{}
	w, fkc := newTestWriter(t)
	h := newHandlerForTest(t, evd, intd, w)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/slack/events", strings.NewReader(`{}`))
	req.Header.Set(headerSlackSignature, "v0=deadbeef")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assertRejected(t, rr, fkc, evd, intd, http.StatusUnauthorized, reasonMissingHeader)
}

// TestNegative_BadSignature asserts a tampered signature is rejected
// with 401 + reason=bad_signature without dispatching.
func TestNegative_BadSignature(t *testing.T) {
	t.Parallel()

	evd := &fakeEventDispatcher{}
	intd := &fakeInteractionDispatcher{}
	w, fkc := newTestWriter(t)
	h := newHandlerForTest(t, evd, intd, w)

	body := []byte(`{"type":"url_verification","challenge":"x"}`)
	req := newSignedRequest(t, http.MethodPost, "/v1/slack/events", "application/json", body)
	// Tamper with the last hex nibble.
	sig := req.Header.Get(headerSlackSignature)
	tampered := sig[:len(sig)-1] + "0"
	if tampered == sig {
		tampered = sig[:len(sig)-1] + "1"
	}
	req.Header.Set(headerSlackSignature, tampered)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assertRejected(t, rr, fkc, evd, intd, http.StatusUnauthorized, reasonBadSignature)
}

// TestNegative_StaleTimestamp asserts a timestamp older than the
// configured window is rejected with 401 + reason=stale_timestamp
// without spending a signature compare.
func TestNegative_StaleTimestamp(t *testing.T) {
	t.Parallel()

	evd := &fakeEventDispatcher{}
	intd := &fakeInteractionDispatcher{}
	w, fkc := newTestWriter(t)
	h := newHandlerForTest(t, evd, intd, w)

	body := []byte(`{"type":"url_verification","challenge":"x"}`)
	// Sign with a timestamp 6 minutes before the handler's frozen
	// clock — outside the default 5-minute window.
	staleTS := fixedTimestamp.Add(-6 * time.Minute)
	tsStr := strconv.FormatInt(staleTS.Unix(), 10)
	sig := Sign([]byte(signingSecret), tsStr, body)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/slack/events", bytes.NewReader(body))
	req.Header.Set(headerSlackSignature, sig)
	req.Header.Set(headerSlackTimestamp, tsStr)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assertRejected(t, rr, fkc, evd, intd, http.StatusUnauthorized, reasonStaleTimestamp)
}

// TestNegative_MalformedJSON asserts a body that passes the signature
// check but fails JSON decode returns 400 with reason=malformed_json.
func TestNegative_MalformedJSON(t *testing.T) {
	t.Parallel()

	evd := &fakeEventDispatcher{}
	intd := &fakeInteractionDispatcher{}
	w, fkc := newTestWriter(t)
	h := newHandlerForTest(t, evd, intd, w)

	body := []byte(`{"type":"event_callback", "event":{`) // truncated JSON
	req := newSignedRequest(t, http.MethodPost, "/v1/slack/events", "application/json", body)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assertRejected(t, rr, fkc, evd, intd, http.StatusBadRequest, reasonMalformedJSON)
}

// TestNegative_OversizeBody asserts a body larger than the configured
// max returns 413 with reason=oversize_body. The handler reads the
// body via [http.MaxBytesReader] so the cap is enforced even for
// malformed signatures.
func TestNegative_OversizeBody(t *testing.T) {
	t.Parallel()

	evd := &fakeEventDispatcher{}
	intd := &fakeInteractionDispatcher{}
	w, fkc := newTestWriter(t)
	// Cap at 16 bytes for the test; the body below is well over.
	h := newHandlerForTest(t, evd, intd, w, WithMaxBodyBytes(16))

	body := []byte(`{"type":"event_callback","event_id":"Ev_oversize_body_test_payload"}`)
	req := newSignedRequest(t, http.MethodPost, "/v1/slack/events", "application/json", body)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%q", rr.Code, rr.Body.String())
	}
	if calls := evd.calls(); len(calls) != 0 {
		t.Errorf("dispatcher called %d times, want 0", len(calls))
	}
	rows := fkc.appended()
	if len(rows) != 1 || rows[0].EventType != auditEventRejected {
		t.Fatalf("audit rows = %+v, want one slack_webhook_rejected", rows)
	}
	if !payloadHasReason(t, rows[0].Payload, reasonOversizeBody) {
		t.Errorf("audit payload missing reason=%s: %s", reasonOversizeBody, rows[0].Payload)
	}
}

// TestNegative_NonPOSTMethod asserts non-POST methods are rejected at
// the method gate. Slack only POSTs to these routes; rejecting other
// methods early gives clearer feedback than the signature gate's 401.
func TestNegative_NonPOSTMethod(t *testing.T) {
	t.Parallel()

	evd := &fakeEventDispatcher{}
	intd := &fakeInteractionDispatcher{}
	w, _ := newTestWriter(t)
	h := newHandlerForTest(t, evd, intd, w)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/slack/events", http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

// assertRejected pins the negative-path contract: 401/400/413 status,
// no dispatcher calls, exactly one slack_webhook_rejected audit row
// carrying the expected `reason`.
func assertRejected(
	t *testing.T,
	rr *httptest.ResponseRecorder,
	fkc *fakeKeepClient,
	evd *fakeEventDispatcher,
	intd *fakeInteractionDispatcher,
	wantStatus int,
	wantReason string,
) {
	t.Helper()
	if rr.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%q", rr.Code, wantStatus, rr.Body.String())
	}
	if calls := evd.calls(); len(calls) != 0 {
		t.Errorf("EventDispatcher called %d times, want 0", len(calls))
	}
	if calls := intd.calls(); len(calls) != 0 {
		t.Errorf("InteractionDispatcher called %d times, want 0", len(calls))
	}
	rows := fkc.appended()
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1; rows=%+v", len(rows), rows)
	}
	if rows[0].EventType != auditEventRejected {
		t.Errorf("audit event_type = %q, want %q", rows[0].EventType, auditEventRejected)
	}
	if !payloadHasReason(t, rows[0].Payload, wantReason) {
		t.Errorf("audit payload missing reason=%s: %s", wantReason, rows[0].Payload)
	}
}

// payloadHasReason decodes the keeperslog envelope's `data.reason`
// field and reports whether it equals the expected value. The
// envelope shape is fixed by [keeperslog.Writer.Append] (data + ID +
// timestamp).
func payloadHasReason(t *testing.T, payload json.RawMessage, want string) bool {
	t.Helper()
	var env struct {
		Data struct {
			Reason string `json:"reason"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; payload=%s", err, payload)
	}
	return env.Data.Reason == want
}

// TestNewHandler_RequiresSigningSecret asserts the constructor refuses
// to build a handler with an empty signing secret. A handler with no
// signing secret cannot verify anything; failing closed at construction
// surfaces the operator's misconfiguration before any request is
// served.
func TestNewHandler_RequiresSigningSecret(t *testing.T) {
	t.Parallel()

	_, err := NewHandler()
	if err == nil {
		t.Fatal("NewHandler with no signing secret: err = nil, want error")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("err = %v, want ErrInvalidConfig", err)
	}
}

// TestHandler_RawBodyReadOnce asserts the handler does not double-read
// http.Request.Body. We feed a body via a custom io.ReadCloser whose
// Read method panics on the second invocation; the test passes if the
// handler completes without panic.
func TestHandler_RawBodyReadOnce(t *testing.T) {
	t.Parallel()

	evd := &fakeEventDispatcher{}
	intd := &fakeInteractionDispatcher{}
	w, _ := newTestWriter(t)
	h := newHandlerForTest(t, evd, intd, w)

	body := []byte(`{"type":"url_verification","challenge":"abc"}`)
	tsStr := strconv.FormatInt(fixedTimestamp.Unix(), 10)
	sig := Sign([]byte(signingSecret), tsStr, body)

	rc := &exhaustOnceReader{src: bytes.NewReader(body)}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/slack/events", rc)
	req.Header.Set(headerSlackSignature, sig)
	req.Header.Set(headerSlackTimestamp, tsStr)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
}

// exhaustOnceReader returns the wrapped bytes once, then panics on any
// subsequent Read past EOF — pinning that the handler does NOT call
// Read again after the body is consumed for the signature check.
type exhaustOnceReader struct {
	src      *bytes.Reader
	consumed bool
}

func (r *exhaustOnceReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if errors.Is(err, io.EOF) {
		if r.consumed {
			panic("handler attempted to re-read body past EOF")
		}
		r.consumed = true
	}
	return n, err
}

func (r *exhaustOnceReader) Close() error { return nil }
