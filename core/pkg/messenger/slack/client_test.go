package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedLogEntry is one captured log call from [recordingLogger].
// The Client's redaction discipline guarantees no bearer token /
// request body / response body lands here, so the test can assert
// metadata directly.
type recordedLogEntry struct {
	Msg string
	KV  []any
}

// recordingLogger is a hand-rolled [Logger] stand-in used by the
// client tests. Mirrors the keeperslog / outbox fakeLogger pattern —
// no mocking library.
type recordingLogger struct {
	mu      sync.Mutex
	entries []recordedLogEntry
}

func (l *recordingLogger) Log(_ context.Context, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]any, len(kv))
	copy(cp, kv)
	l.entries = append(l.entries, recordedLogEntry{Msg: msg, KV: cp})
}

func (l *recordingLogger) snapshot() []recordedLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]recordedLogEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// containsKV asserts that the recorded KV list carries the given
// (key, value) pair. Returns the value found at the key, or nil if
// absent. Tests use this to assert the structured-logging contract.
func containsKV(kv []any, key string) (any, bool) {
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok && k == key {
			return kv[i+1], true
		}
	}
	return nil, false
}

// errTokenSource is a TokenSource that always errors. Used to test
// the "do NOT send a request when the token cannot be resolved"
// security invariant.
type errTokenSource struct{ err error }

func (e errTokenSource) Token(context.Context) (string, error) { return "", e.err }

// TestClient_Do_HappyPath asserts a 2xx `{"ok": true, ...}` response
// decodes into `out` and the Authorization + Content-Type headers
// land on the wire as expected.
func TestClient_Do_HappyPath(t *testing.T) {
	t.Parallel()

	type response struct {
		OK      bool   `json:"ok"`
		Channel string `json:"channel"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Method must be /chat.postMessage on POST.
		if r.URL.Path != "/chat.postMessage" {
			t.Errorf("path = %q, want /chat.postMessage", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		// Bearer auth header must be present.
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		// Content type must announce JSON.
		if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Errorf("Content-Type = %q, want application/json prefix", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"channel":"C1"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("test-token")),
	)
	var out response
	if err := c.Do(context.Background(), "chat.postMessage", map[string]string{"text": "hi"}, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !out.OK || out.Channel != "C1" {
		t.Errorf("response = %+v, want OK + Channel=C1", out)
	}
}

// TestClient_Do_OkFalse_MapsToSentinel asserts a 2xx response with
// `{"ok": false, "error": "<code>"}` surfaces as *APIError whose
// Unwrap matches the documented sentinel.
func TestClient_Do_OkFalse_MapsToSentinel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code string
		want error
	}{
		{"channel_not_found", ErrChannelNotFound},
		{"user_not_found", ErrUserNotFound},
		{"app_not_found", ErrAppNotFound},
		{"invalid_auth", ErrInvalidAuth},
		{"not_authed", ErrInvalidAuth},
		{"token_expired", ErrTokenExpired},
		{"ratelimited", ErrRateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintf(w, `{"ok":false,"error":%q}`, tc.code)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(
				WithBaseURL(srv.URL),
				WithTokenSource(StaticToken("test-token")),
			)
			err := c.Do(context.Background(), "chat.postMessage", nil, nil)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error is not *APIError: %T %v", err, err)
			}
			if apiErr.Code != tc.code {
				t.Errorf("Code = %q, want %q", apiErr.Code, tc.code)
			}
			if apiErr.Method != "chat.postMessage" {
				t.Errorf("Method = %q, want chat.postMessage", apiErr.Method)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("errors.Is(err, %v) = false, want true", tc.want)
			}
		})
	}
}

// TestClient_Do_429_PropagatesAsErrRateLimited asserts the documented
// HTTP 429 contract: parse Retry-After, return *APIError wrapping
// ErrRateLimited, do NOT auto-retry.
func TestClient_Do_429_PropagatesAsErrRateLimited(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	err := c.Do(context.Background(), "chat.postMessage", nil, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("errors.Is(err, ErrRateLimited) = false, want true; got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %T %v", err, err)
	}
	if apiErr.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want 429", apiErr.Status)
	}
	if apiErr.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", apiErr.RetryAfter)
	}
	// Documented contract: NO auto-retry.
	if calls != 1 {
		t.Errorf("server saw %d calls, want exactly 1 (no auto-retry)", calls)
	}
}

// TestClient_Do_429_HTTPDate_Retry asserts Retry-After in HTTP-date
// form decodes correctly relative to the configured clock.
func TestClient_Do_429_HTTPDate_Retry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	retryAt := now.Add(45 * time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", retryAt.UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
		WithClock(func() time.Time { return now }),
	)
	err := c.Do(context.Background(), "chat.postMessage", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %T %v", err, err)
	}
	// HTTP-date precision is integer seconds; allow ±1s slop.
	if d := apiErr.RetryAfter; d < 44*time.Second || d > 46*time.Second {
		t.Errorf("RetryAfter = %v, want ~45s", d)
	}
}

// TestClient_Do_NetworkError_Wrapped asserts a transport-level
// failure (server unreachable) is wrapped with "slack:" prefix and
// is NOT a *APIError.
func TestClient_Do_NetworkError_Wrapped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient(
		WithBaseURL(url),
		WithTokenSource(StaticToken("t")),
	)
	err := c.Do(context.Background(), "chat.postMessage", nil, nil)
	if err == nil {
		t.Fatal("expected error against closed server, got nil")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("transport error must not be a *APIError; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "slack:") {
		t.Errorf("err = %q, want slack: prefix", err.Error())
	}
}

// TestClient_Do_CtxCancellation asserts a pre-cancelled ctx returns
// ctx.Err() without sending a network request.
func TestClient_Do_CtxCancellation(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Do(ctx, "chat.postMessage", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; got %v", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

// TestClient_Do_NoTokenSource_FailsSync asserts the security
// invariant: no [TokenSource] => no network round-trip.
func TestClient_Do_NoTokenSource_FailsSync(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	err := c.Do(context.Background(), "chat.postMessage", nil, nil)
	if !errors.Is(err, ErrInvalidAuth) {
		t.Errorf("errors.Is(err, ErrInvalidAuth) = false, want true; got %v", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (no token => no request)", calls)
	}
}

// TestClient_Do_TokenSourceError_Wrapped asserts a TokenSource
// failure surfaces wrapped with the slack: prefix and the request is
// NOT sent (security invariant: never emit a request with a stale
// token).
func TestClient_Do_TokenSourceError_Wrapped(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	bootErr := errors.New("secrets store offline")
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(errTokenSource{err: bootErr}),
	)
	err := c.Do(context.Background(), "chat.postMessage", nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, bootErr) {
		t.Errorf("errors.Is(err, bootErr) = false, want true; got %v", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (token error => no request)", calls)
	}
}

// TestClient_Do_EmptyMethod_FailsSync asserts an empty method name
// surfaces ErrUnknownMethod synchronously.
func TestClient_Do_EmptyMethod_FailsSync(t *testing.T) {
	t.Parallel()

	c := NewClient(
		WithBaseURL("http://example.invalid"),
		WithTokenSource(StaticToken("t")),
	)
	err := c.Do(context.Background(), "", nil, nil)
	if !errors.Is(err, ErrUnknownMethod) {
		t.Errorf("errors.Is(err, ErrUnknownMethod) = false, want true; got %v", err)
	}
}

// TestClient_Do_LoggerRedacted asserts the redaction discipline:
// the bearer token, request body, and response body NEVER appear in
// the logger entries; method + status DO appear.
func TestClient_Do_LoggerRedacted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"secret":"sk-LEAKED-PAYLOAD"}`)
	}))
	t.Cleanup(srv.Close)

	logger := &recordingLogger{}
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-LEAKED-TOKEN")),
		WithLogger(logger),
	)
	if err := c.Do(context.Background(), "chat.postMessage", map[string]string{"text": "secret-body"}, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}

	entries := logger.snapshot()
	if len(entries) == 0 {
		t.Fatal("expected at least one log entry, got none")
	}

	// Audit every entry: no entry's Msg or KV may contain the
	// banned strings.
	banned := []string{"xoxb-LEAKED-TOKEN", "sk-LEAKED-PAYLOAD", "secret-body", "Bearer "}
	for _, e := range entries {
		entryStr := fmt.Sprintf("msg=%q kv=%v", e.Msg, e.KV)
		for _, b := range banned {
			if strings.Contains(entryStr, b) {
				t.Errorf("log entry %q leaks banned substring %q", entryStr, b)
			}
		}
	}

	// At least one entry must carry method + status — the structured
	// logging contract.
	var sawBegin, sawOK bool
	for _, e := range entries {
		if e.Msg == "slack: request begin" {
			if got, ok := containsKV(e.KV, "method"); ok && got == "chat.postMessage" {
				sawBegin = true
			}
		}
		if e.Msg == "slack: request ok" {
			if got, ok := containsKV(e.KV, "status"); ok && got == http.StatusOK {
				sawOK = true
			}
		}
	}
	if !sawBegin {
		t.Error("missing 'slack: request begin' entry with method=chat.postMessage")
	}
	if !sawOK {
		t.Error("missing 'slack: request ok' entry with status=200")
	}
}

// TestClient_Do_RateLimiterWired asserts that wiring a RateLimiter
// causes it to be consulted: a drained limiter blocks until ctx
// cancellation, with no HTTP request sent.
func TestClient_Do_RateLimiterWired(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	rl := NewRateLimiter(
		WithTierLimit(Tier3, TierLimit{Sustained: 1, Burst: 1, Window: time.Hour}),
	)
	// Drain the bucket up front so the next Do has to Wait.
	if !rl.Allow("not.a.real.method") {
		t.Fatal("Allow returned false; bucket draining setup failed")
	}

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
		WithRateLimiter(rl),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.Do(ctx, "not.a.real.method", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, DeadlineExceeded) = false, want true; got %v", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (limiter blocked)", calls)
	}
}

// TestClient_Do_NilParams_EmptyJSONBody asserts that nil params
// produce a `{}` body, not an empty string (Slack rejects empty
// bodies on `application/json` content type).
func TestClient_Do_NilParams_EmptyJSONBody(t *testing.T) {
	t.Parallel()

	var seen []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	if err := c.Do(context.Background(), "auth.test", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if string(seen) != "{}" {
		t.Errorf("body = %q, want {}", string(seen))
	}
}

// TestClient_Do_NonJSON5xx_GenericAPIError asserts that a 5xx with a
// non-JSON body (raw HTML from a load balancer, e.g.) surfaces as
// *APIError with Status set and Code empty — no JSON parse panic.
func TestClient_Do_NonJSON5xx_GenericAPIError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	err := c.Do(context.Background(), "chat.postMessage", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %T %v", err, err)
	}
	if apiErr.Status != http.StatusBadGateway {
		t.Errorf("Status = %d, want 502", apiErr.Status)
	}
	if apiErr.Code != "" {
		t.Errorf("Code = %q, want empty", apiErr.Code)
	}
	// Does NOT match any sentinel — Unwrap returns nil.
	if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrInvalidAuth) {
		t.Error("502 must not match a documented sentinel")
	}
}

// TestClient_Do_DecodeError_Wrapped asserts a malformed `ok:true`
// response body that fails to decode into `out` surfaces as a wrapped
// error (NOT an *APIError — the HTTP exchange itself succeeded).
func TestClient_Do_DecodeError_Wrapped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// `ok:true` envelope but a `count` that the test struct types
		// as a string — re-decode into `out` will fail.
		_, _ = io.WriteString(w, `{"ok":true,"count":"not-an-int"}`)
	}))
	t.Cleanup(srv.Close)

	type strictResp struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
	}
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	var out strictResp
	err := c.Do(context.Background(), "auth.test", nil, &out)
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("decode error must not be a *APIError: %v", err)
	}
}

// TestAPIError_Unwrap_NoMatch asserts that an unknown error code
// produces an *APIError that does NOT match any sentinel.
func TestAPIError_Unwrap_NoMatch(t *testing.T) {
	t.Parallel()

	apiErr := &APIError{Status: 200, Code: "totally_unknown_code", Method: "x"}
	if errors.Is(apiErr, ErrInvalidAuth) {
		t.Error("unknown code must not match ErrInvalidAuth")
	}
	if errors.Is(apiErr, ErrRateLimited) {
		t.Error("unknown code must not match ErrRateLimited")
	}
}

// TestAPIError_Error_Format covers the four String formats so a
// future refactor doesn't regress log readability.
func TestAPIError_Error_Format(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  *APIError
		want string
	}{
		{"empty", &APIError{Status: 500}, `slack: api error: status=500`},
		{"only-method", &APIError{Status: 500, Method: "x.y"}, `slack: api error: method="x.y" status=500`},
		{"only-code", &APIError{Status: 500, Code: "boom"}, `slack: api error: status=500 code="boom"`},
		{"both", &APIError{Status: 500, Method: "x.y", Code: "boom"}, `slack: api error: method="x.y" status=500 code="boom"`},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%s: Error() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestParseRetryAfter covers the three documented Retry-After forms
// (integer-seconds, HTTP-date, malformed).
func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty", "", 0},
		{"whitespace", "   ", 0},
		{"integer", "42", 42 * time.Second},
		{"negative-integer", "-3", 0},
		{"http-date-future", now.Add(15 * time.Second).Format(http.TimeFormat), 15 * time.Second},
		{"http-date-past", now.Add(-15 * time.Second).Format(http.TimeFormat), 0},
		{"malformed", "not a real duration", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseRetryAfter(tc.in, now)
			// HTTP-date precision is integer seconds; allow ±1s slop.
			diff := got - tc.want
			if diff < 0 {
				diff = -diff
			}
			if diff > time.Second {
				t.Errorf("parseRetryAfter(%q) = %v, want %v (±1s)", tc.in, got, tc.want)
			}
		})
	}
}

// TestWithBaseURL_Panics asserts each panic path documented on
// WithBaseURL fires with a clear message.
func TestWithBaseURL_Panics(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"missing-scheme", "example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for input %q, got none", tc.input)
				}
			}()
			_ = WithBaseURL(tc.input)
		})
	}
}

// TestNewClient_Defaults asserts NewClient with no options builds a
// usable client (default base URL, default HTTP timeout, no-op
// logger).
func TestNewClient_Defaults(t *testing.T) {
	t.Parallel()

	c := NewClient()
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if c.cfg.baseURL == nil {
		t.Fatal("default baseURL is nil")
	}
	if c.cfg.baseURL.String() != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.cfg.baseURL.String(), defaultBaseURL)
	}
	if c.cfg.httpClient == nil || c.cfg.httpClient.Timeout != defaultHTTPTimeout {
		t.Errorf("httpClient.Timeout = %v, want %v", c.cfg.httpClient.Timeout, defaultHTTPTimeout)
	}
}

// TestStaticToken_Token asserts the trivial helper round-trips.
func TestStaticToken_Token(t *testing.T) {
	t.Parallel()
	tok, err := StaticToken("xoxb-test").Token(context.Background())
	if err != nil {
		t.Errorf("Token: %v", err)
	}
	if tok != "xoxb-test" {
		t.Errorf("Token = %q, want xoxb-test", tok)
	}
}

// TestClient_Do_RequestBody_JSONShape asserts callers can pass a
// concrete struct as `params` and the server sees a valid JSON
// payload (no double-encoding, no leading/trailing whitespace).
func TestClient_Do_RequestBody_JSONShape(t *testing.T) {
	t.Parallel()

	type req struct {
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	var got req
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	if err := c.Do(context.Background(), "chat.postMessage", req{Channel: "C1", Text: "hello"}, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got.Channel != "C1" || got.Text != "hello" {
		t.Errorf("decoded request body = %+v, want {C1, hello}", got)
	}
}
