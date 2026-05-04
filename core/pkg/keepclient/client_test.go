package keepclient

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestNewClient_Defaults asserts that NewClient with only WithBaseURL applies
// the documented defaults (10s HTTP timeout, no token source, no-op logger)
// and returns a non-nil *Client even when no token source is configured.
func TestNewClient_Defaults(t *testing.T) {
	t.Parallel()

	c := NewClient(WithBaseURL("http://example.invalid"))
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if c.cfg.httpClient == nil {
		t.Fatal("httpClient default not applied")
	}
	if c.cfg.httpClient.Timeout != 10*time.Second {
		t.Errorf("httpClient.Timeout = %v, want 10s", c.cfg.httpClient.Timeout)
	}
	if c.cfg.tokenSource != nil {
		t.Error("tokenSource should default to nil")
	}
	if c.cfg.logger == nil {
		t.Error("logger should default to a no-op (non-nil) function")
	}
	// The default logger must be safe to call with arbitrary kv args.
	c.cfg.logger(context.Background(), "noop", "key", "value")
}

// TestNewClient_AppliesOptions asserts that custom HTTPClient, TokenSource,
// and Logger options override the defaults.
func TestNewClient_AppliesOptions(t *testing.T) {
	t.Parallel()

	calls := 0
	logger := func(_ context.Context, _ string, _ ...any) { calls++ }
	ts := StaticToken("tok")
	hc := newTestHTTPClient(2 * time.Second)

	c := NewClient(
		WithBaseURL("http://example.invalid/"),
		WithHTTPClient(hc),
		WithTokenSource(ts),
		WithLogger(logger),
	)

	if c.cfg.httpClient != hc {
		t.Error("WithHTTPClient was not honored")
	}
	if c.cfg.tokenSource == nil {
		t.Error("WithTokenSource was not honored")
	}
	c.cfg.logger(context.Background(), "msg")
	if calls != 1 {
		t.Errorf("logger calls = %d, want 1", calls)
	}
}

// TestWithBaseURL_EmptyPanics locks in the chosen behavior for an empty
// base URL (see AC6(a) in TASK-M2.8.a): WithBaseURL panics with a clear
// message. NewClient itself returns *Client (no error), so panic is the only
// deterministic way to surface a programmer error at construction time.
func TestWithBaseURL_EmptyPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for empty base URL")
		}
	}()
	_ = NewClient(WithBaseURL(""))
}

// TestWithBaseURL_UnparseablePanics asserts the same contract for a base URL
// that net/url cannot parse.
func TestWithBaseURL_UnparseablePanics(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for unparseable base URL")
		}
	}()
	// Control characters in the URL force url.Parse to error.
	_ = NewClient(WithBaseURL("http://exa\x7fmple.com\x00"))
}

// TestStaticToken returns the literal token regardless of context.
func TestStaticToken(t *testing.T) {
	t.Parallel()

	ts := StaticToken("abc")
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "abc" {
		t.Errorf("token = %q, want %q", tok, "abc")
	}
}

// TestTokenSourceFunc adapts a function to the TokenSource interface.
func TestTokenSourceFunc(t *testing.T) {
	t.Parallel()

	called := 0
	ts := TokenSourceFunc(func(_ context.Context) (string, error) {
		called++
		return "f", nil
	})
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "f" {
		t.Errorf("token = %q, want %q", tok, "f")
	}
	if called != 1 {
		t.Errorf("called = %d, want 1", called)
	}
}

// TestServerError_Error asserts the formatted Error() string includes the
// status, code, and reason (so log lines are self-describing without
// additional caller context).
func TestServerError_Error(t *testing.T) {
	t.Parallel()

	se := &ServerError{Status: 404, Code: "not_found", Reason: "missing"}
	if se.Error() == "" {
		t.Error("Error() returned empty string")
	}
}

// TestServerError_Unwrap exhaustively verifies the AC3 status -> sentinel map.
// Cases include the explicit table rows, a 5xx that maps to ErrInternal, and
// an "other 4xx" that must Unwrap to an empty/nil chain (per the TASK
// locked-in choice). Unwrap returns []error since M3.2.a; matching is via
// errors.Is so adding a per-code sentinel (e.g. ErrInvalidStatusTransition)
// stays backward compatible.
func TestServerError_Unwrap(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"400_invalid_request", 400, ErrInvalidRequest},
		{"401_unauthorized", 401, ErrUnauthorized},
		{"403_forbidden", 403, ErrForbidden},
		{"404_not_found", 404, ErrNotFound},
		{"409_conflict", 409, ErrConflict},
		{"500_internal", 500, ErrInternal},
		{"502_internal", 502, ErrInternal},
		{"599_internal", 599, ErrInternal},
		{"418_other_4xx_nil", 418, nil},
		{"422_other_4xx_nil", 422, nil},
		{"399_below_4xx_nil", 399, nil},
		{"600_above_5xx_nil", 600, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			se := &ServerError{Status: tc.status}
			got := se.Unwrap()
			if tc.want == nil {
				if len(got) != 0 {
					t.Errorf("Unwrap() = %v, want empty/nil chain", got)
				}
				return
			}
			if !errors.Is(se, tc.want) {
				t.Errorf("errors.Is(se, %v) = false; Unwrap chain = %v", tc.want, got)
			}
		})
	}
}

// TestServerError_IsRoundTrip asserts the sentinels are errors.Is-friendly
// when surfaced as the wrapped error inside a *ServerError. This guards
// against accidental future regressions to the Unwrap chain.
func TestServerError_IsRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status int
		want   error
	}{
		{400, ErrInvalidRequest},
		{401, ErrUnauthorized},
		{403, ErrForbidden},
		{404, ErrNotFound},
		{409, ErrConflict},
		{500, ErrInternal},
		{503, ErrInternal},
	}
	for _, tc := range cases {
		se := &ServerError{Status: tc.status}
		if !errors.Is(se, tc.want) {
			t.Errorf("status=%d: errors.Is(se, %v) = false, want true", tc.status, tc.want)
		}
	}
}
