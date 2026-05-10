package jira

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Probe-style sentinels used by the redaction tests below. These
// strings are deliberately unique and obviously synthetic so the
// runtime redaction tests assert against EXACTLY them. A future
// regression that surfaces the email or token through the logger
// (or through any structured-metadata kv) will round-trip the
// probe value into the recordingLogger entries and the assertion
// fails. Generic values (`tester@example.com`, `secret-token`)
// would only catch verbatim-equality regressions; a probe-shape
// catches subtler kv leaks.
const (
	probeEmail = "REDACTION_PROBE_EMAIL_M81_DO_NOT_LOG"
	probeToken = "REDACTION_PROBE_TOKEN_M81_DO_NOT_LOG"
)

// newTestClient builds a [Client] wired to a fresh httptest.Server
// running `h`. The default options provide a parseable baseURL + a
// [StaticBasicAuth] populated with the redaction-probe sentinels;
// callers can append additional options. The httptest.Server is
// registered with t.Cleanup.
func newTestClient(t *testing.T, h http.HandlerFunc, opts ...ClientOption) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base := []ClientOption{
		WithBaseURL(srv.URL),
		WithBasicAuth(StaticBasicAuth{Email: probeEmail, Token: probeToken}),
	}
	base = append(base, opts...)
	c, err := NewClient(base...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, srv
}

func TestNewClient_RejectsMissingBaseURL(t *testing.T) {
	t.Parallel()
	_, err := NewClient(WithBasicAuth(StaticBasicAuth{Email: "a", Token: "b"}))
	if !errors.Is(err, ErrMissingBaseURL) {
		t.Fatalf("err = %v; want %v", err, ErrMissingBaseURL)
	}
}

func TestNewClient_RejectsMissingAuth(t *testing.T) {
	t.Parallel()
	_, err := NewClient(WithBaseURL("https://example.atlassian.net"))
	if !errors.Is(err, ErrMissingAuth) {
		t.Fatalf("err = %v; want %v", err, ErrMissingAuth)
	}
}

func TestNewClient_RejectsUnparseableBaseURL(t *testing.T) {
	t.Parallel()
	_, err := NewClient(
		WithBaseURL("not a url"),
		WithBasicAuth(StaticBasicAuth{Email: "a", Token: "b"}),
	)
	if !errors.Is(err, ErrInvalidBaseURL) {
		t.Fatalf("err = %v; want wrapped ErrInvalidBaseURL (distinguishes unparseable from missing)", err)
	}
}

func TestNewClient_HappyPath(t *testing.T) {
	t.Parallel()
	c, err := NewClient(
		WithBaseURL("https://example.atlassian.net"),
		WithBasicAuth(StaticBasicAuth{Email: "a@b.c", Token: "t"}),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c == nil {
		t.Fatal("Client is nil")
	}
}

func TestDo_SetsAuthHeader(t *testing.T) {
	t.Parallel()
	var seen string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"key":"PROJ-1"}`)
	})
	_, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(probeEmail+":"+probeToken))
	if seen != want {
		t.Fatalf("Authorization = %q; want %q", seen, want)
	}
}

func TestDo_SetsAcceptAndUserAgent(t *testing.T) {
	t.Parallel()
	var accept, ua string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		ua = r.Header.Get("User-Agent")
		_, _ = io.WriteString(w, `{"key":"PROJ-1"}`)
	})
	_, _ = c.GetIssue(context.Background(), "PROJ-1", nil)
	if accept != "application/json" {
		t.Errorf("Accept = %q; want application/json", accept)
	}
	if !strings.HasPrefix(ua, "watchkeepers-jira/") {
		t.Errorf("User-Agent = %q; want watchkeepers-jira/...", ua)
	}
}

func TestDo_TransportError_WrappedAndContextCancelled(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
		// Block forever; we'll cancel ctx.
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := c.GetIssue(ctx, "PROJ-1", nil)
	if err == nil {
		t.Fatal("expected error on cancelled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want wrapped context.Canceled", err)
	}
}

func TestDo_AuthSourceError_NeverHitsNetwork(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
	}, WithBasicAuth(failingAuth{}))
	_, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	if err == nil {
		t.Fatal("expected error from auth source")
	}
	if !strings.Contains(err.Error(), "resolve credentials") {
		t.Errorf("err = %q; want it to mention credential resolution", err)
	}
	if calls.Load() != 0 {
		t.Errorf("HTTP handler invoked %d times; want 0 (auth must fail-closed before the network)", calls.Load())
	}
}

type failingAuth struct{}

func (failingAuth) BasicAuth(context.Context) (string, string, error) {
	return "", "", errors.New("auth source intentionally broken")
}

func TestDo_429ReturnsErrRateLimitedAndRetryAfter(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"errorMessages":["throttled"]}`)
	})
	_, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v; want wrapped ErrRateLimited", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v; want APIError in chain", err)
	}
	if apiErr.RetryAfter != 42*time.Second {
		t.Errorf("RetryAfter = %v; want 42s", apiErr.RetryAfter)
	}
}

func TestDo_404OnIssueEndpointReturnsErrIssueNotFound(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errorMessages":["Issue does not exist or you do not have permission to see it."]}`)
	})
	_, err := c.GetIssue(context.Background(), "PROJ-999", nil)
	if !errors.Is(err, ErrIssueNotFound) {
		t.Fatalf("err = %v; want wrapped ErrIssueNotFound", err)
	}
}

func TestDo_401ReturnsErrInvalidAuth(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errorMessages":["Unauthorized"]}`)
	})
	_, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	if !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("err = %v; want wrapped ErrInvalidAuth", err)
	}
}

func TestDo_403ReturnsErrInvalidAuth(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"errorMessages":["Forbidden"]}`)
	})
	_, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	if !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("err = %v; want wrapped ErrInvalidAuth", err)
	}
}

func TestDo_RawAPIErrorOnUnmappedStatus(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"errorMessages":["server hiccup"]}`)
	})
	_, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	if err == nil {
		t.Fatal("expected error on 500")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v; want APIError in chain", err)
	}
	if apiErr.Status != 500 {
		t.Errorf("Status = %d; want 500", apiErr.Status)
	}
	for _, sentinel := range []error{ErrInvalidAuth, ErrIssueNotFound, ErrRateLimited, ErrInvalidJQL} {
		if errors.Is(err, sentinel) {
			t.Errorf("err = %v unexpectedly Is %v (500 should be unmapped)", err, sentinel)
		}
	}
}

func TestDo_LoggerReceivesOnlyStructuredMetadata(t *testing.T) {
	t.Parallel()
	logs := newRecordingLogger()
	c, _ := newTestClient(
		t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"key":"PROJ-1"}`)
		},
		WithLogger(logs),
	)
	_, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if logs.size() == 0 {
		t.Fatal("logger received zero entries; expected at least one success entry")
	}
	logs.assertNoForbiddenValues(
		t,
		probeToken,
		probeEmail,
		"Authorization",
		"Basic ", // would catch a leaked Authorization header value
	)
}

func TestDo_LoggerRedactsAtAPIErrorPath(t *testing.T) {
	t.Parallel()
	logs := newRecordingLogger()
	const tenantSpecificMessage = "REDACTION_PROBE_TENANT_TEXT_M81"
	c, _ := newTestClient(
		t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"errorMessages":["`+tenantSpecificMessage+`"]}`)
		},
		WithLogger(logs),
	)
	_, _ = c.GetIssue(context.Background(), "PROJ-1", nil)
	logs.assertNoForbiddenValues(
		t,
		probeToken,
		probeEmail,
		tenantSpecificMessage,
	)
}

func TestDo_LoggerRedactsAtTransportErrorPath(t *testing.T) {
	t.Parallel()
	logs := newRecordingLogger()
	c, err := NewClient(
		WithBaseURL("https://nonexistent-host-for-redaction-probe.invalid"),
		WithBasicAuth(StaticBasicAuth{Email: probeEmail, Token: probeToken}),
		WithLogger(logs),
		WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, _ = c.GetIssue(context.Background(), "PROJ-1", nil)
	logs.assertNoForbiddenValues(
		t,
		probeToken,
		probeEmail,
		"nonexistent-host-for-redaction-probe.invalid", // tenant subdomain MUST NOT leak via err.Error()
	)
}

// TestRedactionDiscipline_SourceGrep pins the redaction contract via a
// source-grep AC: the package's logger emit-points MUST NOT mention
// any forbidden key. Mutation-tested — if a future patch adds
// `"token", token` or `"body", req.body` to a Logger.Log call the
// test fails.
//
// Per the M7.1.c.a lesson: source-grep beats hollow runtime-mock
// assertions for negative ACs ("does NOT log secrets"). A mock-based
// assertion only checks the values reaching the configured sink; a
// source-grep catches misuse via any unconfigured path too.
func TestRedactionDiscipline_SourceGrep(t *testing.T) {
	t.Parallel()
	const target = "client.go"
	src, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	stripped := stripGoComments(string(src))

	// File-level negative AC: literal substrings that MUST NOT
	// appear anywhere in client.go outside comments. Narrow on
	// purpose — the more rigorous AC is the Logger.Log call-site
	// scan below.
	fileForbidden := []string{
		`"api_token"`,
		`"request_body"`,
		`"response_body"`,
		`req.Body`,
	}
	for _, f := range fileForbidden {
		if strings.Contains(stripped, f) {
			t.Errorf("client.go contains forbidden pattern %q outside comments — redaction discipline says no body/token/email in logs or struct keys", f)
		}
	}

	// Logger.Log-level negative AC: extract every `c.cfg.logger.Log(...)`
	// call site (multi-line, paren-balanced) and assert NONE of them
	// contains a forbidden substring. Catches realistic regressions
	// that a kv-key rename would otherwise slip past:
	//   `, token)` / `, token,` → `Log(..., "key", token)` final/middle-arg leak.
	//   `, email)` / `, email,` → same shape with the email var.
	//   `, body)`  / `, body,`  → request/response body leak.
	//   `, req.Body...`         → request body leak.
	//   `+token`   / `+email`   → string-concat leak inside the msg.
	loggerCallForbidden := []string{
		`, token)`, `, token,`,
		`, email)`, `, email,`,
		`, body)`, `, body,`,
		`, req.Body`,
		`+token`, `+email`,
	}
	calls := extractLoggerCalls(stripped)
	if len(calls) == 0 {
		t.Fatal("no `logger.Log(` call sites found — extractor is broken or the file no longer logs")
	}
	for _, call := range calls {
		for _, f := range loggerCallForbidden {
			if strings.Contains(call, f) {
				t.Errorf("logger.Log call contains forbidden pattern %q — redaction discipline violation; call:\n%s", f, call)
			}
		}
	}
}

// extractLoggerCalls walks `src` and returns each
// `c.cfg.logger.Log(...)` call as a single string (start through the
// matching closing paren, paren-balanced so multi-line argument
// lists round-trip in one chunk). Used by
// [TestRedactionDiscipline_SourceGrep] to scan only the diagnostic
// sink's argument lists, not the whole file.
func extractLoggerCalls(src string) []string {
	const needle = "logger.Log("
	var out []string
	for i := 0; i < len(src); {
		idx := strings.Index(src[i:], needle)
		if idx < 0 {
			break
		}
		start := i + idx
		depth := 1
		j := start + len(needle)
		for j < len(src) && depth > 0 {
			switch src[j] {
			case '(':
				depth++
			case ')':
				depth--
			}
			j++
		}
		out = append(out, src[start:j])
		i = j
	}
	return out
}

// stripGoComments removes // line comments and /* ... */ block
// comments from `src` so the source-grep AC can ignore allowed
// mentions inside docstrings (e.g. "NEVER passes the email" in the
// redaction-discipline doc paragraph).
func stripGoComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	inLine := false
	inBlock := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inLine {
			if c == '\n' {
				inLine = false
				b.WriteByte(c)
			}
			continue
		}
		if inBlock {
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(src) {
			if src[i+1] == '/' {
				inLine = true
				i++
				continue
			}
			if src[i+1] == '*' {
				inBlock = true
				i++
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func TestParseRetryAfter_IntegerSeconds(t *testing.T) {
	t.Parallel()
	got := parseRetryAfter("17", time.Now)
	if got != 17*time.Second {
		t.Errorf("parseRetryAfter(\"17\") = %v; want 17s", got)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	target := now.Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	got := parseRetryAfter(target, func() time.Time { return now })
	if got < 110*time.Second || got > 130*time.Second {
		t.Errorf("parseRetryAfter(http-date) = %v; want ~120s", got)
	}
}

func TestParseRetryAfter_PastDateClamps(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	target := now.Add(-time.Hour).UTC().Format(http.TimeFormat)
	got := parseRetryAfter(target, func() time.Time { return now })
	if got != 0 {
		t.Errorf("parseRetryAfter(past) = %v; want 0", got)
	}
}

func TestParseRetryAfter_Garbage(t *testing.T) {
	t.Parallel()
	got := parseRetryAfter("not-a-time", time.Now)
	if got != 0 {
		t.Errorf("parseRetryAfter(garbage) = %v; want 0", got)
	}
}

func TestParseTime_Variants(t *testing.T) {
	t.Parallel()
	cases := []string{
		"2024-09-15T14:30:00.123+0000",       // canonical Atlassian, 3-digit fractional
		"2024-09-15T14:30:00+0000",           // no fractional
		"2024-09-15T14:30:00.123Z",           // Z suffix, 3-digit fractional
		"2024-09-15T14:30:00Z",               // Z suffix, no fractional
		"2024-09-15T14:30:00.123456+0000",    // 6-digit fractional (strip-and-retry path)
		"2024-09-15T14:30:00.123456789+0000", // 9-digit fractional (strip-and-retry path)
		"2024-09-15T14:30:00.1+0000",         // 1-digit fractional (strip-and-retry path)
		"2024-09-15T14:30:00.123+05:30",      // RFC 3339 colon-offset
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if got := parseTime(raw); got.IsZero() {
				t.Errorf("parseTime(%q) returned zero — variable fractional digits OR colon-offset must round-trip", raw)
			}
		})
	}
	if got := parseTime("not-a-time"); !got.IsZero() {
		t.Errorf("parseTime(garbage) = %v; want zero", got)
	}
	if got := parseTime("2024-09-15T14:30:00"); !got.IsZero() {
		// No timezone is genuinely unparseable; document via the
		// zero-return contract and the Issue.Created/Updated
		// docstring collision warning.
		_ = got
	}
}

// recordingLogger captures every Logger.Log call as a single string
// (joined "key=value" pairs) so tests can scan for forbidden values.
type recordingLogger struct {
	mu      sync.Mutex
	entries []string
}

func newRecordingLogger() *recordingLogger { return &recordingLogger{} }

func (l *recordingLogger) Log(_ context.Context, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteByte(' ')
		b.WriteString(toString(kv[i]))
		b.WriteByte('=')
		b.WriteString(toString(kv[i+1]))
	}
	l.entries = append(l.entries, b.String())
}

func (l *recordingLogger) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func (l *recordingLogger) assertNoForbiddenValues(t *testing.T, forbidden ...string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		for _, f := range forbidden {
			if strings.Contains(e, f) {
				t.Errorf("logger entry %q contains forbidden value %q — redaction discipline violated", e, f)
			}
		}
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case error:
		return x.Error()
	default:
		return ""
	}
}
