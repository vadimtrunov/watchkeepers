package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// signedWebhookRequest constructs a httptest request with a correctly
// signed body using the exported [SignWebhook] helper — AC: tests
// construct the request through the SAME HMAC code path the verifier
// consults.
func signedWebhookRequest(t *testing.T, secret []byte, body []byte, now time.Time) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := SignWebhook(secret, ts, body)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/webhook", bytes.NewReader(body))
	req.Header.Set(headerWebhookSignature, sig)
	req.Header.Set(headerWebhookTimestamp, ts)
	return req
}

// newTestWebhook builds a [*Webhook] with hand-rolled fakes seeded with
// one stored proposal. Returns the handler + all fakes for assertion.
func newTestWebhook(t *testing.T) (
	wh *Webhook,
	pub *fakePublisher,
	syncer *fakeSchedulerSyncer,
	store *fakeProposalLookup,
	clk *fakeClock,
	idGen *fakeIDGenerator,
	logger *fakeLogger,
	stored Proposal,
	secret []byte,
	decisions *fakeDecisionRecorder,
) {
	t.Helper()
	pub = &fakePublisher{}
	syncer = &fakeSchedulerSyncer{}
	store = newFakeProposalLookup()
	clk = newFakeClock(time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC))
	idGen = &fakeIDGenerator{}
	logger = &fakeLogger{}
	decisions = newFakeDecisionRecorder()
	stored = newTestProposal()
	store.put(stored)
	secret = []byte("test-webhook-secret-bytes")
	wh = NewWebhook(WebhookDeps{
		SecretResolver: constSecretResolver(secret, nil),
		Lookup:         store,
		Decisions:      decisions,
		Syncer:         syncer,
		SourceResolver: constSourceResolver("platform", nil),
		Publisher:      pub,
		Clock:          clk,
		IDGenerator:    idGen,
		Logger:         logger,
	})
	return
}

func TestWebhook_NewWebhook_PanicsOnNilDeps(t *testing.T) {
	t.Helper()
	mk := func(mutate func(*WebhookDeps)) (panicked bool, msg string) {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				msg = fmt.Sprintf("%v", r)
			}
		}()
		deps := WebhookDeps{
			SecretResolver: constSecretResolver([]byte("x"), nil),
			Lookup:         newFakeProposalLookup(),
			Decisions:      newFakeDecisionRecorder(),
			Syncer:         &fakeSchedulerSyncer{},
			SourceResolver: constSourceResolver("p", nil),
			Publisher:      &fakePublisher{},
			Clock:          newFakeClock(time.Now()),
			IDGenerator:    &fakeIDGenerator{},
		}
		mutate(&deps)
		_ = NewWebhook(deps)
		return
	}
	tests := []struct {
		name    string
		mutate  func(*WebhookDeps)
		wantSub string
	}{
		{"SecretResolver", func(d *WebhookDeps) { d.SecretResolver = nil }, "SecretResolver"},
		{"Lookup", func(d *WebhookDeps) { d.Lookup = nil }, "Lookup"},
		{"Decisions", func(d *WebhookDeps) { d.Decisions = nil }, "Decisions"},
		{"Syncer", func(d *WebhookDeps) { d.Syncer = nil }, "Syncer"},
		{"SourceResolver", func(d *WebhookDeps) { d.SourceResolver = nil }, "SourceResolver"},
		{"Publisher", func(d *WebhookDeps) { d.Publisher = nil }, "Publisher"},
		{"Clock", func(d *WebhookDeps) { d.Clock = nil }, "Clock"},
		{"IDGenerator", func(d *WebhookDeps) { d.IDGenerator = nil }, "IDGenerator"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			panicked, msg := mk(tt.mutate)
			if !panicked {
				t.Fatalf("expected panic")
			}
			if !strings.Contains(msg, tt.wantSub) {
				t.Errorf("panic msg missing %q: %s", tt.wantSub, msg)
			}
		})
	}
}

func TestWebhook_NewWebhook_NilLoggerAccepted(_ *testing.T) {
	deps := WebhookDeps{
		SecretResolver: constSecretResolver([]byte("x"), nil),
		Lookup:         newFakeProposalLookup(),
		Decisions:      newFakeDecisionRecorder(),
		Syncer:         &fakeSchedulerSyncer{},
		SourceResolver: constSourceResolver("p", nil),
		Publisher:      &fakePublisher{},
		Clock:          newFakeClock(time.Now()),
		IDGenerator:    &fakeIDGenerator{},
	}
	// Should not panic.
	_ = NewWebhook(deps)
}

func TestWebhook_Approved_HappyPath(t *testing.T) {
	wh, pub, syncer, _, clk, _, _, stored, secret, _ := newTestWebhook(t)

	payloadBytes := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		PRURL:      "https://github.com/example/tools/pull/42",
		MergedSHA:  "abcdef0123456789",
		Approver:   "octocat",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, payloadBytes, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", rw.Code)
	}
	calls := syncer.snapshot()
	if len(calls) != 1 || calls[0] != "platform" {
		t.Errorf("SyncOnce calls: %v", calls)
	}
	events := pub.eventsForTopic(TopicToolApproved)
	if len(events) != 1 {
		t.Fatalf("expected 1 tool_approved event, got %d", len(events))
	}
	got, ok := events[0].event.(ToolApproved)
	if !ok {
		t.Fatalf("event type: %T", events[0].event)
	}
	if got.ProposalID != stored.ID {
		t.Errorf("ProposalID mismatch")
	}
	if got.Route != RouteGitPR {
		t.Errorf("Route: want git-pr got %s", got.Route)
	}
	if got.SourceName != "platform" {
		t.Errorf("SourceName: %s", got.SourceName)
	}
	if got.PRURL != "https://github.com/example/tools/pull/42" {
		t.Errorf("PRURL: %s", got.PRURL)
	}
	if got.MergedSHA != "abcdef0123456789" {
		t.Errorf("MergedSHA: %s", got.MergedSHA)
	}
	if got.CorrelationID != stored.CorrelationID {
		t.Errorf("CorrelationID: want %s got %s", stored.CorrelationID, got.CorrelationID)
	}
}

func TestWebhook_Rejected_HappyPath(t *testing.T) {
	wh, pub, syncer, _, clk, _, _, stored, secret, _ := newTestWebhook(t)

	payloadBytes := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "rejected",
		Approver:   "octocat",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, payloadBytes, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", rw.Code)
	}
	if len(syncer.snapshot()) != 0 {
		t.Errorf("rejected must not trigger SyncOnce: %v", syncer.snapshot())
	}
	events := pub.eventsForTopic(TopicToolRejected)
	if len(events) != 1 {
		t.Fatalf("expected 1 tool_rejected event, got %d", len(events))
	}
	got := events[0].event.(ToolRejected)
	if got.Route != RouteGitPR {
		t.Errorf("Route: %s", got.Route)
	}
}

func TestWebhook_Approved_SyncErrorStillPublishes(t *testing.T) {
	wh, pub, syncer, _, clk, _, _, stored, secret, _ := newTestWebhook(t)
	syncer.err = errors.New("git server unreachable")

	body := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Approver:   "octocat",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, body, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)

	if rw.Code != http.StatusAccepted {
		t.Fatalf("status: want 202 got %d", rw.Code)
	}
	if len(pub.eventsForTopic(TopicToolApproved)) != 1 {
		t.Errorf("publish must succeed even when sync fails")
	}
}

func TestWebhook_MissingSignatureHeader_401(t *testing.T) {
	wh, _, _, _, _, _, _, stored, _, _ := newTestWebhook(t)
	body := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  1,
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/webhook", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("status: want 401 got %d", rw.Code)
	}
}

func TestWebhook_BadSignature_401(t *testing.T) {
	wh, _, _, _, clk, _, _, stored, _, _ := newTestWebhook(t)
	body := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	ts := strconv.FormatInt(clk.Now().Unix(), 10)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/webhook", bytes.NewReader(body))
	req.Header.Set(headerWebhookSignature, "sha256=00deadbeef")
	req.Header.Set(headerWebhookTimestamp, ts)
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("status: want 401 got %d", rw.Code)
	}
}

func TestWebhook_PayloadTampering_RejectsAfterSignedBody(t *testing.T) {
	wh, _, _, _, clk, _, _, stored, secret, _ := newTestWebhook(t)
	originalBody := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	ts := strconv.FormatInt(clk.Now().Unix(), 10)
	sig := SignWebhook(secret, ts, originalBody)

	// Tamper the body AFTER signing.
	tampered := bytes.Replace(originalBody, []byte("approved"), []byte("rejected"), 1)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/webhook", bytes.NewReader(tampered))
	req.Header.Set(headerWebhookSignature, sig)
	req.Header.Set(headerWebhookTimestamp, ts)
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("status: want 401 got %d", rw.Code)
	}
}

func TestWebhook_StaleTimestamp_401(t *testing.T) {
	wh, _, _, _, clk, _, _, stored, secret, _ := newTestWebhook(t)
	body := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	// Sign with a timestamp 10 minutes in the past — beyond the
	// default 5-minute window.
	stale := clk.Now().Add(-10 * time.Minute)
	req := signedWebhookRequest(t, secret, body, stale)
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("status: want 401 got %d", rw.Code)
	}
}

func TestWebhook_NonIntegerTimestamp_401(t *testing.T) {
	wh, _, _, _, clk, _, _, stored, secret, _ := newTestWebhook(t)
	body := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	ts := "not-a-number"
	sig := SignWebhook(secret, ts, body)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/webhook", bytes.NewReader(body))
	req.Header.Set(headerWebhookSignature, sig)
	req.Header.Set(headerWebhookTimestamp, ts)
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("status: want 401 got %d", rw.Code)
	}
}

func TestWebhook_OversizeBody_413(t *testing.T) {
	wh, _, _, _, clk, _, _, _, secret, _ := newTestWebhook(t)
	// 2 MiB body — over the 1 MiB default cap.
	big := bytes.Repeat([]byte{'x'}, 2<<20)
	ts := strconv.FormatInt(clk.Now().Unix(), 10)
	sig := SignWebhook(secret, ts, big)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/webhook", bytes.NewReader(big))
	req.Header.Set(headerWebhookSignature, sig)
	req.Header.Set(headerWebhookTimestamp, ts)
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status: want 413 got %d", rw.Code)
	}
}

func TestWebhook_MalformedJSON_400(t *testing.T) {
	wh, _, _, _, clk, _, _, _, secret, _ := newTestWebhook(t)
	body := []byte("not json")
	req := signedWebhookRequest(t, secret, body, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("status: want 400 got %d", rw.Code)
	}
}

func TestWebhook_UnknownAction_400(t *testing.T) {
	wh, _, _, _, clk, _, _, stored, secret, _ := newTestWebhook(t)
	body := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "merged_with_secret_flag",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, body, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("status: want 400 got %d", rw.Code)
	}
}

func TestWebhook_MissingProposalID_400(t *testing.T) {
	wh, _, _, _, clk, _, _, _, secret, _ := newTestWebhook(t)
	body := mustMarshal(t, webhookPayload{
		ProposalID: "",
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, body, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("status: want 400 got %d", rw.Code)
	}
}

func TestWebhook_NonUUIDProposalID_400(t *testing.T) {
	wh, _, _, _, clk, _, _, _, secret, _ := newTestWebhook(t)
	body := mustMarshal(t, webhookPayload{
		ProposalID: "not-a-uuid",
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, body, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("status: want 400 got %d", rw.Code)
	}
}

func TestWebhook_ProposalNotFound_404(t *testing.T) {
	wh, _, _, _, clk, _, _, _, secret, _ := newTestWebhook(t)
	body := mustMarshal(t, webhookPayload{
		ProposalID: mustNewUUIDv7().String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, body, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusNotFound {
		t.Errorf("status: want 404 got %d", rw.Code)
	}
}

func TestWebhook_SecretResolverError_500(t *testing.T) {
	pub := &fakePublisher{}
	store := newFakeProposalLookup()
	store.put(newTestProposal())
	clk := newFakeClock(time.Now())
	wh := NewWebhook(WebhookDeps{
		SecretResolver: constSecretResolver(nil, errors.New("vault offline")),
		Lookup:         store,
		Decisions:      newFakeDecisionRecorder(),
		Syncer:         &fakeSchedulerSyncer{},
		SourceResolver: constSourceResolver("platform", nil),
		Publisher:      pub,
		Clock:          clk,
		IDGenerator:    &fakeIDGenerator{},
	})

	body := []byte(`{"proposal_id":"x","action":"approved","timestamp":1}`)
	ts := strconv.FormatInt(clk.Now().Unix(), 10)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/webhook", bytes.NewReader(body))
	req.Header.Set(headerWebhookSignature, "sha256=ignored-because-secret-resolution-fails-first")
	req.Header.Set(headerWebhookTimestamp, ts)
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusInternalServerError {
		t.Errorf("status: want 500 got %d", rw.Code)
	}
}

func TestWebhook_EmptyResolvedSecret_500(t *testing.T) {
	pub := &fakePublisher{}
	store := newFakeProposalLookup()
	store.put(newTestProposal())
	clk := newFakeClock(time.Now())
	wh := NewWebhook(WebhookDeps{
		SecretResolver: constSecretResolver([]byte{}, nil),
		Lookup:         store,
		Decisions:      newFakeDecisionRecorder(),
		Syncer:         &fakeSchedulerSyncer{},
		SourceResolver: constSourceResolver("platform", nil),
		Publisher:      pub,
		Clock:          clk,
		IDGenerator:    &fakeIDGenerator{},
	})

	body := []byte(`{"proposal_id":"x","action":"approved","timestamp":1}`)
	ts := strconv.FormatInt(clk.Now().Unix(), 10)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/webhook", bytes.NewReader(body))
	req.Header.Set(headerWebhookSignature, "sha256=ignored")
	req.Header.Set(headerWebhookTimestamp, ts)
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusInternalServerError {
		t.Errorf("status: want 500 got %d", rw.Code)
	}
}

func TestWebhook_SourceMappingFailed_422(t *testing.T) {
	_, _, syncer, _, clk, _, _, stored, secret, _ := newTestWebhook(t)
	// Re-construct webhook with a failing source resolver.
	wh := NewWebhook(WebhookDeps{
		SecretResolver: constSecretResolver(secret, nil),
		Lookup:         &fakeProposalLookup{items: map[uuid.UUID]Proposal{stored.ID: stored}},
		Decisions:      newFakeDecisionRecorder(),
		Syncer:         syncer,
		SourceResolver: constSourceResolver("", errors.New("no source for target")),
		Publisher:      &fakePublisher{},
		Clock:          clk,
		IDGenerator:    &fakeIDGenerator{},
	})

	body := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, body, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnprocessableEntity {
		t.Errorf("status: want 422 got %d", rw.Code)
	}
}

func TestWebhook_EmptyResolvedSourceName_422(t *testing.T) {
	stored := newTestProposal()
	store := &fakeProposalLookup{items: map[uuid.UUID]Proposal{stored.ID: stored}}
	clk := newFakeClock(time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC))
	secret := []byte("s")
	wh := NewWebhook(WebhookDeps{
		SecretResolver: constSecretResolver(secret, nil),
		Lookup:         store,
		Decisions:      newFakeDecisionRecorder(),
		Syncer:         &fakeSchedulerSyncer{},
		SourceResolver: constSourceResolver("", nil), // no err but empty
		Publisher:      &fakePublisher{},
		Clock:          clk,
		IDGenerator:    &fakeIDGenerator{},
	})
	body := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, body, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnprocessableEntity {
		t.Errorf("status: want 422 got %d", rw.Code)
	}
}

func TestWebhook_PublishError_500(t *testing.T) {
	wh, pub, _, _, clk, _, _, stored, secret, _ := newTestWebhook(t)
	pub.err = errors.New("eventbus closed")

	body := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, body, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusInternalServerError {
		t.Errorf("status: want 500 got %d", rw.Code)
	}
}

func TestWebhook_LookupCtxCancelled_RequestTimeout(t *testing.T) {
	_, _, _, _, clk, _, _, stored, secret, _ := newTestWebhook(t)

	body := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, body, clk.Now())
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	// Re-construct with a Lookup that returns ctx.Err.
	wh := NewWebhook(WebhookDeps{
		SecretResolver: constSecretResolver(secret, nil),
		Lookup:         ctxErrLookup{},
		Decisions:      newFakeDecisionRecorder(),
		Syncer:         &fakeSchedulerSyncer{},
		SourceResolver: constSourceResolver("platform", nil),
		Publisher:      &fakePublisher{},
		Clock:          clk,
		IDGenerator:    &fakeIDGenerator{},
	})
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusRequestTimeout {
		t.Errorf("status: want 408 got %d", rw.Code)
	}
}

type ctxErrLookup struct{}

func (ctxErrLookup) Lookup(_ context.Context, _ uuid.UUID) (Proposal, error) {
	return Proposal{}, context.Canceled
}

func TestWebhook_Concurrency_16DistinctProposals(t *testing.T) {
	wh, pub, _, store, clk, _, _, _, secret, _ := newTestWebhook(t)

	// Seed 16 distinct proposals so each concurrent delivery has a
	// unique decision claim. Same-id concurrency is exercised by
	// TestWebhook_Concurrency_SameProposal_OneWinner below.
	const n = 16
	proposals := make([]Proposal, n)
	for i := 0; i < n; i++ {
		proposals[i] = newTestProposal()
		store.put(proposals[i])
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			body := mustMarshal(t, webhookPayload{
				ProposalID: proposals[i].ID.String(),
				Action:     "approved",
				Timestamp:  clk.Now().Unix(),
			})
			req := signedWebhookRequest(t, secret, body, clk.Now())
			rw := httptest.NewRecorder()
			wh.ServeHTTP(rw, req)
			if rw.Code != http.StatusOK {
				t.Errorf("status: want 200 got %d", rw.Code)
			}
		}()
	}
	wg.Wait()
	if len(pub.eventsForTopic(TopicToolApproved)) != n {
		t.Errorf("expected %d publishes for %d distinct proposals, got %d", n, n, len(pub.eventsForTopic(TopicToolApproved)))
	}
}

// TestWebhook_Concurrency_SameProposal_OneWinner pins the M9.4.b
// iter-1 M2 finding: 16 concurrent deliveries of the SAME proposal
// (e.g. GitHub-side retry storm against a flaky downstream) must
// elect exactly one publisher; the rest silent-200 via the decision
// claim.
func TestWebhook_Concurrency_SameProposal_OneWinner(t *testing.T) {
	wh, pub, _, _, clk, _, _, stored, secret, _ := newTestWebhook(t)

	body := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			req := signedWebhookRequest(t, secret, body, clk.Now())
			rw := httptest.NewRecorder()
			wh.ServeHTTP(rw, req)
			if rw.Code != http.StatusOK {
				t.Errorf("status: want 200 got %d", rw.Code)
			}
		}()
	}
	wg.Wait()
	if got := len(pub.eventsForTopic(TopicToolApproved)); got != 1 {
		t.Errorf("expected exactly 1 publish under same-proposal concurrency, got %d", got)
	}
}

func TestWebhook_PIICanary_LoggerRedaction(t *testing.T) {
	_, _, _, _, clk, _, logger, stored, secret, _ := newTestWebhook(t)
	// Inject canary substrings into the proposal's PII-bearing fields.
	const canaryPurpose = "CANARY_PURPOSE_PII_zzzzzz"
	const canaryDesc = "CANARY_DESC_PII_zzzzzz"
	const canaryCode = "CANARY_CODE_PII_zzzzzz"
	stored.Input.Purpose = canaryPurpose
	stored.Input.PlainLanguageDescription = canaryDesc
	stored.Input.CodeDraft = canaryCode
	// Re-seed the store with the canary-laden proposal.
	store := newFakeProposalLookup()
	store.put(stored)
	wh := NewWebhook(WebhookDeps{
		SecretResolver: constSecretResolver(secret, nil),
		Lookup:         store,
		Decisions:      newFakeDecisionRecorder(),
		Syncer:         &fakeSchedulerSyncer{err: errors.New("force sync error to drive log path")},
		SourceResolver: constSourceResolver("platform", nil),
		Publisher:      &fakePublisher{},
		Clock:          clk,
		IDGenerator:    &fakeIDGenerator{},
		Logger:         logger,
	})

	body := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, body, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)

	// Sanity: at least one log entry MUST carry a non-empty kv pair.
	// Without this guard the PII-canary assertion below would
	// trivially pass against a logger that never received kv (the
	// M9.4.b iter-1 review flagged this exact regression in the
	// prior shape of this test).
	sawKv := false
	for _, e := range logger.snapshot() {
		if len(e.kv) > 0 {
			sawKv = true
			break
		}
	}
	if !sawKv {
		t.Fatalf("PII canary trivially passes: no log entry carried kv (logErr plumbing regressed)")
	}

	// Assert no logger entry — message OR kv pairs — mentions the
	// canary substrings.
	for _, e := range logger.snapshot() {
		joined := e.msg
		for _, v := range e.kv {
			joined += "|" + asString(v)
		}
		for _, canary := range []string{canaryPurpose, canaryDesc, canaryCode} {
			if strings.Contains(joined, canary) {
				t.Errorf("logger entry leaked canary %q: %s", canary, joined)
			}
		}
	}
}

func TestWebhook_ZeroUUIDProposalID_400(t *testing.T) {
	wh, _, _, _, clk, _, _, _, secret, _ := newTestWebhook(t)
	body := mustMarshal(t, webhookPayload{
		ProposalID: uuid.Nil.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, body, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("status: want 400 (zero uuid is malformed payload, not lookup miss) got %d", rw.Code)
	}
}

func TestWebhook_UnknownField_400(t *testing.T) {
	wh, _, _, _, clk, _, _, _, secret, _ := newTestWebhook(t)
	body := []byte(`{"proposal_id":"` + mustNewUUIDv7().String() +
		`","action":"approved","timestamp":` + strconv.FormatInt(clk.Now().Unix(), 10) +
		`,"hostile_extra_field":"sneaky"}`)
	req := signedWebhookRequest(t, secret, body, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("status: want 400 (unknown field must be rejected via DisallowUnknownFields) got %d", rw.Code)
	}
}

func TestWebhook_DuplicateApprovedDelivery_Idempotent(t *testing.T) {
	wh, pub, _, _, clk, _, _, stored, secret, decisions := newTestWebhook(t)

	payloadBytes := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})

	// First delivery — claims the decision and publishes.
	req1 := signedWebhookRequest(t, secret, payloadBytes, clk.Now())
	rw1 := httptest.NewRecorder()
	wh.ServeHTTP(rw1, req1)
	if rw1.Code != http.StatusOK {
		t.Fatalf("first delivery: want 200 got %d", rw1.Code)
	}

	// Second delivery (replay, e.g. GitHub retry on transient 5xx).
	req2 := signedWebhookRequest(t, secret, payloadBytes, clk.Now())
	rw2 := httptest.NewRecorder()
	wh.ServeHTTP(rw2, req2)
	if rw2.Code != http.StatusOK {
		t.Errorf("replay: want 200 (idempotent silent success) got %d", rw2.Code)
	}

	if got := len(pub.eventsForTopic(TopicToolApproved)); got != 1 {
		t.Errorf("duplicate delivery must publish at most once, got %d", got)
	}
	if got := decisions.markCount(); got != 2 {
		t.Errorf("MarkDecided expected 2 calls (1 first-time + 1 replay), got %d", got)
	}
}

func TestWebhook_DuplicateRejectedDelivery_Idempotent(t *testing.T) {
	wh, pub, _, _, clk, _, _, stored, secret, _ := newTestWebhook(t)

	payloadBytes := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "rejected",
		Timestamp:  clk.Now().Unix(),
	})

	for i := 0; i < 3; i++ {
		req := signedWebhookRequest(t, secret, payloadBytes, clk.Now())
		rw := httptest.NewRecorder()
		wh.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("delivery #%d: want 200 got %d", i+1, rw.Code)
		}
	}
	if got := len(pub.eventsForTopic(TopicToolRejected)); got != 1 {
		t.Errorf("3 deliveries must publish 1 event, got %d", got)
	}
}

func TestWebhook_ApprovedThenRejected_Conflict_409(t *testing.T) {
	wh, _, _, _, clk, _, _, stored, secret, _ := newTestWebhook(t)

	approveBody := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	rejectBody := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "rejected",
		Timestamp:  clk.Now().Unix(),
	})

	req1 := signedWebhookRequest(t, secret, approveBody, clk.Now())
	rw1 := httptest.NewRecorder()
	wh.ServeHTTP(rw1, req1)
	if rw1.Code != http.StatusOK {
		t.Fatalf("first approve: want 200 got %d", rw1.Code)
	}

	req2 := signedWebhookRequest(t, secret, rejectBody, clk.Now())
	rw2 := httptest.NewRecorder()
	wh.ServeHTTP(rw2, req2)
	if rw2.Code != http.StatusConflict {
		t.Errorf("reject after approve: want 409 got %d", rw2.Code)
	}
}

func TestWebhook_PublishError_RollsBackDecision(t *testing.T) {
	wh, pub, _, _, clk, _, _, stored, secret, decisions := newTestWebhook(t)
	pub.err = errors.New("eventbus closed")

	body := mustMarshal(t, webhookPayload{
		ProposalID: stored.ID.String(),
		Action:     "approved",
		Timestamp:  clk.Now().Unix(),
	})
	req := signedWebhookRequest(t, secret, body, clk.Now())
	rw := httptest.NewRecorder()
	wh.ServeHTTP(rw, req)
	if rw.Code != http.StatusInternalServerError {
		t.Fatalf("publish error: want 500 got %d", rw.Code)
	}
	if got := decisions.unmarkCount(); got != 1 {
		t.Errorf("publish error must trigger Unmark rollback (so retry can re-publish); got unmark count %d", got)
	}
	publishCallsAfterFirst := len(pub.eventsForTopic(TopicToolApproved))

	// Retry with publish succeeding — the second delivery must
	// observe the cleared decision and re-publish (i.e. NOT silent-200).
	pub.err = nil
	req2 := signedWebhookRequest(t, secret, body, clk.Now())
	rw2 := httptest.NewRecorder()
	wh.ServeHTTP(rw2, req2)
	if rw2.Code != http.StatusOK {
		t.Fatalf("retry after rollback: want 200 got %d", rw2.Code)
	}
	publishCallsAfterRetry := len(pub.eventsForTopic(TopicToolApproved))
	if publishCallsAfterRetry <= publishCallsAfterFirst {
		t.Errorf("retry must re-invoke Publish after rollback; before=%d after=%d", publishCallsAfterFirst, publishCallsAfterRetry)
	}
}

func TestWebhook_HMACEqualPinned(t *testing.T) {
	// AC: verify the signature compare uses crypto/hmac.Equal, not
	// bytes.Equal or `==`. Same regression guard as the slack/inbound
	// equivalent.
	src, err := os.ReadFile("webhook.go")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	srcStr := string(src)
	if !strings.Contains(srcStr, "hmac.Equal(") {
		t.Errorf("webhook.go must use hmac.Equal for signature comparison")
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func asString(v any) string {
	return fmt.Sprintf("%+v", v)
}
