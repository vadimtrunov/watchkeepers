package keepclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// authPathPrefix is the literal prefix on `path` (the do() argument) that
// triggers token injection. The check is on the unjoined path string so
// callers cannot bypass it via base-URL trickery (e.g. a base URL of
// `http://x/v1/` plus a path of `health`).
const authPathPrefix = "/v1/"

// errorBodyLimit caps the bytes read from a non-2xx response body so a
// pathological server cannot force unbounded allocation. 1 KiB is plenty
// for a JSON error envelope and matches the input-bounding discipline the
// Keep server uses on the request side (LESSON M2.7.b+c).
const errorBodyLimit = 1 << 10

// errorEnvelope mirrors the Keep server's `{"error":"<code>","reason":"<r>"}`
// shape (handlers_read.go writeError / writeErrorReason and middleware.go
// writeAuthError). Decoded best-effort: a non-JSON body falls back to raw.
type errorEnvelope struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

// Health calls GET /health on the configured base URL. The endpoint is open
// (no token), expects 200 + `{"status":"ok"}`, and returns nil on success.
// Transport errors are wrapped with %w; server errors surface as
// [*ServerError] (whose Unwrap chain matches the Err* sentinels).
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/health", nil, nil)
}

// do is the internal request helper shared by every endpoint method.
//
// Behavior:
//   - URL is built by ResolveReference(base, path) so trailing-slash
//     differences in the base URL cannot produce double slashes.
//   - When body is non-nil it is JSON-marshalled and Content-Type is set
//     to application/json.
//   - Authorization is injected only when path starts with /v1/. The check
//     is on the unjoined path string so a misconfigured base URL cannot
//     bypass it. When no TokenSource is configured, do returns
//     ErrNoTokenSource synchronously — no network round-trip.
//   - When the TokenSource itself errors, that error is wrapped with %w
//     and the request is NOT sent (security invariant: never emit a
//     request with a stale or zero-value token).
//   - On 2xx, when out is non-nil, the response body is JSON-decoded into
//     out. A decode failure surfaces as a wrapped error (NOT a
//     *ServerError — the HTTP exchange itself succeeded).
//   - On non-2xx, do reads the body (capped to errorBodyLimit), best-effort
//     decodes it as the {"error","reason"} envelope, and returns a
//     *ServerError. Failed decode falls back to Code="" / Reason=raw.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	c.cfg.logger(ctx, "keepclient.do begin", "method", method, "path", path)

	requiresAuth := strings.HasPrefix(path, authPathPrefix)
	if requiresAuth && c.cfg.tokenSource == nil {
		c.cfg.logger(ctx, "keepclient.do end", "status", 0, "err", ErrNoTokenSource)
		return ErrNoTokenSource
	}

	endpoint, err := joinURL(c.cfg.baseURL, path)
	if err != nil {
		c.cfg.logger(ctx, "keepclient.do end", "status", 0, "err", err)
		return fmt.Errorf("keepclient: join url: %w", err)
	}

	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.cfg.logger(ctx, "keepclient.do end", "status", 0, "err", err)
			return fmt.Errorf("keepclient: marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		c.cfg.logger(ctx, "keepclient.do end", "status", 0, "err", err)
		return fmt.Errorf("keepclient: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if requiresAuth {
		// Resolve the token BEFORE issuing the request so a refresh failure
		// never becomes a stale-token request.
		tok, err := c.cfg.tokenSource.Token(ctx)
		if err != nil {
			c.cfg.logger(ctx, "keepclient.do end", "status", 0, "err", err)
			return fmt.Errorf("keepclient: token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.cfg.httpClient.Do(req)
	if err != nil {
		c.cfg.logger(ctx, "keepclient.do end", "status", 0, "err", err)
		return fmt.Errorf("keepclient: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		c.cfg.logger(ctx, "keepclient.do end", "status", resp.StatusCode, "err", nil)
		if out == nil {
			// Drain so the connection can be reused. Ignore errors — the
			// response was successful from the caller's perspective.
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("keepclient: decode response: %w", err)
		}
		return nil
	}

	se := parseServerError(resp)
	c.cfg.logger(ctx, "keepclient.do end", "status", resp.StatusCode, "err", se)
	return se
}

// joinURL resolves path against base using net/url.ResolveReference so that
// trailing-slash variations in base do not produce double slashes. base must
// be non-nil; callers (NewClient) enforce this via WithBaseURL's panic.
func joinURL(base *url.URL, path string) (string, error) {
	if base == nil {
		return "", errors.New("base URL is nil")
	}
	// ResolveReference with a leading-slash path replaces the base path
	// entirely; with a relative path it joins to the directory portion of
	// the base. Strip a leading slash from the path so both base shapes
	// ("http://x" and "http://x/") behave identically.
	rel, err := url.Parse(strings.TrimPrefix(path, "/"))
	if err != nil {
		return "", err
	}
	// Ensure the base has a trailing slash so ResolveReference treats it as
	// a directory, otherwise relative paths would replace the last segment.
	b := *base
	if !strings.HasSuffix(b.Path, "/") {
		b.Path += "/"
	}
	return b.ResolveReference(rel).String(), nil
}

// parseServerError builds a *ServerError from a non-2xx response. It reads
// the body up to errorBodyLimit, attempts to decode the
// {"error":"<code>","reason":"<reason>"} envelope, and falls back to a raw
// truncated body when JSON decode fails (or when the body is empty).
func parseServerError(resp *http.Response) *ServerError {
	se := &ServerError{Status: resp.StatusCode}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	if err != nil || len(raw) == 0 {
		return se
	}
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && (env.Error != "" || env.Reason != "") {
		se.Code = env.Error
		se.Reason = env.Reason
		return se
	}
	se.Reason = string(raw)
	return se
}
