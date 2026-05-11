package github

import (
	"context"
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
// regression that surfaces the token through the logger (or through
// any structured-metadata kv) will round-trip the probe value into
// the recordingLogger entries and the assertion fails. Generic values
// would only catch verbatim-equality regressions; a probe-shape
// catches subtler kv leaks.
const probeToken = "REDACTION_PROBE_TOKEN_M82D_DO_NOT_LOG" //nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.

// newTestClient builds a [Client] wired to a fresh httptest.Server
// running `h`. The default options provide a parseable baseURL + a
// [StaticToken] populated with the redaction-probe sentinel; callers
// can append additional options. The httptest.Server is registered
// with t.Cleanup.
func newTestClient(t *testing.T, h http.HandlerFunc, opts ...ClientOption) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base := []ClientOption{
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken{Value: probeToken}),
	}
	base = append(base, opts...)
	c, err := NewClient(base...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, srv
}

func TestNewClient_DefaultBaseURLIsAPI(t *testing.T) {
	t.Parallel()
	c, err := NewClient(WithTokenSource(StaticToken{Value: "t"}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c.cfg.baseURL.String(); got != defaultBaseURL {
		t.Errorf("default base URL = %q; want %q", got, defaultBaseURL)
	}
}

func TestNewClient_RejectsMissingAuth(t *testing.T) {
	t.Parallel()
	_, err := NewClient()
	if !errors.Is(err, ErrMissingAuth) {
		t.Fatalf("err = %v; want wrapped ErrMissingAuth", err)
	}
}

func TestNewClient_RejectsUnparseableBaseURL(t *testing.T) {
	t.Parallel()
	_, err := NewClient(
		WithBaseURL("not a url"),
		WithTokenSource(StaticToken{Value: "t"}),
	)
	if !errors.Is(err, ErrInvalidBaseURL) {
		t.Fatalf("err = %v; want wrapped ErrInvalidBaseURL", err)
	}
}

func TestNewClient_AcceptsGHESPathPrefix(t *testing.T) {
	t.Parallel()
	// Unlike jira (which rejects path prefixes — they would route to
	// Confluence), GitHub Enterprise Server REQUIRES /api/v3 prefix.
	_, err := NewClient(
		WithBaseURL("https://github.example.com/api/v3"),
		WithTokenSource(StaticToken{Value: "t"}),
	)
	if err != nil {
		t.Fatalf("NewClient with GHES path prefix should succeed: %v", err)
	}
}

func TestDo_SetsBearerAuthHeader(t *testing.T) {
	t.Parallel()
	var seen string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})
	_, err := c.ListPullRequests(context.Background(), "owner", "repo", ListPullRequestsOptions{})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	want := "Bearer " + probeToken
	if seen != want {
		t.Fatalf("Authorization = %q; want %q", seen, want)
	}
}

func TestDo_SetsAcceptUAAndAPIVersion(t *testing.T) {
	t.Parallel()
	var accept, ua, apiVer string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		ua = r.Header.Get("User-Agent")
		apiVer = r.Header.Get("X-GitHub-Api-Version")
		_, _ = io.WriteString(w, `[]`)
	})
	_, _ = c.ListPullRequests(context.Background(), "owner", "repo", ListPullRequestsOptions{})
	if accept != acceptHeader {
		t.Errorf("Accept = %q; want %q", accept, acceptHeader)
	}
	if !strings.HasPrefix(ua, "watchkeepers-github/") {
		t.Errorf("User-Agent = %q; want watchkeepers-github/...", ua)
	}
	if apiVer != apiVersionHeader {
		t.Errorf("X-GitHub-Api-Version = %q; want %q", apiVer, apiVersionHeader)
	}
}

func TestDo_TransportError_WrappedAndContextCancelled(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := c.ListPullRequests(ctx, "owner", "repo", ListPullRequestsOptions{})
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
	}, WithTokenSource(failingToken{}))
	_, err := c.ListPullRequests(context.Background(), "owner", "repo", ListPullRequestsOptions{})
	if err == nil {
		t.Fatal("expected error from token source")
	}
	if !strings.Contains(err.Error(), "resolve credentials") {
		t.Errorf("err = %q; want it to mention credential resolution", err)
	}
	if calls.Load() != 0 {
		t.Errorf("HTTP handler invoked %d times; want 0 (auth must fail-closed before the network)", calls.Load())
	}
}

type failingToken struct{}

func (failingToken) Token(context.Context) (string, error) {
	return "", errors.New("token source intentionally broken")
}

func TestDo_429ReturnsErrRateLimited(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Minute).Unix()
	c, _ := newTestClient(
		t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strFromInt64(reset))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"message":"slow down"}`)
		},
		WithClock(func() time.Time { return now }),
	)
	_, err := c.ListPullRequests(context.Background(), "owner", "repo", ListPullRequestsOptions{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v; want wrapped ErrRateLimited", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v; want APIError in chain", err)
	}
	if apiErr.RetryAfter < 110*time.Second || apiErr.RetryAfter > 130*time.Second {
		t.Errorf("RetryAfter = %v; want ~120s from X-RateLimit-Reset", apiErr.RetryAfter)
	}
}

func TestDo_403WithZeroRemaining_AliasesRateLimited(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	})
	_, err := c.ListPullRequests(context.Background(), "owner", "repo", ListPullRequestsOptions{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v; want wrapped ErrRateLimited (403 with X-RateLimit-Remaining: 0 is GitHub's documented primary-rate-limit shape)", err)
	}
}

func TestDo_403WithMissingRateLimitHeader_DoesNotAliasRateLimited(t *testing.T) {
	t.Parallel()
	// Iter-1 critic Minor #6: when X-RateLimit-Remaining is absent
	// the parser returns -1 (NOT 0), so the Unwrap path does NOT
	// alias rate-limit. Pin this so a future regression that changes
	// the absent-header default to 0 fails CI.
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// No X-RateLimit-Remaining header at all.
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"forbidden, no rate-limit hint"}`)
	})
	_, err := c.ListPullRequests(context.Background(), "owner", "repo", ListPullRequestsOptions{})
	if errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v; must NOT alias ErrRateLimited when X-RateLimit-Remaining header is absent (default -1, not 0)", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v; want APIError in chain", err)
	}
	if apiErr.RateLimitRemaining != -1 {
		t.Errorf("RateLimitRemaining = %d; want -1 (header absent default)", apiErr.RateLimitRemaining)
	}
}

func TestDo_403WithNonZeroRemaining_DoesNotAliasRateLimited(t *testing.T) {
	t.Parallel()
	// Plain 403 — token lacks the scope. MUST NOT alias rate-limit;
	// callers may need to surface "scope missing" distinctly.
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
	})
	_, err := c.ListPullRequests(context.Background(), "owner", "repo", ListPullRequestsOptions{})
	if errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v; must NOT alias ErrRateLimited when X-RateLimit-Remaining > 0 (auth/scope failure, not rate-limit)", err)
	}
}

func TestDo_404OnRepoEndpointReturnsErrRepoNotFound(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})
	_, err := c.ListPullRequests(context.Background(), "owner", "missing", ListPullRequestsOptions{})
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v; want wrapped ErrRepoNotFound", err)
	}
}

func TestDo_401ReturnsErrInvalidAuth(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
	})
	_, err := c.ListPullRequests(context.Background(), "owner", "repo", ListPullRequestsOptions{})
	if !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("err = %v; want wrapped ErrInvalidAuth", err)
	}
}

func TestDo_RawAPIErrorOnUnmappedStatus(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"server hiccup"}`)
	})
	_, err := c.ListPullRequests(context.Background(), "owner", "repo", ListPullRequestsOptions{})
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
	for _, sentinel := range []error{ErrInvalidAuth, ErrRepoNotFound, ErrRateLimited} {
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
			_, _ = io.WriteString(w, `[]`)
		},
		WithLogger(logs),
	)
	_, err := c.ListPullRequests(context.Background(), "owner", "repo", ListPullRequestsOptions{})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if logs.size() == 0 {
		t.Fatal("logger received zero entries; expected at least one success entry")
	}
	logs.assertNoForbiddenValues(
		t,
		probeToken,
		"Authorization",
		"Bearer ",
	)
}

func TestDo_LoggerRedactsAtAPIErrorPath(t *testing.T) {
	t.Parallel()
	logs := newRecordingLogger()
	const tenantSpecificMessage = "REDACTION_PROBE_TENANT_TEXT_M82D"
	c, _ := newTestClient(
		t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"`+tenantSpecificMessage+`"}`)
		},
		WithLogger(logs),
	)
	_, _ = c.ListPullRequests(context.Background(), "owner", "repo", ListPullRequestsOptions{})
	logs.assertNoForbiddenValues(
		t,
		probeToken,
		tenantSpecificMessage,
	)
}

func TestDo_LoggerRedactsAtTransportErrorPath(t *testing.T) {
	t.Parallel()
	logs := newRecordingLogger()
	c, err := NewClient(
		WithBaseURL("https://nonexistent-host-for-redaction-probe.invalid"),
		WithTokenSource(StaticToken{Value: probeToken}),
		WithLogger(logs),
		WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, _ = c.ListPullRequests(context.Background(), "owner", "repo", ListPullRequestsOptions{})
	logs.assertNoForbiddenValues(
		t,
		probeToken,
		"nonexistent-host-for-redaction-probe.invalid",
	)
}

// TestRedactionDiscipline_SourceGrep pins the redaction contract via a
// source-grep AC: the package's logger emit-points MUST NOT mention any
// forbidden key. Mirrors the M8.1 lesson #7 layered source-grep
// discipline (file-level + Logger.Log call-site).
func TestRedactionDiscipline_SourceGrep(t *testing.T) {
	t.Parallel()
	const target = "client.go"
	src, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	stripped := stripGoComments(string(src))

	fileForbidden := []string{
		`"api_token"`,
		`"bearer_token"`,
		`"request_body"`,
		`"response_body"`,
		`req.Body`,
	}
	for _, f := range fileForbidden {
		if strings.Contains(stripped, f) {
			t.Errorf("client.go contains forbidden pattern %q outside comments — redaction discipline says no body/token in logs or struct keys", f)
		}
	}

	loggerCallForbidden := []string{
		`, token)`, `, token,`,
		`, body)`, `, body,`,
		`, req.Body`,
		`+token`,
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
// `c.cfg.logger.Log(...)` call as a single string (paren-balanced).
// Used by [TestRedactionDiscipline_SourceGrep] to scan only the
// diagnostic sink's argument lists.
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

// stripGoComments removes // line comments and /* ... */ block comments
// from `src` so the source-grep AC can ignore allowed mentions inside
// docstrings.
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

func TestParseResetHeader_FutureEpoch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	target := now.Add(2 * time.Minute).Unix()
	got := parseResetHeader(strFromInt64(target), func() time.Time { return now })
	if got < 110*time.Second || got > 130*time.Second {
		t.Errorf("parseResetHeader(future) = %v; want ~120s", got)
	}
}

func TestParseResetHeader_PastEpochClamps(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	target := now.Add(-time.Hour).Unix()
	got := parseResetHeader(strFromInt64(target), func() time.Time { return now })
	if got != 0 {
		t.Errorf("parseResetHeader(past) = %v; want 0", got)
	}
}

func TestParseResetHeader_Garbage(t *testing.T) {
	t.Parallel()
	if got := parseResetHeader("not-a-time", time.Now); got != 0 {
		t.Errorf("parseResetHeader(garbage) = %v; want 0", got)
	}
	if got := parseResetHeader("", time.Now); got != 0 {
		t.Errorf("parseResetHeader(empty) = %v; want 0", got)
	}
}

func TestParseRateLimitRemaining(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]int{
		"":       -1,
		"   ":    -1,
		"abc":    -1,
		"0":      0,
		"42":     42,
		"4999":   4999,
		"  100 ": 100,
		"-1":     -1,
		"1.0":    -1,
	} {
		if got := parseRateLimitRemaining(raw); got != want {
			t.Errorf("parseRateLimitRemaining(%q) = %d; want %d", raw, got, want)
		}
	}
}

func TestParseLinkHeaderNextPage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		hdr     string
		want    int
		wantErr bool
	}{
		{"empty", "", 0, false},
		{"only first/last", `<https://api.github.com/repos/o/r/pulls?page=1>; rel="first", <https://api.github.com/repos/o/r/pulls?page=10>; rel="last"`, 0, false},
		{"next first in list", `<https://api.github.com/repos/o/r/pulls?page=2>; rel="next", <https://api.github.com/repos/o/r/pulls?page=10>; rel="last"`, 2, false},
		{"next not first", `<https://api.github.com/repos/o/r/pulls?page=1>; rel="prev", <https://api.github.com/repos/o/r/pulls?page=3>; rel="next"`, 3, false},
		{"page with other params", `<https://api.github.com/repos/o/r/pulls?per_page=100&page=5&state=open>; rel="next"`, 5, false},
		// Iter-1 codex Major #1: rel="next" marker present but no
		// parseable page= → MUST surface error rather than silent 0.
		{"no page param in rel-next URL", `<https://api.github.com/repos/o/r/pulls>; rel="next"`, 0, true},
		// Iter-1 critic Major #2: rel="Next" capitalised → RFC 8288
		// says case-insensitive; the parser MUST accept it.
		{"case-insensitive Next", `<https://api.github.com/repos/o/r/pulls?page=7>; rel="Next"`, 7, false},
		{"case-insensitive NEXT", `<https://api.github.com/repos/o/r/pulls?page=8>; rel="NEXT"`, 8, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLinkHeaderNextPage(tc.hdr)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parseLinkHeaderNextPage(%q) returned no error; want error (rel=\"next\" marker without parseable page= must fail loudly)", tc.hdr)
				}
				return
			}
			if err != nil {
				t.Errorf("parseLinkHeaderNextPage(%q) = err %v; want nil", tc.hdr, err)
			}
			if got != tc.want {
				t.Errorf("parseLinkHeaderNextPage = %d; want %d", got, tc.want)
			}
		})
	}
}

func TestParseTime_Variants(t *testing.T) {
	t.Parallel()
	good := []string{
		"2024-09-15T14:30:00Z",
		"2024-09-15T14:30:00+00:00",
		"2024-09-15T14:30:00.123Z",
	}
	for _, raw := range good {
		t.Run(raw, func(t *testing.T) {
			if got := parseTime(raw); got.IsZero() {
				t.Errorf("parseTime(%q) returned zero", raw)
			}
		})
	}
	if got := parseTime("not-a-time"); !got.IsZero() {
		t.Errorf("parseTime(garbage) = %v; want zero", got)
	}
	if got := parseTime(""); !got.IsZero() {
		t.Errorf("parseTime(empty) = %v; want zero", got)
	}
}

func TestValidateOwner_RejectsMalformed(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"-leading-hyphen",
		"trailing-hyphen-",
		"has space",
		"has/slash",
		"a..b",
		"a@b",
		"averylonglogin-that-exceeds-the-thirty-nine-character-cap-for-github-logins",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if err := validateOwner(RepoOwner(raw)); !errors.Is(err, ErrInvalidArgs) {
				t.Errorf("validateOwner(%q) = %v; want wrapped ErrInvalidArgs", raw, err)
			}
		})
	}
	good := []string{"alice", "Org-Name", "octocat", "a", "ab-c-d-1-2"}
	for _, raw := range good {
		if err := validateOwner(RepoOwner(raw)); err != nil {
			t.Errorf("validateOwner(%q) = %v; want nil", raw, err)
		}
	}
}

func TestValidateRepo_RejectsMalformed(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"has space",
		"has/slash",
		"has@at",
		strings.Repeat("a", 101),
		// Iter-1 critic Major #1: path-traversal shapes MUST reject
		// at the regex layer. `.` and `..` would compose to
		// `/repos/owner/./pulls` or `/repos/owner/../pulls`, and
		// GHES proxies that normalise paths may route to a
		// different tenant.
		".",
		"..",
		"...",
		".foo",
		"..foo",
		// Leading hyphen — inconsistency with the owner pattern's
		// no-leading-hyphen discipline, and GitHub itself rejects
		// these at repo-creation time. Iter-1 critic Nit #8.
		"-foo",
		"-",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if err := validateRepo(RepoName(raw)); !errors.Is(err, ErrInvalidArgs) {
				t.Errorf("validateRepo(%q) = %v; want wrapped ErrInvalidArgs", raw, err)
			}
		})
	}
	good := []string{"repo", "watch-keepers", "a.b.c", "_test", "repo_with_underscore", "1234", "a"}
	for _, raw := range good {
		if err := validateRepo(RepoName(raw)); err != nil {
			t.Errorf("validateRepo(%q) = %v; want nil", raw, err)
		}
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
	case int:
		return strFromInt64(int64(x))
	default:
		return ""
	}
}

func strFromInt64(v int64) string {
	// Tiny stdlib-free helper to avoid pulling fmt into the recording
	// helper's hot path.
	if v == 0 {
		return "0"
	}
	neg := false
	if v < 0 {
		neg = true
		v = -v
	}
	var buf [20]byte
	pos := len(buf)
	for v > 0 {
		pos--
		buf[pos] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
