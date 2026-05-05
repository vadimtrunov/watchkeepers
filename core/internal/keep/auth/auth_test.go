package auth_test

import (
	"context"
	"encoding/base64"
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

// TestNewHMACVerifier_EmptyIssuer guards against a verifier configured
// with an empty issuer — the constructor must reject.
func TestNewHMACVerifier_EmptyIssuer(t *testing.T) {
	_, err := auth.NewHMACVerifier(fixedKey, "", time.Now)
	if err == nil {
		t.Fatal("NewHMACVerifier with empty issuer returned nil error")
	}
}

// TestNewTestIssuer_EmptyIssuer guards the mirror case on the mint path.
func TestNewTestIssuer_EmptyIssuer(t *testing.T) {
	_, err := auth.NewTestIssuer(fixedKey, "", time.Now)
	if err == nil {
		t.Fatal("NewTestIssuer with empty issuer returned nil error")
	}
}

// TestNewTestIssuer_ShortKey asserts the mirror of the key-length guard.
func TestNewTestIssuer_ShortKey(t *testing.T) {
	_, err := auth.NewTestIssuer([]byte("too-short"), "k", time.Now)
	if err == nil {
		t.Fatal("NewTestIssuer with short key returned nil error")
	}
}

// TestIssueWithExpiry lets tests mint a token whose `exp` is decoupled
// from the issuer's clock — useful for edge cases like "already expired
// at mint time".
func TestIssueWithExpiry(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	ti := mustIssuer(t, "keep-test", now)
	v := mustVerifier(t, "keep-test", now)

	// exp 2 minutes past the issuer's clock → valid.
	exp := now().Add(2 * time.Minute)
	tok, err := ti.IssueWithExpiry(auth.Claim{Subject: "u", Scope: "org"}, exp)
	if err != nil {
		t.Fatalf("IssueWithExpiry: %v", err)
	}
	got, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !got.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, exp)
	}

	// exp 1 hour before now → ErrExpired.
	past := now().Add(-time.Hour)
	expiredTok, err := ti.IssueWithExpiry(auth.Claim{Subject: "u", Scope: "org"}, past)
	if err != nil {
		t.Fatalf("IssueWithExpiry (past): %v", err)
	}
	_, err = v.Verify(context.Background(), expiredTok)
	if !errors.Is(err, auth.ErrExpired) {
		t.Errorf("Verify past exp = %v, want ErrExpired", err)
	}
}

// TestHMACVerifier_RoundTrip_OrganizationID covers the M3.5.a contract:
// a TestIssuer that mints a Claim carrying OrganizationID round-trips
// through HMAC verification with the field intact. Pinning this
// behaviour at the unit level locks the JWT `org_id` wire shape for
// every consumer (broker mint path, future keep handlers).
func TestHMACVerifier_RoundTrip_OrganizationID(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC) }
	ti := mustIssuer(t, "keep-test", now)
	v := mustVerifier(t, "keep-test", now)

	const orgID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	claim := auth.Claim{
		Subject:        "u-1",
		Scope:          "user:abc",
		OrganizationID: orgID,
	}
	tok, err := ti.Issue(claim, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	got, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.OrganizationID != orgID {
		t.Errorf("OrganizationID = %q, want %q", got.OrganizationID, orgID)
	}
	if got.Subject != claim.Subject || got.Scope != claim.Scope {
		t.Errorf("claim mismatch: got %+v, want %+v", got, claim)
	}
}

// TestHMACVerifier_LegacyTokenWithoutOrganizationID asserts the
// rolling-deploy contract: a token whose JSON payload omits the
// `org_id` key (legacy mint shape from before M3.5.a) MUST still verify
// successfully and surface an empty Claim.OrganizationID. We construct
// the legacy payload byte-for-byte using a sibling encoder so the test
// is decoupled from any change to TestIssuer's mint behaviour.
func TestHMACVerifier_LegacyTokenWithoutOrganizationID(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC) }
	v := mustVerifier(t, "keep-test", now)

	// Forge a payload with the pre-M3.5.a wire shape: no org_id key.
	tok, err := auth.MintLegacyTokenForTest(
		fixedKey, "keep-test",
		"u-1", "org",
		now(), now().Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("MintLegacyTokenForTest: %v", err)
	}

	got, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify legacy token: %v, want nil", err)
	}
	if got.OrganizationID != "" {
		t.Errorf("legacy token surfaced OrganizationID = %q, want empty", got.OrganizationID)
	}
	if got.Subject != "u-1" || got.Scope != "org" {
		t.Errorf("legacy round-trip mismatch: got %+v", got)
	}
}

// TestTestIssuer_OmitsOrganizationIDWhenEmpty asserts the byte-level
// wire-shape contract from M4.2.b applied to M3.5.a: when a Claim is
// minted with OrganizationID = "", the resulting JWT payload MUST NOT
// carry an `org_id` key. This keeps tokens minted by callers that
// haven't adopted the new field byte-compatible with the pre-M3.5.a
// shape so a verifier rolled back to the legacy code path can still
// parse them.
func TestTestIssuer_OmitsOrganizationIDWhenEmpty(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC) }
	ti := mustIssuer(t, "keep-test", now)

	tok, err := ti.Issue(auth.Claim{Subject: "u", Scope: "org"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("base64 decode payload: %v", err)
	}
	if strings.Contains(string(payloadJSON), `"org_id"`) {
		t.Errorf("legacy mint emitted `org_id` key; want omitted. payload=%s", payloadJSON)
	}
}

// TestTestIssuer_EmitsOrganizationIDWhenSet asserts the positive
// counterpart: when the claim carries a non-empty OrganizationID, the
// payload MUST encode it under the `org_id` key. Defends the contract
// against a future refactor that accidentally drops the field.
func TestTestIssuer_EmitsOrganizationIDWhenSet(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC) }
	ti := mustIssuer(t, "keep-test", now)

	const orgID = "11111111-1111-1111-1111-111111111111"
	tok, err := ti.Issue(auth.Claim{
		Subject:        "u",
		Scope:          "org",
		OrganizationID: orgID,
	}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(tok, ".")
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("base64 decode payload: %v", err)
	}
	want := `"org_id":"` + orgID + `"`
	if !strings.Contains(string(payloadJSON), want) {
		t.Errorf("payload missing %s; got %s", want, payloadJSON)
	}
}

// TestValidScope asserts the exported scope validator (used by both the
// verifier and db.RoleForScope) returns the right call for every
// documented shape.
func TestValidScope(t *testing.T) {
	good := []string{"org", "user:abc", "agent:abc"}
	for _, s := range good {
		if !auth.ValidScope(s) {
			t.Errorf("ValidScope(%q) = false, want true", s)
		}
	}
	bad := []string{"", "ORG", "user:", "agent:", "weird"}
	for _, s := range bad {
		if auth.ValidScope(s) {
			t.Errorf("ValidScope(%q) = true, want false", s)
		}
	}
}
