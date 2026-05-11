package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// defaultHTTPTimeout is the timeout applied to the [Client]'s default
// *http.Client when the caller does not supply one via [WithHTTPClient].
// 30 seconds matches the jira / slack-package precedent and is loose
// enough for `/pulls` calls returning a per_page=100 page.
const defaultHTTPTimeout = 30 * time.Second

// errorBodyLimit caps the bytes read from a non-2xx response body so a
// pathological server cannot force unbounded allocation. 4 KiB is
// plenty for a GitHub error envelope plus diagnostics and matches the
// jira / slack discipline.
const errorBodyLimit = 1 << 12

// userAgent is the static identification string the [Client] sends on
// every request. GitHub's REST API REQUIRES a User-Agent header on
// every request — unidentified clients are 403'd at the edge. A
// stable UA also lets GitHub correlate cross-method traffic to a
// single integration.
const userAgent = "watchkeepers-github/0.1 (+https://github.com/vadimtrunov/watchkeepers)"

// defaultBaseURL is the canonical public-cloud GitHub REST API host.
// [WithBaseURL] overrides for GitHub Enterprise Server deployments.
const defaultBaseURL = "https://api.github.com"

// apiVersionHeader pins the GitHub REST API version. The latest
// documented value as of M8.2.d (2026-05) is `2022-11-28`; pinning
// guards against silent shape evolution.
const apiVersionHeader = "2022-11-28"

// acceptHeader is GitHub's recommended Accept value for REST API
// callers. Plain `application/json` works too, but the documented
// shape is the vendor mime type.
const acceptHeader = "application/vnd.github+json"

// TokenSource resolves the GitHub bearer token for a single REST call.
// The shape mirrors `slack.TokenSource` — callers wrapping a rotating
// secret store (M3.4.b secrets interface) can drive per-request refresh
// transparently.
//
// Implementations MUST be safe for concurrent use across goroutines.
//
// GitHub's documented bearer scheme is `Authorization: Bearer <TOKEN>`
// (canonical for PATs + GitHub Apps). The token is the user-scoped
// fine-grained PAT or classic PAT created at
// https://github.com/settings/tokens, OR a GitHub App installation
// token retrieved via the App installation flow.
type TokenSource interface {
	// Token returns the bearer token for the supplied ctx. Errors
	// surface as wrapped failures from [Client.do] (the request is
	// never sent on a credential-resolution failure — security
	// invariant matches the jira / slack adapters).
	Token(ctx context.Context) (string, error)
}

// StaticToken is the trivial [TokenSource] that always returns the
// same token. Useful in tests and for bootstrapping; production
// wiring should use a [TokenSource] backed by the secrets interface
// so token rotation is observable.
type StaticToken struct {
	Value string
}

// Token satisfies [TokenSource] for [StaticToken].
func (s StaticToken) Token(context.Context) (string, error) { return s.Value, nil }

// Logger is the optional diagnostic sink wired in via [WithLogger].
// The shape mirrors the jira / slack / keepclient / keeperslog Logger
// interfaces: a single `Log(ctx, msg, kv...)` so callers can substitute
// structured loggers (slog, zap wrapper, …) without losing type
// compatibility.
//
// IMPORTANT (redaction discipline): the [Client] NEVER passes the
// token, the Authorization header, the request body, or the GitHub
// response body through the logger. Only structured metadata (HTTP
// method, endpoint path, status, error class, X-RateLimit-Reset
// value) appears in log entries. Mirrors the M3.4.b / M3.5 / M8.1
// redaction patterns documented in `docs/LESSONS.md`.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// Client is the low-level GitHub REST API HTTP client. Construct via
// [NewClient]. The zero value is not usable.
//
// The single high-level operation M8.2.d ships ([Client.ListPullRequests])
// composes through [Client.do], which carries the auth, decoding,
// envelope-decoding, and redaction discipline.
//
// Client is safe for concurrent use across goroutines once
// constructed; configuration is immutable after [NewClient] returns.
type Client struct {
	cfg clientConfig
}

// clientConfig is the internal mutable bag the [ClientOption]
// callbacks populate. Held in a separate type so [Client] itself
// stays immutable after [NewClient] returns.
type clientConfig struct {
	baseURL    *url.URL
	httpClient *http.Client
	auth       TokenSource
	logger     Logger
	clock      func() time.Time

	// baseURLInvalid is the flag [WithBaseURL] flips when it parses or
	// validates a non-conforming string ([NewClient] surfaces the flag
	// as wrapped [ErrInvalidBaseURL]).
	baseURLInvalid bool
}

// NewClient constructs a [Client] from the supplied options. The
// invariants checked synchronously:
//
//   - [WithTokenSource] MUST have been supplied; otherwise returns
//     wrapped [ErrMissingAuth] (fail-closed).
//   - [WithBaseURL], if supplied, MUST parse cleanly and have a
//     non-empty scheme + host; otherwise returns wrapped
//     [ErrInvalidBaseURL]. When NOT supplied, the default
//     `https://api.github.com` is used.
//
// Optional configuration (httpClient, logger, clock) falls back to
// safe defaults.
func NewClient(opts ...ClientOption) (*Client, error) {
	def, err := url.Parse(defaultBaseURL)
	if err != nil {
		// Unreachable: the literal is a valid URL — but defend the
		// invariant for future edits.
		return nil, fmt.Errorf("github: NewClient: internal: default base URL: %w", err)
	}
	cfg := clientConfig{
		baseURL: def,
		clock:   time.Now,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.baseURLInvalid {
		return nil, fmt.Errorf("github: NewClient: %w", ErrInvalidBaseURL)
	}
	if cfg.auth == nil {
		return nil, fmt.Errorf("github: NewClient: %w", ErrMissingAuth)
	}
	if cfg.httpClient == nil {
		cfg.httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{cfg: cfg}, nil
}

// doParams is the internal call descriptor passed to [Client.do]. The
// dispatching method (ListPullRequests, …) populates it; the lower-
// level transport handles auth + encoding + envelope decoding.
type doParams struct {
	method   string
	path     string
	query    url.Values
	body     any               // when non-nil, JSON-encoded as the request body
	dst      any               // when non-nil, JSON-decoded from the success response body
	headerFn func(http.Header) // optional: read response headers (e.g. Link, X-RateLimit-*)
	kind     endpointKind      // disambiguates 404 / 403 mapping in [APIError.Unwrap]
	expected map[int]bool      // optional set of acceptable success statuses; defaults to {200, 201, 204}
}

// do executes a single REST call against the configured base URL with
// auth, JSON encoding, response-envelope decoding, and structured-
// metadata-only logging. Callers (ListPullRequests, …) supply a
// [doParams]; do never inspects the success body except to populate
// [doParams.dst] (or skip when dst is nil).
//
// Failure paths:
//
//   - [TokenSource] error → wrapped, no HTTP request sent.
//   - [http.Client.Do] transport error → wrapped, no envelope.
//   - non-2xx response → [*APIError] populated from the envelope (when
//     parseable) or with empty Message otherwise; status / endpoint /
//     method / kind / RetryAfter populated.
//
// The Authorization header carries `Bearer <token>` per GitHub's
// documented REST API auth scheme.
func (c *Client) do(ctx context.Context, p doParams) error {
	if p.method == "" {
		return fmt.Errorf("github: do: %w: empty method", ErrInvalidArgs)
	}
	if p.path == "" {
		return fmt.Errorf("github: do: %w: empty path", ErrInvalidArgs)
	}

	token, err := c.cfg.auth.Token(ctx)
	if err != nil {
		return fmt.Errorf("github: do: resolve credentials: %w", err)
	}

	u := *c.cfg.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + p.path
	if len(p.query) > 0 {
		u.RawQuery = p.query.Encode()
	}

	var bodyReader io.Reader
	if p.body != nil {
		buf, err := json.Marshal(p.body)
		if err != nil {
			return fmt.Errorf("github: do: encode body: %w", err)
		}
		bodyReader = strings.NewReader(string(buf))
	}

	req, err := http.NewRequestWithContext(ctx, p.method, u.String(), bodyReader)
	if err != nil {
		return fmt.Errorf("github: do: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set("X-GitHub-Api-Version", apiVersionHeader)
	req.Header.Set("Authorization", "Bearer "+token)
	if p.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.cfg.httpClient.Do(req)
	if err != nil {
		c.logTransportError(ctx, p, err)
		return fmt.Errorf("github: do: %s %s: %w", p.method, p.path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !c.acceptStatus(p.expected, resp.StatusCode) {
		apiErr := c.decodeAPIError(p, resp)
		c.logAPIError(ctx, p, apiErr)
		return apiErr
	}

	if p.headerFn != nil {
		p.headerFn(resp.Header)
	}
	if p.dst != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(p.dst); err != nil {
			return fmt.Errorf("github: do: %s %s: decode response: %w", p.method, p.path, err)
		}
	}
	// Drain any unread bytes so http.Transport can reuse the
	// connection. Mirrors the keepclient / jira discipline.
	_, _ = io.Copy(io.Discard, resp.Body)
	c.logSuccess(ctx, p, resp.StatusCode)
	return nil
}

// acceptStatus reports whether resp.StatusCode is in p.expected. When
// p.expected is nil/empty the default success set {200, 201, 204} is
// used.
func (c *Client) acceptStatus(expected map[int]bool, status int) bool {
	if len(expected) == 0 {
		return status == http.StatusOK || status == http.StatusCreated || status == http.StatusNoContent
	}
	return expected[status]
}

// decodeAPIError builds an [*APIError] from a non-2xx response. The
// caller has NOT consumed resp.Body; this method does (capped by
// [errorBodyLimit]). GitHub envelopes outside the documented shape
// (raw 5xx from a load balancer, plain HTML 502) leave Message empty —
// Status / Method / Endpoint / RetryAfter / RateLimitRemaining are
// always populated when the source data is available.
func (c *Client) decodeAPIError(p doParams, resp *http.Response) *APIError {
	apiErr := &APIError{
		Status:             resp.StatusCode,
		Method:             p.method,
		Endpoint:           p.path,
		kind:               p.kind,
		RateLimitRemaining: parseRateLimitRemaining(resp.Header.Get("X-RateLimit-Remaining")),
	}
	// Compute RetryAfter for the documented rate-limit shapes (429 OR
	// 403-with-remaining=0). The header is `X-RateLimit-Reset` (Unix-
	// epoch seconds, NOT HTTP-date / NOT delta-seconds).
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && apiErr.RateLimitRemaining == 0) {
		apiErr.RetryAfter = parseResetHeader(resp.Header.Get("X-RateLimit-Reset"), c.cfg.clock)
	}

	if resp.Body == nil {
		return apiErr
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	if len(body) == 0 {
		return apiErr
	}
	var env struct {
		Message          string `json:"message"`
		DocumentationURL string `json:"documentation_url"`
	}
	if err := json.Unmarshal(body, &env); err == nil {
		apiErr.Message = env.Message
		apiErr.DocumentationURL = env.DocumentationURL
	}
	return apiErr
}

// parseResetHeader parses `X-RateLimit-Reset` (Unix-epoch seconds)
// and returns the duration between now and the reset time. Returns
// zero on any parse failure OR when the reset is in the past.
// Best-effort: the caller still has the sentinel + status; precise
// wait time is a hint, not a contract.
func parseResetHeader(raw string, clock func() time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || secs <= 0 {
		return 0
	}
	reset := time.Unix(secs, 0)
	now := clock()
	if reset.After(now) {
		return reset.Sub(now)
	}
	return 0
}

// parseRateLimitRemaining parses `X-RateLimit-Remaining` (integer)
// and returns its value. Returns -1 when the header was absent OR not
// parseable — the caller treats -1 as "unknown" (not "zero") to
// avoid mis-classifying a missing-header response as rate-limited.
func parseRateLimitRemaining(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return -1
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return -1
	}
	return v
}

// logSuccess emits a structured-metadata-only success entry. NEVER
// inspects the request or response body.
func (c *Client) logSuccess(ctx context.Context, p doParams, status int) {
	if c.cfg.logger == nil {
		return
	}
	c.cfg.logger.Log(
		ctx, "github request",
		"method", p.method,
		"endpoint", p.path,
		"status", status,
	)
}

// logTransportError emits a structured-metadata-only entry for a
// transport-level failure (DNS, dial, TLS, …). Only the error's
// dynamic Go TYPE name is logged (e.g. `*url.Error`, `*net.OpError`,
// `context.deadlineExceededError`) — the error's Error() string is
// NOT, because Go's net-stack errors typically embed the full URL
// (host + path), which leaks the configured base URL into log sinks.
// The original error is still returned to the caller verbatim (this
// method only governs the LOG path).
func (c *Client) logTransportError(ctx context.Context, p doParams, err error) {
	if c.cfg.logger == nil {
		return
	}
	c.cfg.logger.Log(
		ctx, "github transport error",
		"method", p.method,
		"endpoint", p.path,
		"error_kind", classifyTransportError(err),
	)
}

// classifyTransportError returns a short stable string for the
// Logger.Log "error_kind" kv. The classifier walks the error chain for
// known sentinels first, then falls back to the dynamic Go type name.
// Never returns user / tenant data.
func classifyTransportError(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline_exceeded"
	}
	return fmt.Sprintf("%T", err)
}

// logAPIError emits a structured-metadata-only entry for a GitHub-
// returned API error. The Logger receives the HTTP status code and
// the documented sentinels — but NOT the message string, because
// GitHub envelopes occasionally echo back caller-supplied path
// fragments or tenant-specific text. The full APIError (including
// Message) is still surfaced to the caller via the returned error
// chain.
func (c *Client) logAPIError(ctx context.Context, p doParams, apiErr *APIError) {
	if c.cfg.logger == nil {
		return
	}
	c.cfg.logger.Log(
		ctx, "github api error",
		"method", p.method,
		"endpoint", p.path,
		"status", apiErr.Status,
	)
}

// ownerPattern matches the GitHub owner-login shape: alphanumeric +
// hyphen, with no leading/trailing hyphen, max 39 chars (GitHub's
// documented limit). Used by [validateOwner] to reject path-
// traversal / injection attempts BEFORE the URL is composed.
var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-(?:[A-Za-z0-9])){0,38}$`)

// repoPattern matches the GitHub repository-name shape: alphanumeric +
// `_` + `-` + `.`, max 100 chars. The leading character MUST be
// alphanumeric or `_` — `.` and `-` are not accepted at position 0,
// which rejects path-traversal shapes (`.`, `..`, `..foo`, leading-
// hyphen logins) at the regex layer BEFORE the URL is composed.
// Iter-1 critic Major: a previous version permitted leading `.`/`-`
// which composed to `/repos/owner/../pulls` and risked GHES proxy
// normalisation routing to a different tenant entirely.
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]{0,99}$`)

// validateOwner rejects owners that would otherwise be concatenated
// raw into the REST path. GitHub's documented owner-login shape leaves
// no slot for `/`, `..`, `?`, `#`, or any whitespace; the regex
// enforces this synchronously. Any violation surfaces as wrapped
// [ErrInvalidArgs] BEFORE the network exchange.
func validateOwner(owner RepoOwner) error {
	s := string(owner)
	if s == "" {
		return fmt.Errorf("github: %w: owner must not be empty", ErrInvalidArgs)
	}
	if !ownerPattern.MatchString(s) {
		return fmt.Errorf("github: %w: malformed owner (expected GitHub login shape: alphanumeric + hyphen, max 39 chars)", ErrInvalidArgs)
	}
	return nil
}

// validateRepo rejects repos that would otherwise be concatenated raw
// into the REST path. GitHub's documented repo-name shape permits
// alphanumeric / underscore / hyphen / dot only; the regex enforces
// this synchronously. Any violation surfaces as wrapped
// [ErrInvalidArgs] BEFORE the network exchange.
//
// PII discipline: the wrap message intentionally does NOT echo the
// raw `repo` value — a caller passing a token-shaped string would
// otherwise leak it through the M5.6.b reflector layer.
func validateRepo(repo RepoName) error {
	s := string(repo)
	if s == "" {
		return fmt.Errorf("github: %w: repo must not be empty", ErrInvalidArgs)
	}
	if !repoPattern.MatchString(s) {
		return fmt.Errorf("github: %w: malformed repo (expected GitHub repository-name shape: alphanumeric/./-/_, max 100 chars)", ErrInvalidArgs)
	}
	return nil
}

// githubTimeFormats is the closed set of layouts the parser tries
// against the raw GitHub timestamp. GitHub's canonical shape is
// `2024-09-15T14:30:00Z` (RFC 3339 with Z suffix, no fractional);
// edge endpoints occasionally surface fractional or +00:00 offsets.
var githubTimeFormats = []string{
	time.RFC3339,
	time.RFC3339Nano,
}

// parseTime tries each [githubTimeFormats] entry in order. Returns the
// zero time on persistent parse failure — GitHub's REST API has been
// known to return non-canonical formats on edge endpoints; callers
// treat zero as "unknown / unset" rather than panicking on parse.
//
// IMPORTANT for M8.2.d callers: the zero return collides with the
// "field not present" case. Code that computes "PR is stale" against
// [PullRequest.UpdatedAt] must guard against the zero value to avoid
// mis-classifying every parse-failure PR as stale.
func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range githubTimeFormats {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// linkNextPagePattern extracts the `page=N` query parameter from a
// `Link:` response header's rel="next" entry. GitHub's Link header
// format is documented at
// https://docs.github.com/en/rest/using-the-rest-api/using-pagination-in-the-rest-api.
//
// The regex is anchored on the `rel="next"` marker. The `(?i)` flag
// makes the match case-insensitive — RFC 8288 §3 defines `rel` values
// as case-insensitive, and GHES / proxy / future GitHub.com header
// variants may capitalise the marker. Iter-1 critic Major: a previous
// version used a case-sensitive match, silently truncating pagination
// to one page if the header was emitted as `rel="Next"`.
var linkNextPagePattern = regexp.MustCompile(`(?i)<[^>]*[?&]page=(\d+)[^>]*>;\s*rel="next"`)

// relNextMarkerPattern reports whether the supplied Link header carries
// ANY `rel="next"` token (case-insensitive). Used by
// [parseLinkHeaderNextPage] to disambiguate "no next page" (legitimate
// terminal page) from "next page advertised but URL unparseable"
// (header drift — must surface as an error rather than silently
// truncate).
var relNextMarkerPattern = regexp.MustCompile(`(?i)rel="next"`)

// parseLinkHeaderNextPage returns the page number GitHub advertised
// as rel="next" in the supplied Link header. Returns:
//
//   - (0, nil) when the header is empty OR contains no rel="next"
//     marker (legitimate terminal page).
//   - (N, nil) when the header carries a rel="next" with parseable
//     page=N (non-terminal page).
//   - (0, err) when the header carries a rel="next" marker BUT no
//     parseable page= parameter — header drift / GHES proxy
//     rewriting / format change that, untreated, would silently
//     truncate the scan after the current page. Iter-1 codex Major:
//     fail-loud rather than fail-open.
func parseLinkHeaderNextPage(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	if !relNextMarkerPattern.MatchString(raw) {
		return 0, nil
	}
	m := linkNextPagePattern.FindStringSubmatch(raw)
	if len(m) < 2 {
		return 0, fmt.Errorf("github: parseLinkHeaderNextPage: %w: rel=\"next\" marker present but no parseable page= param in Link header", ErrInvalidArgs)
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("github: parseLinkHeaderNextPage: %w: rel=\"next\" page= value unparseable", ErrInvalidArgs)
	}
	return v, nil
}
