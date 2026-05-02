package keepclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHealth_Success asserts the happy path: a 200 {"status":"ok"} server
// returns nil from Client.Health.
func TestHealth_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q, want /health", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("Authorization header must be absent on /health")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	if err := c.Health(context.Background()); err != nil {
		t.Errorf("Health: %v", err)
	}
}

// TestHealth_Internal asserts that a 500 server response is surfaced as a
// *ServerError whose Unwrap chain matches ErrInternal.
func TestHealth_Internal(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"internal","reason":"boom"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	err := c.Health(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *ServerError
	if !errors.As(err, &se) {
		t.Fatalf("error is not *ServerError: %T %v", err, err)
	}
	if se.Status != http.StatusInternalServerError {
		t.Errorf("Status = %d, want 500", se.Status)
	}
	if se.Code != "internal" || se.Reason != "boom" {
		t.Errorf("Code/Reason = %q/%q, want internal/boom", se.Code, se.Reason)
	}
	if !errors.Is(err, ErrInternal) {
		t.Error("errors.Is(err, ErrInternal) = false, want true")
	}
}

// TestHealth_NetworkError asserts that a transport-level failure (server
// unreachable) is wrapped, not converted to a *ServerError.
func TestHealth_NetworkError(t *testing.T) {
	t.Parallel()

	// Reserve a port and immediately release it so the connection refuses.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient(WithBaseURL(url))
	err := c.Health(context.Background())
	if err == nil {
		t.Fatal("expected error against closed server, got nil")
	}
	var se *ServerError
	if errors.As(err, &se) {
		t.Errorf("transport error must not be a *ServerError; got %v", err)
	}
}

// TestDo_AllSentinelMappings parametrically verifies that every status in the
// AC3 mapping table round-trips through do() into the documented sentinel.
// Driven against a /v1/* path so the auth-injection branch is exercised too.
func TestDo_AllSentinelMappings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status int
		want   error // nil means: no sentinel match (generic *ServerError)
	}{
		{"400", 400, ErrInvalidRequest},
		{"401", 401, ErrUnauthorized},
		{"403", 403, ErrForbidden},
		{"404", 404, ErrNotFound},
		{"409", 409, ErrConflict},
		{"500", 500, ErrInternal},
		{"503", 503, ErrInternal},
		{"418", 418, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":"err_%d","reason":"r_%d"}`, tc.status, tc.status)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			err := c.do(context.Background(), http.MethodGet, "/v1/anything", nil, nil)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var se *ServerError
			if !errors.As(err, &se) {
				t.Fatalf("error is not *ServerError: %T %v", err, err)
			}
			if se.Status != tc.status {
				t.Errorf("Status = %d, want %d", se.Status, tc.status)
			}
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Errorf("errors.Is(err, %v) = false, want true", tc.want)
				}
			} else {
				// Generic *ServerError: Unwrap returns nil; sentinels do
				// not match.
				for _, sentinel := range []error{
					ErrUnauthorized, ErrForbidden, ErrNotFound,
					ErrConflict, ErrInvalidRequest, ErrInternal,
				} {
					if errors.Is(err, sentinel) {
						t.Errorf("errors.Is(err, %v) = true, want false", sentinel)
					}
				}
			}
		})
	}
}

// TestDo_NoTokenSourceForV1 asserts that calling /v1/* without a TokenSource
// returns ErrNoTokenSource synchronously, with NO network round-trip.
func TestDo_NoTokenSourceForV1(t *testing.T) {
	t.Parallel()

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	err := c.do(context.Background(), http.MethodGet, "/v1/anything", nil, nil)
	if !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("err = %v, want ErrNoTokenSource", err)
	}
	if called {
		t.Error("server was contacted; ErrNoTokenSource must short-circuit before any network call")
	}
}

// TestDo_InjectsAuthorizationOnV1 asserts that a /v1/* request carries the
// Authorization header sourced from the configured TokenSource.
func TestDo_InjectsAuthorizationOnV1(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("xyz")))
	if err := c.do(context.Background(), http.MethodGet, "/v1/keepers-log", nil, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if gotAuth != "Bearer xyz" {
		t.Errorf("Authorization = %q, want \"Bearer xyz\"", gotAuth)
	}
}

// TestDo_DoesNotInjectAuthOnHealth asserts that even when WithTokenSource is
// set, the /health path stays unauthenticated (no Token call, no header).
func TestDo_DoesNotInjectAuthOnHealth(t *testing.T) {
	t.Parallel()

	tokenCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("Authorization = %q, want empty on /health", h)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	t.Cleanup(srv.Close)

	ts := TokenSourceFunc(func(context.Context) (string, error) {
		tokenCalls++
		return "leak", nil
	})
	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(ts))
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if tokenCalls != 0 {
		t.Errorf("TokenSource was consulted %d times for /health; want 0", tokenCalls)
	}
}

// TestDo_TokenSourceErrorShortCircuits asserts that a TokenSource returning
// an error halts the request before any network call (security invariant: a
// stale-token request must never be sent).
func TestDo_TokenSourceErrorShortCircuits(t *testing.T) {
	t.Parallel()

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)

	wantErr := errors.New("refresh failed")
	ts := TokenSourceFunc(func(context.Context) (string, error) {
		return "", wantErr
	})
	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(ts))
	err := c.do(context.Background(), http.MethodGet, "/v1/x", nil, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wraps %v", err, wantErr)
	}
	if called {
		t.Error("server was contacted; token error must short-circuit before any network call")
	}
}

// TestDo_TrailingSlashJoin asserts that base URLs with and without trailing
// slash join paths identically (no double slash).
func TestDo_TrailingSlashJoin(t *testing.T) {
	t.Parallel()

	cases := []string{"http://example.invalid", "http://example.invalid/"}
	for _, base := range cases {
		t.Run(base, func(t *testing.T) {
			t.Parallel()
			var seen string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.URL.Path
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"status":"ok"}`)
			}))
			t.Cleanup(srv.Close)
			// Substitute the base host with the real test-server host, but
			// preserve the trailing-slash pattern under test by appending
			// the path through WithBaseURL on srv.URL plus a synthetic
			// trailing slash flag handled via the local 'base' loop value.
			useBase := srv.URL
			if strings.HasSuffix(base, "/") {
				useBase = srv.URL + "/"
			}
			c := NewClient(WithBaseURL(useBase))
			if err := c.Health(context.Background()); err != nil {
				t.Fatalf("Health: %v", err)
			}
			if seen != "/health" {
				t.Errorf("server saw %q, want /health (no double slash)", seen)
			}
		})
	}
}

// TestDo_BodyMarshalAndContentType asserts that a non-nil body is marshalled
// as JSON and Content-Type is set correctly.
func TestDo_BodyMarshalAndContentType(t *testing.T) {
	t.Parallel()

	type req struct {
		Foo string `json:"foo"`
	}
	type resp struct {
		Echo string `json:"echo"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var got req
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp{Echo: got.Foo})
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	var out resp
	err := c.do(context.Background(), http.MethodPost, "/v1/echo", req{Foo: "bar"}, &out)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if out.Echo != "bar" {
		t.Errorf("out.Echo = %q, want bar", out.Echo)
	}
}

// TestDo_BadResponseJSONWhenOutNonNil asserts that a 200 with a non-JSON
// body when out != nil is reported as a decode error (not silently dropped).
func TestDo_BadResponseJSONWhenOutNonNil(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not json at all")
	}))
	t.Cleanup(srv.Close)

	type out struct{ X int }
	var got out
	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.do(context.Background(), http.MethodGet, "/v1/x", nil, &got)
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	// Explicitly NOT a *ServerError — the request itself succeeded.
	var se *ServerError
	if errors.As(err, &se) {
		t.Errorf("decode failure must not surface as *ServerError; got %v", err)
	}
}

// TestDo_ErrorBodyFallbackToRaw asserts that when the server returns a
// non-2xx with a body that is NOT the JSON envelope, the *ServerError still
// surfaces with Code="" and Reason set to the (truncated) raw body.
func TestDo_ErrorBodyFallbackToRaw(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream borked")
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	err := c.Health(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *ServerError
	if !errors.As(err, &se) {
		t.Fatalf("error is not *ServerError: %T %v", err, err)
	}
	if se.Status != http.StatusBadGateway {
		t.Errorf("Status = %d, want 502", se.Status)
	}
	if se.Code != "" {
		t.Errorf("Code = %q, want empty (no JSON envelope)", se.Code)
	}
	if se.Reason != "upstream borked" {
		t.Errorf("Reason = %q, want raw body fallback", se.Reason)
	}
	if !errors.Is(err, ErrInternal) {
		t.Error("502 should map to ErrInternal")
	}
}
