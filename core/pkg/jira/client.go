package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// defaultHTTPTimeout is the timeout applied to the [Client]'s default
// *http.Client when the caller does not supply one via [WithHTTPClient].
// 30 seconds matches the slack-package precedent and is loose enough
// for `/search/jql` calls returning hundreds of issues.
const defaultHTTPTimeout = 30 * time.Second

// errorBodyLimit caps the bytes read from a non-2xx response body so
// a pathological server cannot force unbounded allocation. 4 KiB is
// plenty for an Atlassian envelope plus diagnostics and matches the
// keepclient / slack discipline.
const errorBodyLimit = 1 << 12

// userAgent is the static identification string the [Client] sends on
// every request. Atlassian's TOS recommends a stable UA so they can
// correlate cross-method traffic to a single integration.
const userAgent = "watchkeepers-jira/0.1 (+https://github.com/vadimtrunov/watchkeepers)"

// BasicAuthSource resolves the email + Atlassian-API-token pair for a
// single REST call. The shape mirrors slack.TokenSource — callers
// wrapping a rotating secret store (M3.4.b secrets interface) can
// drive per-request refresh transparently.
//
// Implementations MUST be safe for concurrent use across goroutines.
//
// Atlassian Cloud's documented HTTP basic-auth scheme is
// `Basic base64(<email>:<api-token>)`. The token is the user-scoped
// API token created in https://id.atlassian.com/manage-profile/security/api-tokens
// (NOT the password); the email is the account's primary email.
type BasicAuthSource interface {
	// BasicAuth returns the (email, token) pair for the supplied ctx.
	// Errors surface as wrapped failures from [Client.Do] (the request
	// is never sent on a credential-resolution failure — security
	// invariant matches the slack adapter).
	BasicAuth(ctx context.Context) (email, token string, err error)
}

// StaticBasicAuth is the trivial [BasicAuthSource] that always returns
// the same email/token pair. Useful in tests and for bootstrapping;
// production wiring should use a [BasicAuthSource] backed by the
// secrets interface so token rotation is observable.
type StaticBasicAuth struct {
	Email string
	Token string
}

// BasicAuth satisfies [BasicAuthSource] for [StaticBasicAuth].
func (s StaticBasicAuth) BasicAuth(context.Context) (string, string, error) {
	return s.Email, s.Token, nil
}

// Logger is the optional diagnostic sink wired in via [WithLogger].
// The shape mirrors the slack / keepclient / keeperslog Logger
// interfaces: a single `Log(ctx, msg, kv...)` so callers can substitute
// structured loggers (slog, zap wrapper, …) without losing type
// compatibility.
//
// IMPORTANT (redaction discipline): the [Client] NEVER passes the email,
// the API token, the Authorization header, the request body, or the
// Atlassian response body through the logger. Only structured metadata
// (HTTP method, endpoint path, status, error class, Retry-After value)
// appears in log entries. Mirrors the M3.4.b / M3.5 / M3.6 / M3.7
// redaction patterns documented in `docs/LESSONS.md`.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// Client is the low-level Atlassian REST API HTTP client. Construct
// via [NewClient]. The zero value is not usable.
//
// The four high-level operations M8.1 ships ([Client.Search],
// [Client.GetIssue], [Client.AddComment], [Client.UpdateFields]) all
// compose through [Client.Do], which carries the auth, encoding,
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
	auth       BasicAuthSource
	logger     Logger
	clock      func() time.Time

	// baseURLInvalid is the flag [WithBaseURL] flips when it parses
	// or validates a non-conforming string ([NewClient] surfaces the
	// flag as wrapped [ErrInvalidBaseURL], distinguishing "supplied
	// wrongly" from "not supplied at all" → wrapped
	// [ErrMissingBaseURL]).
	baseURLInvalid bool

	// fieldWhitelist is the closed set of field IDs the
	// [Client.UpdateFields] method is permitted to write. A nil OR
	// empty set means NO field is permitted — fail-closed default.
	// Atlassian field IDs are short strings (`summary`,
	// `description`, `customfield_10001`); the whitelist is keyed by
	// the same strings the caller passes to [Client.UpdateFields].
	fieldWhitelist map[string]struct{}
}

// NewClient constructs a [Client] from the supplied options. The
// invariants checked synchronously:
//
//   - [WithBaseURL] MUST have been supplied (Atlassian Cloud has no
//     canonical hostname); otherwise returns wrapped [ErrMissingBaseURL].
//   - [WithBasicAuth] MUST have been supplied; otherwise returns wrapped
//     [ErrMissingAuth].
//
// Optional configuration (httpClient, logger, fieldWhitelist, clock)
// falls back to safe defaults. The whitelist default is the empty set
// (writes refused) — callers that intend to drive [Client.UpdateFields]
// MUST supply [WithFieldWhitelist].
func NewClient(opts ...ClientOption) (*Client, error) {
	cfg := clientConfig{
		clock: time.Now,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.baseURLInvalid {
		return nil, fmt.Errorf("jira: NewClient: %w", ErrInvalidBaseURL)
	}
	if cfg.baseURL == nil {
		return nil, fmt.Errorf("jira: NewClient: %w", ErrMissingBaseURL)
	}
	if cfg.auth == nil {
		return nil, fmt.Errorf("jira: NewClient: %w", ErrMissingAuth)
	}
	if cfg.httpClient == nil {
		cfg.httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{cfg: cfg}, nil
}

// doParams is the internal call descriptor passed to [Client.do]. The
// dispatching method (Search, GetIssue, …) populates it; the lower-
// level transport handles auth + encoding + envelope decoding.
type doParams struct {
	method   string
	path     string
	query    url.Values
	body     any          // when non-nil, JSON-encoded as the request body
	dst      any          // when non-nil, JSON-decoded from the success response body
	kind     endpointKind // disambiguates 404 / 400 mapping in [APIError.Unwrap]
	expected map[int]bool // optional set of acceptable success statuses; defaults to {200, 201, 204}
}

// do executes a single REST call against the configured base URL with
// auth, JSON encoding, response-envelope decoding, and structured-
// metadata-only logging. Callers (Search, GetIssue, …) supply a
// [doParams]; do never inspects the success body except to populate
// [doParams.dst] (or skip when dst is nil).
//
// Failure paths:
//
//   - [BasicAuthSource] error → wrapped, no HTTP request sent.
//   - [http.Client.Do] transport error → wrapped, no envelope.
//   - non-2xx response → [*APIError] populated from the envelope (when
//     parseable) or with empty Messages otherwise; status / endpoint /
//     method / kind / Retry-After populated.
//
// The Authorization header carries `Basic base64(email:token)` per
// Atlassian's documented Cloud auth scheme.
func (c *Client) do(ctx context.Context, p doParams) error {
	if p.method == "" {
		return fmt.Errorf("jira: do: %w: empty method", ErrInvalidArgs)
	}
	if p.path == "" {
		return fmt.Errorf("jira: do: %w: empty path", ErrInvalidArgs)
	}

	email, token, err := c.cfg.auth.BasicAuth(ctx)
	if err != nil {
		return fmt.Errorf("jira: do: resolve credentials: %w", err)
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
			return fmt.Errorf("jira: do: encode body: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, p.method, u.String(), bodyReader)
	if err != nil {
		return fmt.Errorf("jira: do: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if p.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth(email, token)

	resp, err := c.cfg.httpClient.Do(req)
	if err != nil {
		c.logTransportError(ctx, p, err)
		return fmt.Errorf("jira: do: %s %s: %w", p.method, p.path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !c.acceptStatus(p.expected, resp.StatusCode) {
		apiErr := c.decodeAPIError(p, resp)
		c.logAPIError(ctx, p, apiErr)
		return apiErr
	}

	if p.dst != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(p.dst); err != nil {
			return fmt.Errorf("jira: do: %s %s: decode response: %w", p.method, p.path, err)
		}
	}
	// Drain any unread bytes so http.Transport can reuse the
	// connection. Mirrors the keepclient discipline (M2.8.a). Without
	// this, a 2xx body the decoder did not fully consume (or any 2xx
	// where p.dst is nil but the server sent a body anyway) tears
	// down the keep-alive on Body.Close.
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
// [errorBodyLimit]). Atlassian envelopes outside the documented shape
// (raw 5xx from a load balancer, plain HTML 502) leave Messages /
// FieldErrors empty — Status / Method / Endpoint / RetryAfter are
// always populated.
func (c *Client) decodeAPIError(p doParams, resp *http.Response) *APIError {
	apiErr := &APIError{
		Status:   resp.StatusCode,
		Method:   p.method,
		Endpoint: p.path,
		kind:     p.kind,
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		apiErr.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), c.cfg.clock)
	}

	if resp.Body == nil {
		return apiErr
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	if len(body) == 0 {
		return apiErr
	}
	var env struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err == nil {
		apiErr.Messages = env.ErrorMessages
		apiErr.FieldErrors = env.Errors
	}
	return apiErr
}

// parseRetryAfter parses the `Retry-After` header per RFC 9110
// §10.2.3 — both integer-second and HTTP-date forms. Returns zero on
// any parse failure (best-effort: the caller still has the sentinel +
// status; precise wait time is a hint, not a contract).
func parseRetryAfter(raw string, clock func() time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		now := clock()
		if t.After(now) {
			return t.Sub(now)
		}
	}
	return 0
}

// logSuccess emits a structured-metadata-only success entry. NEVER
// inspects the request or response body.
func (c *Client) logSuccess(ctx context.Context, p doParams, status int) {
	if c.cfg.logger == nil {
		return
	}
	c.cfg.logger.Log(
		ctx, "jira request",
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
// (host + path), which leaks the Atlassian tenant subdomain into log
// sinks. The original error is still returned to the caller verbatim
// (this method only governs the LOG path).
func (c *Client) logTransportError(ctx context.Context, p doParams, err error) {
	if c.cfg.logger == nil {
		return
	}
	c.cfg.logger.Log(
		ctx, "jira transport error",
		"method", p.method,
		"endpoint", p.path,
		"error_kind", classifyTransportError(err),
	)
}

// classifyTransportError returns a short stable string for the
// Logger.Log "error_kind" kv. The classifier walks the error chain
// for known sentinels first, then falls back to the dynamic Go type
// name. Never returns user / tenant data.
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

// logAPIError emits a structured-metadata-only entry for an Atlassian-
// returned API error. The Logger receives the HTTP status code and
// the count of Atlassian envelope messages — but NOT the message
// strings themselves, because Atlassian envelopes can echo
// user-supplied JQL fragments, custom-field names, or other tenant-
// specific text. The full APIError (including Messages) is still
// surfaced to the caller via the returned error chain.
func (c *Client) logAPIError(ctx context.Context, p doParams, apiErr *APIError) {
	if c.cfg.logger == nil {
		return
	}
	c.cfg.logger.Log(
		ctx, "jira api error",
		"method", p.method,
		"endpoint", p.path,
		"status", apiErr.Status,
		"messages_count", len(apiErr.Messages),
	)
}

// errMustNotBeEmpty is a tiny helper for argument validation that keeps
// the doParams-level call sites uniform. Returns wrapped
// [ErrInvalidArgs] when v is the typed zero value.
func errMustNotBeEmpty(label, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("jira: %s: %w: must not be empty", label, ErrInvalidArgs)
	}
	return nil
}

// issueKeyPattern matches the Atlassian project-key + issue-number
// shape: project key is one uppercase letter followed by ≥1
// uppercase-letter / digit / underscore (Atlassian permits all three
// in custom project keys), separated from a positive issue number
// by a single hyphen. Used by [validateIssueKey] to reject path-
// traversal / injection attempts BEFORE the URL is composed.
var issueKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]+-[1-9][0-9]*$`)

// validateIssueKey rejects keys that would otherwise be concatenated
// raw into the REST path. Atlassian's documented key shape (project
// prefix + numeric tail) leaves no slot for `/`, `..`, `?`, `#`, or
// any whitespace; the regex enforces this synchronously. Any
// violation surfaces as wrapped [ErrInvalidArgs] BEFORE the network
// exchange, matching the M8.1 transport-layer security boundary
// discipline.
func validateIssueKey(key IssueKey) error {
	s := string(key)
	if s == "" {
		return fmt.Errorf("jira: %w: issue key must not be empty", ErrInvalidArgs)
	}
	if !issueKeyPattern.MatchString(s) {
		return fmt.Errorf("jira: %w: malformed issue key %q (expected [A-Z][A-Z0-9_]+-[1-9][0-9]*)", ErrInvalidArgs, s)
	}
	return nil
}

// jiraTimeFormats is the closed set of layouts the parser tries
// against the raw Atlassian timestamp BEFORE falling back to a
// fractional-strip retry (see [parseTime]). Atlassian's canonical
// shape is `2024-09-15T14:30:00.000+0000` (3-digit fractional, no
// colon in the offset); callers occasionally see the variants
// without fractional, with a `Z` suffix, and with an RFC 3339-style
// `+00:00` offset.
var jiraTimeFormats = []string{
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05-0700",
	time.RFC3339Nano,
	time.RFC3339,
}

// jiraTimeFormatsNoFraction is the layout list [parseTime] retries
// against AFTER stripping the fractional-second run. Used when
// Atlassian emits a non-3-digit fractional that none of the
// canonical layouts match (e.g. 6 or 9 digits, an Atlassian bug
// observed on edge endpoints). The strip-and-retry path keeps the
// returned time accurate to the second; sub-second precision is
// not load-bearing for any M8.x consumer.
var jiraTimeFormatsNoFraction = []string{
	"2006-01-02T15:04:05-0700",
	time.RFC3339,
}

// parseTime tries each [jiraTimeFormats] entry in order; on failure,
// strips the fractional-second run and retries against
// [jiraTimeFormatsNoFraction]. Returns the zero time on persistent
// parse failure — Atlassian's REST API has been known to return
// non-canonical formats on edge endpoints; callers treat zero as
// "unknown / unset" rather than panicking on parse.
//
// IMPORTANT for M8.x callers: the zero return collides with the
// "field not requested" and "server-omitted" cases. Code that
// computes "issue is overdue" against [Issue.Updated] must guard
// against the zero value to avoid mis-classifying every
// parse-failure issue as overdue. See [Issue] doc for the explicit
// collision contract.
func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range jiraTimeFormats {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	if i := strings.Index(raw, "."); i > 0 {
		j := i + 1
		for j < len(raw) && raw[j] >= '0' && raw[j] <= '9' {
			j++
		}
		stripped := raw[:i] + raw[j:]
		for _, layout := range jiraTimeFormatsNoFraction {
			if t, err := time.Parse(layout, stripped); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

// fieldWhitelistContains reports whether the configured whitelist
// contains every key in fields. When at least one key is absent, the
// returned error wraps [ErrFieldNotWhitelisted] and names every
// offending key (sorted alphabetically for deterministic operator
// debugging — Go map iteration is non-deterministic, so naming "the
// first" offender would surface different keys on different runs).
// Field NAMES (e.g. `summary`, `customfield_10001`) are
// configuration-class data and safe to surface in errors; the
// proposed values (which may carry user-supplied text) are NOT
// included in the error message.
func (c *Client) fieldWhitelistContains(fields map[string]any) error {
	if len(fields) == 0 {
		return fmt.Errorf("jira: %w: no fields supplied", ErrInvalidArgs)
	}
	var offenders []string
	for k := range fields {
		if _, ok := c.cfg.fieldWhitelist[k]; !ok {
			offenders = append(offenders, k)
		}
	}
	if len(offenders) == 0 {
		return nil
	}
	sort.Strings(offenders)
	return fmt.Errorf("jira: %w: fields not in whitelist: %s", ErrFieldNotWhitelisted, strings.Join(offenders, ", "))
}
