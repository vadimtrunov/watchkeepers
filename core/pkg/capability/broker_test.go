package capability

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is the deterministic clock injected via [WithClock] so the
// expiry-edge tests avoid `time.Sleep`. The zero-value epoch is fixed
// to 2026-01-01 UTC so test traces print stable timestamps.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// Now returns the clock's current reading. Goroutine-safe.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by `d`. Goroutine-safe.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set overrides the clock to `t`. Goroutine-safe.
func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// fakeLogEntry records one call to [recordingLogger.Log].
type fakeLogEntry struct {
	Msg string
	KV  []any
}

// recordingLogger is the hand-rolled [Logger] stand-in used by the
// capability test suite. Mirrors the secrets/cron mutex-guarded entries
// pattern documented in M3.4.a/M3.4.b LESSONS — no mocking library,
// fmt.Sprintf-grep-able for redaction assertions.
type recordingLogger struct {
	mu      sync.Mutex
	entries []fakeLogEntry
}

func (l *recordingLogger) Log(_ context.Context, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]any, len(kv))
	copy(cp, kv)
	l.entries = append(l.entries, fakeLogEntry{Msg: msg, KV: cp})
}

// allEntries returns a defensive copy of all recorded entries.
func (l *recordingLogger) allEntries() []fakeLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]fakeLogEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// containsString reports whether any log entry contains needle as a
// substring, checking the serialized fmt.Sprintf("%+v", e) form. Used
// to assert redaction: no log payload must contain the full token.
//
// Defense-in-depth: the entire entry is serialized so that future log
// calls passing the value as a non-string type (`[]byte`, `error`,
// concrete struct, etc.) are caught regardless of kv-value type.
func containsString(entries []fakeLogEntry, needle string) bool {
	for _, e := range entries {
		if strings.Contains(fmt.Sprintf("%+v", e), needle) {
			return true
		}
	}
	return false
}

// pollUntil is the polling-deadline helper documented in
// `docs/LESSONS.md` (M2b.5). Polls `cond` every 10ms until either the
// condition returns true or `deadline` elapses; on timeout calls
// t.Fatalf with `desc`.
func pollUntil(t *testing.T, deadline time.Duration, desc string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("polling deadline (%s) elapsed without %s", deadline, desc)
}

// tokenCount returns the current size of the broker's internal map
// under the read lock. Used by tests that assert lazy-cleanup or
// reaper effects on the underlying store without going through
// Validate.
func tokenCount(b *Broker) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.tokens)
}

// =====================================================================
// Happy path
// =====================================================================

// TestBroker_IssueReturnsUniqueTokens — issue 100 tokens; every result
// is a 43-character base64-URL string and all 100 are unique.
func TestBroker_IssueReturnsUniqueTokens(t *testing.T) {
	t.Parallel()
	b := New()
	t.Cleanup(func() { _ = b.Close() })

	const n = 100
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		tok, err := b.Issue("keep:write", time.Hour)
		if err != nil {
			t.Fatalf("Issue %d: %v", i, err)
		}
		// 32 random bytes → base64.RawURLEncoding → 43 chars (no
		// padding). Reject anything else so a future encoding switch
		// surfaces here.
		if len(tok) != 43 {
			t.Fatalf("token %d length = %d, want 43 (got %q)", i, len(tok), tok)
		}
		// URL-safe base64 alphabet contains only A-Z, a-z, 0-9, '-', '_'.
		for _, r := range tok {
			ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_'
			if !ok {
				t.Fatalf("token %d contains non-URL-safe rune %q in %q", i, r, tok)
			}
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("token %d is a duplicate: %q", i, tok)
		}
		seen[tok] = struct{}{}
	}
}

// TestBroker_ValidateAcceptsCurrentToken — issue with scope keep:write
// ttl 1h; Validate returns nil.
func TestBroker_ValidateAcceptsCurrentToken(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	b := New(WithClock(clk.Now))
	t.Cleanup(func() { _ = b.Close() })

	tok, err := b.Issue("keep:write", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := b.Validate(context.Background(), tok, "keep:write"); err != nil {
		t.Fatalf("Validate: %v, want nil", err)
	}
}

// TestBroker_ValidateRejectsScopeMismatch — issue scope keep:write;
// Validate with keep:read returns ErrScopeMismatch.
func TestBroker_ValidateRejectsScopeMismatch(t *testing.T) {
	t.Parallel()
	b := New()
	t.Cleanup(func() { _ = b.Close() })

	tok, err := b.Issue("keep:write", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	err = b.Validate(context.Background(), tok, "keep:read")
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("Validate err = %v, want errors.Is ErrScopeMismatch", err)
	}
}

// =====================================================================
// Expiry
// =====================================================================

// TestBroker_ValidateRejectsExpiredToken_LazyCleanup — fakeClock; issue
// with ttl=1m; advance clock by 2m; Validate returns ErrTokenExpired
// AND the entry is removed from the internal map (lazy cleanup).
func TestBroker_ValidateRejectsExpiredToken_LazyCleanup(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	b := New(WithClock(clk.Now))
	t.Cleanup(func() { _ = b.Close() })

	tok, err := b.Issue("keep:write", time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if got := tokenCount(b); got != 1 {
		t.Fatalf("after Issue tokenCount = %d, want 1", got)
	}

	clk.Advance(2 * time.Minute)

	err = b.Validate(context.Background(), tok, "keep:write")
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("Validate err = %v, want errors.Is ErrTokenExpired", err)
	}
	if got := tokenCount(b); got != 0 {
		t.Fatalf("after expired Validate tokenCount = %d, want 0 (lazy cleanup)", got)
	}

	// Subsequent Validate of the same token now returns ErrInvalidToken
	// (the entry was pruned).
	err = b.Validate(context.Background(), tok, "keep:write")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("second Validate err = %v, want errors.Is ErrInvalidToken", err)
	}
}

// TestBroker_ValidateAtExactExpiryRejects — clock at exactly the
// expiry time → ErrTokenExpired (boundary inclusive).
func TestBroker_ValidateAtExactExpiryRejects(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	b := New(WithClock(clk.Now))
	t.Cleanup(func() { _ = b.Close() })

	const ttl = time.Minute
	tok, err := b.Issue("keep:write", ttl)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	clk.Advance(ttl)

	err = b.Validate(context.Background(), tok, "keep:write")
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("Validate at exact expiry err = %v, want errors.Is ErrTokenExpired", err)
	}
}

// TestBroker_ReaperPrunesExpiredEntries — WithReaperInterval; issue 5
// tokens with a short ttl; advance the fakeClock past expiry; poll
// until the broker's internal map is empty without calling Validate
// (proves the reaper, not lazy validation, did the cleanup).
func TestBroker_ReaperPrunesExpiredEntries(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	b := New(WithClock(clk.Now), WithReaperInterval(20*time.Millisecond))
	t.Cleanup(func() { _ = b.Close() })

	for i := 0; i < 5; i++ {
		if _, err := b.Issue("keep:write", 10*time.Millisecond); err != nil {
			t.Fatalf("Issue %d: %v", i, err)
		}
	}
	if got := tokenCount(b); got != 5 {
		t.Fatalf("after Issue tokenCount = %d, want 5", got)
	}

	clk.Advance(time.Second) // far past every entry's expiry

	pollUntil(t, time.Second, "reaper to drain the map", func() bool {
		return tokenCount(b) == 0
	})
}

// =====================================================================
// Revoke
// =====================================================================

// TestBroker_RevokeRemovesToken — issue, revoke, Validate returns
// ErrInvalidToken.
func TestBroker_RevokeRemovesToken(t *testing.T) {
	t.Parallel()
	b := New()
	t.Cleanup(func() { _ = b.Close() })

	tok, err := b.Issue("keep:write", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := b.Revoke(tok); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	err = b.Validate(context.Background(), tok, "keep:write")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Validate after Revoke err = %v, want errors.Is ErrInvalidToken", err)
	}
}

// TestBroker_RevokeIdempotent — revoke twice; second call returns nil.
func TestBroker_RevokeIdempotent(t *testing.T) {
	t.Parallel()
	b := New()
	t.Cleanup(func() { _ = b.Close() })

	tok, err := b.Issue("keep:write", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := b.Revoke(tok); err != nil {
		t.Fatalf("first Revoke: %v", err)
	}
	if err := b.Revoke(tok); err != nil {
		t.Fatalf("second Revoke: %v", err)
	}
	// Revoking a never-issued token is also fine.
	if err := b.Revoke("never-issued-token-bytes"); err != nil {
		t.Fatalf("Revoke unknown token: %v, want nil", err)
	}
}

// =====================================================================
// Lifecycle
// =====================================================================

// TestBroker_CloseStopsReaper_NoGoroutineLeak — start with reaper;
// Close; assert post-Close goroutine count returns to baseline + slack
// within polling deadline.
func TestBroker_CloseStopsReaper_NoGoroutineLeak(t *testing.T) {
	// Not parallel: NumGoroutine is process-wide.
	baseline := runtime.NumGoroutine()

	b := New(WithReaperInterval(10 * time.Millisecond))
	if got := runtime.NumGoroutine(); got <= baseline {
		t.Fatalf("after New with reaper, goroutines = %d, want > baseline %d", got, baseline)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pollUntil(t, 2*time.Second, "goroutine count to return to baseline (±2 slack)", func() bool {
		return runtime.NumGoroutine() <= baseline+2
	})
}

// TestBroker_OperationsAfterClose_ErrClosed — Close; Issue, Validate,
// Revoke each return errors.Is(err, ErrClosed).
func TestBroker_OperationsAfterClose_ErrClosed(t *testing.T) {
	t.Parallel()
	b := New()
	tok, err := b.Issue("keep:write", time.Hour)
	if err != nil {
		t.Fatalf("Issue before Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := b.Issue("keep:write", time.Hour); !errors.Is(err, ErrClosed) {
		t.Fatalf("Issue after Close err = %v, want errors.Is ErrClosed", err)
	}
	if err := b.Validate(context.Background(), tok, "keep:write"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Validate after Close err = %v, want errors.Is ErrClosed", err)
	}
	if err := b.Revoke(tok); !errors.Is(err, ErrClosed) {
		t.Fatalf("Revoke after Close err = %v, want errors.Is ErrClosed", err)
	}
}

// TestBroker_CloseIdempotent — Close twice; second returns nil.
func TestBroker_CloseIdempotent(t *testing.T) {
	t.Parallel()
	b := New(WithReaperInterval(10 * time.Millisecond))
	if err := b.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v, want nil (idempotent)", err)
	}
}

// =====================================================================
// Negative
// =====================================================================

// TestBroker_IssueEmptyScope_ErrInvalidScope — synchronous validation;
// no map mutation.
func TestBroker_IssueEmptyScope_ErrInvalidScope(t *testing.T) {
	t.Parallel()
	b := New()
	t.Cleanup(func() { _ = b.Close() })

	_, err := b.Issue("", time.Hour)
	if !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("Issue empty scope err = %v, want errors.Is ErrInvalidScope", err)
	}
	if got := tokenCount(b); got != 0 {
		t.Fatalf("tokenCount after rejected Issue = %d, want 0", got)
	}
}

// TestBroker_IssueNonPositiveTTL_ErrInvalidTTL — ttl=0 and ttl=-1m
// both rejected.
func TestBroker_IssueNonPositiveTTL_ErrInvalidTTL(t *testing.T) {
	t.Parallel()
	b := New()
	t.Cleanup(func() { _ = b.Close() })

	for _, ttl := range []time.Duration{0, -time.Minute, -1} {
		_, err := b.Issue("keep:write", ttl)
		if !errors.Is(err, ErrInvalidTTL) {
			t.Fatalf("Issue ttl=%v err = %v, want errors.Is ErrInvalidTTL", ttl, err)
		}
	}
	if got := tokenCount(b); got != 0 {
		t.Fatalf("tokenCount after rejected Issues = %d, want 0", got)
	}
}

// TestBroker_ValidateUnknownToken_ErrInvalidToken — random base64
// string that the broker never issued returns ErrInvalidToken.
func TestBroker_ValidateUnknownToken_ErrInvalidToken(t *testing.T) {
	t.Parallel()
	b := New()
	t.Cleanup(func() { _ = b.Close() })

	// 43-char URL-safe base64 the broker definitely never minted.
	bogus := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	err := b.Validate(context.Background(), bogus, "keep:write")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Validate unknown token err = %v, want errors.Is ErrInvalidToken", err)
	}
}

// TestBroker_ValidateCancelledCtx — pre-cancelled ctx returns
// ctx.Err(); no map mutation.
func TestBroker_ValidateCancelledCtx(t *testing.T) {
	t.Parallel()
	b := New()
	t.Cleanup(func() { _ = b.Close() })

	tok, err := b.Issue("keep:write", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	before := tokenCount(b)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = b.Validate(ctx, tok, "keep:write")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Validate cancelled ctx err = %v, want errors.Is context.Canceled", err)
	}
	if after := tokenCount(b); after != before {
		t.Fatalf("tokenCount before/after cancelled Validate = %d/%d, want stable", before, after)
	}
}

// =====================================================================
// Redaction
// =====================================================================

// TestBroker_LoggerNeverSeesFullToken — wire recordingLogger; issue +
// validate (success, scope mismatch, expired) + revoke; serialize all
// log entries via fmt.Sprintf("%+v", entry); assert the full token
// NEVER appears anywhere. Only the 8-char prefix may appear.
func TestBroker_LoggerNeverSeesFullToken(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	logger := &recordingLogger{}
	b := New(
		WithClock(clk.Now),
		WithLogger(logger),
		WithReaperInterval(5*time.Millisecond),
	)

	// Issue → success log
	tok, err := b.Issue("keep:write", time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Validate match → success log
	if err := b.Validate(context.Background(), tok, "keep:write"); err != nil {
		t.Fatalf("Validate match: %v", err)
	}
	// Validate scope mismatch → mismatch log
	if err := b.Validate(context.Background(), tok, "keep:read"); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("Validate scope-mismatch err = %v", err)
	}
	// Revoke → revoked log
	if err := b.Revoke(tok); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// Issue another token, expire it via the clock, validate-to-expire.
	tok2, err := b.Issue("keep:write", time.Minute)
	if err != nil {
		t.Fatalf("Issue 2: %v", err)
	}
	clk.Advance(2 * time.Minute)
	if err := b.Validate(context.Background(), tok2, "keep:write"); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("Validate expired err = %v", err)
	}

	// Issue a third token, let the reaper clean it.
	tok3, err := b.Issue("keep:write", time.Minute)
	if err != nil {
		t.Fatalf("Issue 3: %v", err)
	}
	clk.Advance(2 * time.Minute)
	pollUntil(t, time.Second, "reaper to prune tok3", func() bool {
		return tokenCount(b) == 0
	})

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entries := logger.allEntries()
	if len(entries) == 0 {
		t.Fatalf("recordingLogger captured 0 entries; expected ≥1 issue+validate log")
	}

	for _, full := range []string{tok, tok2, tok3} {
		if containsString(entries, full) {
			t.Fatalf("FULL token %q appeared in log entries; redaction discipline broken", full)
		}
	}

	// Sanity: the 8-char prefix DOES appear at least once (otherwise
	// the test would pass for the wrong reason — e.g. logger never
	// fired).
	for _, full := range []string{tok, tok2, tok3} {
		if !containsString(entries, full[:tokenPrefixLen]) {
			t.Fatalf("token prefix %q missing from log entries; logger may not be wired",
				full[:tokenPrefixLen])
		}
	}
}

// TestBroker_TokenNotInErrorMessages — trigger ErrInvalidToken,
// ErrTokenExpired, ErrScopeMismatch; assert err.Error() does NOT
// contain the input token bytes.
func TestBroker_TokenNotInErrorMessages(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	b := New(WithClock(clk.Now))
	t.Cleanup(func() { _ = b.Close() })

	// ErrInvalidToken
	bogus := "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	err := b.Validate(context.Background(), bogus, "keep:write")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("setup: want ErrInvalidToken, got %v", err)
	}
	if strings.Contains(err.Error(), bogus) {
		t.Fatalf("ErrInvalidToken err.Error() = %q contains input token bytes", err.Error())
	}

	// ErrTokenExpired
	tok, err := b.Issue("keep:write", time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	clk.Advance(2 * time.Minute)
	err = b.Validate(context.Background(), tok, "keep:write")
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("setup: want ErrTokenExpired, got %v", err)
	}
	if strings.Contains(err.Error(), tok) {
		t.Fatalf("ErrTokenExpired err.Error() = %q contains input token bytes", err.Error())
	}

	// ErrScopeMismatch
	tok2, err := b.Issue("keep:write", time.Hour)
	if err != nil {
		t.Fatalf("Issue 2: %v", err)
	}
	err = b.Validate(context.Background(), tok2, "keep:read")
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("setup: want ErrScopeMismatch, got %v", err)
	}
	if strings.Contains(err.Error(), tok2) {
		t.Fatalf("ErrScopeMismatch err.Error() = %q contains input token bytes", err.Error())
	}
}

// =====================================================================
// Concurrency
// =====================================================================

// TestBroker_ConcurrentIssueAndValidate — 50 goroutines mix Issue +
// Validate calls; under -race the detector must stay quiet.
func TestBroker_ConcurrentIssueAndValidate(t *testing.T) {
	t.Parallel()
	b := New()
	t.Cleanup(func() { _ = b.Close() })

	const goroutines = 50
	const itersPerG = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)

	tokens := make(chan string, goroutines*itersPerG)
	var validateErrs atomic.Int64

	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < itersPerG; i++ {
				tok, err := b.Issue("keep:write", time.Hour)
				if err != nil {
					t.Errorf("Issue: %v", err)
					return
				}
				tokens <- tok
				if err := b.Validate(context.Background(), tok, "keep:write"); err != nil {
					validateErrs.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	close(tokens)

	if got := validateErrs.Load(); got != 0 {
		t.Fatalf("validate errors during concurrent run = %d, want 0", got)
	}

	// Sanity: every issued token should still be Validate-able under a
	// fresh ctx (the broker was not closed).
	count := 0
	for tok := range tokens {
		if err := b.Validate(context.Background(), tok, "keep:write"); err != nil {
			t.Fatalf("post-run Validate: %v", err)
		}
		count++
	}
	if want := goroutines * itersPerG; count != want {
		t.Fatalf("issued tokens = %d, want %d", count, want)
	}
}
