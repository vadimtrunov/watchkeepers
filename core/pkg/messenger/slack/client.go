package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultBaseURL is the production Slack Web API endpoint. The
// per-method URL is `<baseURL>/<method>` (Slack's URL scheme is flat:
// every method is a top-level child of `slack.com/api`).
const defaultBaseURL = "https://slack.com/api"

// defaultHTTPTimeout is the timeout applied to the [Client]'s default
// *http.Client when the caller does not supply one via [WithHTTPClient].
// 30 seconds matches Slack's documented soft timeout for `chat.postMessage`
// with content unfurls; callers with stricter budgets pass a tuned client.
const defaultHTTPTimeout = 30 * time.Second

// errorBodyLimit caps the bytes read from a non-2xx response body so
// a pathological server cannot force unbounded allocation. 4 KiB is
// plenty for a JSON envelope plus diagnostics and matches the
// keepclient discipline (LESSON M2.7.b+c).
const errorBodyLimit = 1 << 12

// userAgent is the static identification string the [Client] sends on
// every request. Slack's TOS recommends a stable UA so they can
// correlate cross-method traffic to a single integration.
const userAgent = "watchkeepers-slack/0.1 (+https://github.com/vadimtrunov/watchkeepers)"

// TokenSource resolves a bearer token for a single Slack API call.
// The shape mirrors keepclient.TokenSource — callers wrapping a
// rotating secret store (M3.4.b secrets interface) can drive
// per-request refresh transparently.
//
// Implementations MUST be safe for concurrent use across goroutines.
type TokenSource interface {
	// Token returns the bearer token for the supplied ctx. Errors
	// surface as wrapped failures from [Client.Do] (the request is
	// never sent on a token-resolution failure — security invariant
	// matches keepclient).
	Token(ctx context.Context) (string, error)
}

// StaticToken is the trivial [TokenSource] that always returns the
// same string. Useful in tests and for bootstrapping; production
// wiring should use a [TokenSource] backed by the secrets interface
// so token rotation is observable.
type StaticToken string

// Token satisfies [TokenSource] for [StaticToken].
func (s StaticToken) Token(context.Context) (string, error) {
	return string(s), nil
}

// Logger is the optional diagnostic sink wired in via [WithLogger].
// The shape mirrors the keepclient / keeperslog / outbox Logger
// interfaces: a single `Log(ctx, msg, kv...)` so callers can
// substitute structured loggers (slog, zap wrapper, etc.) without
// losing type compatibility.
//
// IMPORTANT (redaction discipline): the [Client] NEVER passes the
// bearer token, the request body, or the Slack response body through
// the logger. Only structured metadata (method name, HTTP status,
// error type, Retry-After value) appears in log entries. Mirrors the
// M3.4.b / M3.5 / M3.6 / M3.7 redaction patterns documented in
// `docs/LESSONS.md`.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// Client is the low-level Slack Web API HTTP client. Construct via
// [NewClient]. The zero value is not usable.
//
// Public surface in M4.2.a is intentionally narrow: [Client.Do] is
// the single building block M4.2.b/c/d compose into adapter methods.
// Higher-level operations (`SendMessage`, `Subscribe`, `CreateApp`,
// …) land in those follow-up sub-bullets.
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
	baseURL     *url.URL
	httpClient  *http.Client
	tokenSource TokenSource
	rateLimiter *RateLimiter
	logger      Logger
	clock       func() time.Time

	// socketHelloTimeout caps the wait for the Socket Mode `hello`
	// envelope after dialling the WSS URL. Configured via
	// [WithSocketModeHelloTimeout]; zero falls back to the package
	// default ([defaultHelloTimeout]).
	socketHelloTimeout time.Duration

	// socketDialer overrides the WSS dial step. Configured via
	// [WithSocketModeDialer]; nil falls back to coder/websocket's
	// [websocket.Dial]. Tests substitute an in-process pair so the
	// happy-path runs without a real handshake.
	socketDialer socketModeDialer
}

// ClientOption configures a [Client] at construction time. Pass
// options to [NewClient]; later options override earlier ones for
// the same field.
type ClientOption func(*clientConfig)

// WithBaseURL overrides the default Slack base URL
// (`https://slack.com/api`). Useful for tests pointing at a
// `httptest.Server` and for self-hosted Slack-compatible APIs.
//
// The supplied string must be non-empty and parseable by
// [net/url.Parse]; on either failure WithBaseURL panics with a clear
// message — matches the panic discipline of [keepclient.WithBaseURL]
// (programmer error, no error return on the constructor).
func WithBaseURL(raw string) ClientOption {
	if raw == "" {
		panic("slack: WithBaseURL: base URL must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		panic(fmt.Sprintf("slack: WithBaseURL: %v", err))
	}
	if u.Scheme == "" || u.Host == "" {
		panic(fmt.Sprintf("slack: WithBaseURL: %q is missing scheme or host", raw))
	}
	return func(c *clientConfig) { c.baseURL = u }
}

// WithHTTPClient overrides the default *http.Client. Pass a tuned
// client to adjust the request timeout or transport. A nil argument
// is ignored so callers can apply a conditional override without
// explicit branching.
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *clientConfig) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithTokenSource registers the [TokenSource] consulted for every
// API call. A nil source leaves the field unset (calls to
// [Client.Do] then fail with a wrapped sentinel before any network
// round-trip — security invariant: never emit a request with a
// stale or zero-value token).
func WithTokenSource(ts TokenSource) ClientOption {
	return func(c *clientConfig) { c.tokenSource = ts }
}

// WithRateLimiter wires a [RateLimiter] onto the returned [Client].
// When set, every [Client.Do] call routes through
// [RateLimiter.Wait] before issuing the HTTP request. A nil limiter
// is ignored so callers can apply a conditional override without
// explicit branching; a [Client] without a limiter sends requests
// at the rate dictated by the caller (suitable for unit tests, NOT
// production wiring against the real Slack Web API).
func WithRateLimiter(rl *RateLimiter) ClientOption {
	return func(c *clientConfig) {
		if rl != nil {
			c.rateLimiter = rl
		}
	}
}

// WithLogger overrides the default no-op [Logger]. The hook is called
// from [Client.Do] at request begin and end with stable kv pairs
// (`method`, `status`, `err_type`, `retry_after`). A nil argument is
// ignored so callers can apply a conditional override without
// explicit branching.
//
// IMPORTANT: log entries NEVER carry the bearer token, the request
// body, or the response body. Only structured metadata appears.
func WithLogger(l Logger) ClientOption {
	return func(c *clientConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithClock overrides the wall-clock function the client uses (only
// relevant for `Retry-After: <HTTP-date>` parsing relative to "now").
// Defaults to [time.Now]. A nil function is a no-op so callers can
// always pass through whatever they have.
func WithClock(c func() time.Time) ClientOption {
	return func(cfg *clientConfig) {
		if c != nil {
			cfg.clock = c
		}
	}
}

// NewClient returns a configured [Client]. The defaults are:
//
//   - baseURL = `https://slack.com/api`
//   - httpClient = `&http.Client{Timeout: 30s}`
//   - tokenSource = nil (calls to [Client.Do] then fail synchronously)
//   - rateLimiter = nil (no throttling at the client layer; suitable
//     only for tests — production callers wire a [RateLimiter] in)
//   - logger = no-op
//   - clock = [time.Now]
//
// Supplied options override the defaults.
func NewClient(opts ...ClientOption) *Client {
	cfg := clientConfig{
		httpClient: &http.Client{Timeout: defaultHTTPTimeout},
		logger:     noopLogger{},
		clock:      time.Now,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.baseURL == nil {
		u, _ := url.Parse(defaultBaseURL)
		cfg.baseURL = u
	}
	return &Client{cfg: cfg}
}

// slackEnvelope is the shared shape every Slack Web API JSON response
// matches: a top-level `ok` boolean plus an optional `error` code.
// We decode just these two fields so the per-call response struct can
// reuse the rest verbatim. Slack also emits `response_metadata`,
// `warning`, etc.; those land on the per-call struct, not here.
type slackEnvelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Do issues a POST against `<baseURL>/<method>` with `params`
// JSON-encoded as the request body, and decodes a successful
// response (`{"ok": true, ...}`) into `out` (when non-nil).
//
// Resolution order:
//
//  1. Validate method != "" → otherwise [ErrUnknownMethod].
//  2. Validate that a [TokenSource] is configured → otherwise
//     [ErrInvalidAuth] (no network round-trip).
//  3. Wait on the configured [RateLimiter] for the method's tier (if
//     any limiter is wired in). Honours ctx cancellation.
//  4. Resolve the bearer token via [TokenSource.Token]. A
//     resolution error is wrapped and the request is NOT sent.
//  5. Build and send a JSON POST with `Authorization: Bearer ...`.
//  6. On HTTP 429, parse `Retry-After` (integer-seconds OR HTTP-date
//     per RFC 9110 §10.2.3) and return [*APIError] wrapping
//     [ErrRateLimited].
//  7. On other non-2xx, return [*APIError] with empty Code.
//  8. On 2xx, decode the body as a Slack envelope; when `ok: false`
//     return [*APIError] whose Code is the `error` field; otherwise
//     re-decode into `out` (if non-nil) and return nil.
//
// Slack's `error` field is documented at
// https://api.slack.com/web#methods-evaluation; common codes that
// surface via [APIError.Unwrap] sentinels are listed in `errors.go`.
func (c *Client) Do(ctx context.Context, method string, params any, out any) error {
	if err := c.validateAndWait(ctx, method); err != nil {
		return err
	}

	body, err := encodeBody(params)
	if err != nil {
		return err
	}

	tok, err := c.cfg.tokenSource.Token(ctx)
	if err != nil {
		c.cfg.logger.Log(
			ctx, "slack: token resolve failed",
			"method", method,
			"err_type", fmt.Sprintf("%T", err),
		)
		return fmt.Errorf("slack: token: %w", err)
	}

	endpoint := c.endpointFor(method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("slack: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	c.cfg.logger.Log(ctx, "slack: request begin", "method", method)

	resp, err := c.cfg.httpClient.Do(req)
	if err != nil {
		c.cfg.logger.Log(
			ctx, "slack: request failed",
			"method", method,
			"err_type", fmt.Sprintf("%T", err),
		)
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	return c.decodeResponse(ctx, method, resp, out)
}

// validateAndWait runs the synchronous-validation + rate-limit-wait
// preamble for [Client.Do]. Split out so the request-encoding path is
// only entered when we know we're going to send.
func (c *Client) validateAndWait(ctx context.Context, method string) error {
	if method == "" {
		return ErrUnknownMethod
	}
	if c.cfg.tokenSource == nil {
		return fmt.Errorf("slack: %s: %w", method, ErrInvalidAuth)
	}
	if c.cfg.rateLimiter != nil {
		if err := c.cfg.rateLimiter.Wait(ctx, method); err != nil {
			return fmt.Errorf("slack: %s: %w", method, err)
		}
	}
	return ctx.Err()
}

// endpointFor returns the absolute URL for `<baseURL>/<method>`.
// Slack's URL scheme is flat (every method is a top-level child of
// `slack.com/api`) so we just concatenate. The base URL is guaranteed
// non-nil after [NewClient].
func (c *Client) endpointFor(method string) string {
	base := c.cfg.baseURL.String()
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + method
}

// decodeResponse routes a single HTTP response through the
// status-classification logic, populating `out` on success and
// returning an [*APIError] on failure. Caller defers the body close.
func (c *Client) decodeResponse(ctx context.Context, method string, resp *http.Response, out any) error {
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return c.handle429(ctx, method, resp)
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return c.handle2xx(ctx, method, resp, out)
	default:
		return c.handleNon2xx(ctx, method, resp)
	}
}

// handle2xx parses a successful response: read the body once, decode
// the envelope to check `ok`, then re-decode into `out` on success.
func (c *Client) handle2xx(ctx context.Context, method string, resp *http.Response, out any) error {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("slack: %s: read body: %w", method, err)
	}
	var env slackEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("slack: %s: decode envelope: %w", method, err)
	}
	if !env.OK {
		apiErr := &APIError{
			Status: resp.StatusCode,
			Code:   env.Error,
			Method: method,
		}
		c.cfg.logger.Log(
			ctx, "slack: api error",
			"method", method,
			"status", resp.StatusCode,
			"code", env.Error,
		)
		return apiErr
	}
	c.cfg.logger.Log(
		ctx, "slack: request ok",
		"method", method,
		"status", resp.StatusCode,
	)
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("slack: %s: decode response: %w", method, err)
	}
	return nil
}

// handle429 builds an [*APIError] for a Too-Many-Requests response,
// parsing `Retry-After` per RFC 9110 §10.2.3 (integer seconds OR
// HTTP-date). The body is drained but its content is discarded —
// Slack's 429 body is not a JSON envelope.
func (c *Client) handle429(ctx context.Context, method string, resp *http.Response) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyLimit))
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), c.cfg.clock())
	c.cfg.logger.Log(
		ctx, "slack: rate limited",
		"method", method,
		"status", resp.StatusCode,
		"retry_after_seconds", int(retryAfter/time.Second),
	)
	return &APIError{
		Status:     resp.StatusCode,
		Method:     method,
		RetryAfter: retryAfter,
	}
}

// handleNon2xx builds an [*APIError] for an unexpected non-2xx that
// is not 429. The body is read up to errorBodyLimit so failures with
// a JSON envelope still carry a Code; non-JSON bodies (raw 5xx from a
// load balancer) leave Code empty.
func (c *Client) handleNon2xx(ctx context.Context, method string, resp *http.Response) error {
	apiErr := &APIError{Status: resp.StatusCode, Method: method}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	if err == nil && len(raw) > 0 {
		var env slackEnvelope
		if jerr := json.Unmarshal(raw, &env); jerr == nil && env.Error != "" {
			apiErr.Code = env.Error
		}
	}
	c.cfg.logger.Log(
		ctx, "slack: http error",
		"method", method,
		"status", resp.StatusCode,
		"code", apiErr.Code,
	)
	return apiErr
}

// encodeBody marshals `params` as JSON. Nil/empty params produce an
// empty `{}` body so Slack's strict Content-Type check accepts the
// request unchanged.
func encodeBody(params any) (io.Reader, error) {
	if params == nil {
		return bytes.NewReader([]byte(`{}`)), nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("slack: marshal params: %w", err)
	}
	return bytes.NewReader(raw), nil
}

// parseRetryAfter decodes the `Retry-After` header per RFC 9110
// §10.2.3. The header value can be either:
//
//   - A non-negative integer of seconds (e.g. `Retry-After: 30`).
//   - An HTTP-date (e.g. `Retry-After: Wed, 21 Oct 2026 07:28:00 GMT`).
//
// `now` is the current wall-clock time used to convert an HTTP-date
// into a duration. Returns 0 for an empty / malformed header so the
// caller can decide on a sensible default rather than carrying NaN.
func parseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if n, err := strconv.Atoi(raw); err == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		d := t.Sub(now)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

// noopLogger is the default [Logger] used when the caller doesn't
// supply one via [WithLogger]. The receiver discards every entry.
type noopLogger struct{}

// Log satisfies [Logger] for [noopLogger].
func (noopLogger) Log(context.Context, string, ...any) {}
