package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
)

// fixedKey is a deterministic 32-byte signing secret used by every unit test
// in this file. It is never a real credential.
// #nosec G101 -- test fixture.
var fixedKey = []byte("0123456789abcdef0123456789abcdef")

func mustIssuer(t *testing.T, issuer string, now func() time.Time) *auth.TestIssuer {
	t.Helper()
	ti, err := auth.NewTestIssuer(fixedKey, issuer, now)
	if err != nil {
		t.Fatalf("NewTestIssuer: %v", err)
	}
	return ti
}

func mustVerifier(t *testing.T, issuer string, now func() time.Time) auth.Verifier {
	t.Helper()
	v, err := auth.NewHMACVerifier(fixedKey, issuer, now)
	if err != nil {
		t.Fatalf("NewHMACVerifier: %v", err)
	}
	return v
}

// TestHMACVerifier_RoundTrip covers the AC1/Test-plan unit: testIssuer mints
// a token, hmacVerifier.Verify returns the identical Claim.
func TestHMACVerifier_RoundTrip(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	ti := mustIssuer(t, "keep-test", now)
	v := mustVerifier(t, "keep-test", now)

	claim := auth.Claim{
		Subject: "agent-42",
		Scope:   "agent:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
	}
	tok, err := ti.Issue(claim, 5*time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	got, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Subject != claim.Subject || got.Scope != claim.Scope {
		t.Errorf("claim mismatch: got %+v, want %+v", got, claim)
	}
	if got.Issuer != "keep-test" {
		t.Errorf("Issuer = %q, want %q", got.Issuer, "keep-test")
	}
	if got.ExpiresAt.IsZero() || got.IssuedAt.IsZero() {
		t.Errorf("timestamps unset: %+v", got)
	}
}

// TestHMACVerifier_WrongKey exercises the key-rotation regression: a
// verifier built with a different key must reject tokens minted with the
// original key.
func TestHMACVerifier_WrongKey(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	ti := mustIssuer(t, "keep-test", now)

	tok, err := ti.Issue(auth.Claim{Subject: "u", Scope: "org"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	otherKey := append([]byte{}, fixedKey...)
	otherKey[0] ^= 0xFF
	v, err := auth.NewHMACVerifier(otherKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewHMACVerifier: %v", err)
	}

	_, err = v.Verify(context.Background(), tok)
	if !errors.Is(err, auth.ErrBadSignature) {
		t.Errorf("Verify with wrong key = %v, want ErrBadSignature", err)
	}
}

// TestHMACVerifier_Expired asserts the expired-token branch: a token with
// exp before now() is rejected with ErrExpired.
func TestHMACVerifier_Expired(t *testing.T) {
	issueAt := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	verifyAt := issueAt.Add(10 * time.Minute)

	ti := mustIssuer(t, "keep-test", func() time.Time { return issueAt })
	v := mustVerifier(t, "keep-test", func() time.Time { return verifyAt })

	tok, err := ti.Issue(auth.Claim{Subject: "u", Scope: "org"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	_, err = v.Verify(context.Background(), tok)
	if !errors.Is(err, auth.ErrExpired) {
		t.Errorf("Verify expired = %v, want ErrExpired", err)
	}
}

// TestHMACVerifier_WrongIssuer asserts iss-check rejection.
func TestHMACVerifier_WrongIssuer(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	ti := mustIssuer(t, "other-issuer", now)
	v := mustVerifier(t, "keep-test", now)

	tok, err := ti.Issue(auth.Claim{Subject: "u", Scope: "org"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	_, err = v.Verify(context.Background(), tok)
	if !errors.Is(err, auth.ErrBadIssuer) {
		t.Errorf("Verify wrong issuer = %v, want ErrBadIssuer", err)
	}
}

// TestHMACVerifier_MalformedToken covers several shapes of invalid input.
func TestHMACVerifier_MalformedToken(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	v := mustVerifier(t, "keep-test", now)

	cases := []struct {
		name string
		tok  string
	}{
		{"empty", ""},
		{"one-segment", "abc"},
		{"two-segments", "abc.def"},
		{"four-segments", "a.b.c.d"},
		{"bad-base64-header", "@@@.eyJ9.signature"},
		{"bad-json-payload", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.@@@.signature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), tc.tok)
			if !errors.Is(err, auth.ErrMalformed) {
				t.Errorf("Verify(%q) = %v, want ErrMalformed", tc.tok, err)
			}
		})
	}
}

// TestHMACVerifier_BadScope asserts that a structurally valid token with an
// unrecognised scope is rejected before the middleware even sees it.
func TestHMACVerifier_BadScope(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	ti := mustIssuer(t, "keep-test", now)
	v := mustVerifier(t, "keep-test", now)

	tok, err := ti.Issue(auth.Claim{Subject: "u", Scope: "weird"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	_, err = v.Verify(context.Background(), tok)
	if !errors.Is(err, auth.ErrBadScope) {
		t.Errorf("Verify bad scope = %v, want ErrBadScope", err)
	}
}

// TestNewHMACVerifier_ShortKey asserts the key-length guard documented in the
// AC1 "≥ 32 bytes enforced at startup" line.
func TestNewHMACVerifier_ShortKey(t *testing.T) {
	_, err := auth.NewHMACVerifier([]byte("too-short"), "keep-test", time.Now)
	if err == nil {
		t.Fatal("NewHMACVerifier with short key returned nil error")
	}
	if !strings.Contains(err.Error(), "32") {
		t.Errorf("err = %v; want mention of 32-byte minimum", err)
	}
}
