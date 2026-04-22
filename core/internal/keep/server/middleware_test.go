package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
)

// #nosec G101 -- test fixture, not a real credential.
var mwSigningKey = []byte("0123456789abcdef0123456789abcdef")

func newIssuerAndVerifier(t *testing.T, now func() time.Time) (*auth.TestIssuer, auth.Verifier) {
	t.Helper()
	ti, err := auth.NewTestIssuer(mwSigningKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewTestIssuer: %v", err)
	}
	v, err := auth.NewHMACVerifier(mwSigningKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewHMACVerifier: %v", err)
	}
	return ti, v
}

// downstream is the canonical "happy path" handler we wrap with
// AuthMiddleware in every positive test. It asserts that the claim plucked
// from the context matches the one minted by the test issuer.
func downstream(t *testing.T, want auth.Claim) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := server.ClaimFromContext(r.Context())
		if !ok {
			t.Error("ClaimFromContext returned ok=false after AuthMiddleware success")
			http.Error(w, "no claim on context", http.StatusInternalServerError)
			return
		}
		if got.Subject != want.Subject || got.Scope != want.Scope {
			t.Errorf("claim mismatch: got %+v, want %+v", got, want)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
}

func TestAuthMiddleware_Happy(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	ti, v := newIssuerAndVerifier(t, now)

	claim := auth.Claim{Subject: "u-1", Scope: "org"}
	tok, err := ti.Issue(claim, 5*time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/whatever", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	server.AuthMiddleware(v)(downstream(t, claim)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// readErrorBody decodes the JSON error envelope the middleware writes on
// 401 and returns the `reason` field for assertion.
func readErrorBody(t *testing.T, rec *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var body struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (raw=%q)", err, rec.Body.String())
	}
	return body.Error, body.Reason
}

func TestAuthMiddleware_MissingHeader(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	_, v := newIssuerAndVerifier(t, now)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/whatever", nil)
	rec := httptest.NewRecorder()

	server.AuthMiddleware(v)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("downstream should not run on missing Authorization")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	errField, reason := readErrorBody(t, rec)
	if errField != "unauthorized" || reason != "missing_token" {
		t.Errorf("body = %q, want error=unauthorized reason=missing_token", rec.Body.String())
	}
}

func TestAuthMiddleware_MalformedHeader(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	_, v := newIssuerAndVerifier(t, now)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/whatever", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()

	server.AuthMiddleware(v)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("downstream should not run on non-Bearer auth")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	_, reason := readErrorBody(t, rec)
	if reason != "missing_token" {
		t.Errorf("reason = %q, want missing_token", reason)
	}
}

func TestAuthMiddleware_BadSignature(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }

	// Issuer uses a different key than the verifier to force a signature mismatch.
	otherKey := append([]byte{}, mwSigningKey...)
	otherKey[0] ^= 0xFF
	ti, err := auth.NewTestIssuer(otherKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewTestIssuer: %v", err)
	}
	v, err := auth.NewHMACVerifier(mwSigningKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewHMACVerifier: %v", err)
	}

	tok, err := ti.Issue(auth.Claim{Subject: "u", Scope: "org"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/whatever", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	server.AuthMiddleware(v)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("downstream should not run on bad signature")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	_, reason := readErrorBody(t, rec)
	if reason != "bad_signature" {
		t.Errorf("reason = %q, want bad_signature", reason)
	}
}

func TestAuthMiddleware_Expired(t *testing.T) {
	issueAt := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	verifyAt := issueAt.Add(10 * time.Minute)
	ti, err := auth.NewTestIssuer(mwSigningKey, "keep-test", func() time.Time { return issueAt })
	if err != nil {
		t.Fatalf("NewTestIssuer: %v", err)
	}
	v, err := auth.NewHMACVerifier(mwSigningKey, "keep-test", func() time.Time { return verifyAt })
	if err != nil {
		t.Fatalf("NewHMACVerifier: %v", err)
	}

	tok, err := ti.Issue(auth.Claim{Subject: "u", Scope: "org"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/whatever", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	server.AuthMiddleware(v)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("downstream should not run on expired token")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	_, reason := readErrorBody(t, rec)
	if reason != "expired" {
		t.Errorf("reason = %q, want expired", reason)
	}
}

func TestAuthMiddleware_BadScope(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	ti, v := newIssuerAndVerifier(t, now)

	tok, err := ti.Issue(auth.Claim{Subject: "u", Scope: "weird"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/whatever", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	server.AuthMiddleware(v)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("downstream should not run on bad scope")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	_, reason := readErrorBody(t, rec)
	if reason != "bad_scope" {
		t.Errorf("reason = %q, want bad_scope", reason)
	}
}

func TestClaimFromContext_Empty(t *testing.T) {
	_, ok := server.ClaimFromContext(context.Background())
	if ok {
		t.Error("ClaimFromContext on bare context returned ok=true")
	}
}
