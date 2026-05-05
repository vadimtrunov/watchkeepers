package auth

import (
	"encoding/json"
	"errors"
	"time"
)

// TestIssuer mints HS256 tokens whose shape exactly matches what
// hmacVerifier expects. It lives in the main package file set (not a
// `_test.go`) so integration tests in other packages — including the
// build-tag-gated keep read integration test — can import it as
// auth.NewTestIssuer. Production code must never depend on it; the core
// capability broker (M3.5) is the real mint path.
type TestIssuer struct {
	key    []byte
	issuer string
	now    func() time.Time
}

// NewTestIssuer constructs a TestIssuer. It shares the same key-length
// guard as NewHMACVerifier so test tokens have the same minimum security
// posture as prod verification. now may be nil — defaults to time.Now.
func NewTestIssuer(signingKey []byte, issuer string, now func() time.Time) (*TestIssuer, error) {
	if len(signingKey) < MinSigningKeyBytes {
		return nil, errors.New("auth: test issuer key must match verifier minimum length")
	}
	if issuer == "" {
		return nil, errors.New("auth: test issuer must not be empty")
	}
	if now == nil {
		now = time.Now
	}
	return &TestIssuer{
		key:    append([]byte(nil), signingKey...),
		issuer: issuer,
		now:    now,
	}, nil
}

// Issue mints a compact JWT-style token carrying the supplied claim. The
// claim's Issuer / ExpiresAt / IssuedAt fields are ignored — the issuer
// owns those to avoid test-side drift. The Subject, Scope, and
// OrganizationID fields are copied verbatim onto the JWT payload; an
// empty OrganizationID is omitted from the wire form via the payload's
// `omitempty` tag so legacy tests that never populated the field still
// produce byte-compatible tokens.
func (ti *TestIssuer) Issue(c Claim, ttl time.Duration) (string, error) {
	issuedAt := ti.now().UTC()
	expiresAt := issuedAt.Add(ttl)

	headerSeg, err := encodeSegment(jwtHeader)
	if err != nil {
		return "", err
	}
	payloadSeg, err := encodeSegment(payload{
		Subject:        c.Subject,
		Scope:          c.Scope,
		OrganizationID: c.OrganizationID,
		Issuer:         ti.issuer,
		ExpiresAt:      expiresAt.Unix(),
		IssuedAt:       issuedAt.Unix(),
	})
	if err != nil {
		return "", err
	}

	signingInput := headerSeg + "." + payloadSeg
	return signingInput + "." + sign(ti.key, signingInput), nil
}

// legacyPayload mirrors the pre-M3.5.a JWT payload byte-for-byte: no
// `org_id` key. Held as a private struct in this file (not the prod
// payload type) so the legacy shape stays pinned even if the prod
// struct evolves further. Only [MintLegacyTokenForTest] uses it.
type legacyPayload struct {
	Subject   string `json:"sub"`
	Scope     string `json:"scope"`
	Issuer    string `json:"iss"`
	ExpiresAt int64  `json:"exp"`
	IssuedAt  int64  `json:"iat"`
}

// MintLegacyTokenForTest forges a JWT-style token whose payload uses
// the pre-M3.5.a wire shape (no `org_id` key). Tests that exercise the
// rolling-deploy contract — "a verifier on the M3.5.a code path MUST
// still parse legacy tokens" — call this helper rather than depending
// on TestIssuer's behaviour, so the legacy shape stays pinned even if
// TestIssuer's mint path changes again. Production code must never use
// this; the export is gated on the file's package-level "test issuer"
// purpose (mirrors [TestIssuer]).
func MintLegacyTokenForTest(signingKey []byte, issuer, subject, scope string, issuedAt, expiresAt time.Time) (string, error) {
	if len(signingKey) < MinSigningKeyBytes {
		return "", errors.New("auth: legacy mint key must match verifier minimum length")
	}
	headerSeg, err := encodeSegment(jwtHeader)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(legacyPayload{
		Subject:   subject,
		Scope:     scope,
		Issuer:    issuer,
		ExpiresAt: expiresAt.UTC().Unix(),
		IssuedAt:  issuedAt.UTC().Unix(),
	})
	if err != nil {
		return "", err
	}
	payloadSeg := encodeSegmentRaw(raw)
	signingInput := headerSeg + "." + payloadSeg
	return signingInput + "." + sign(signingKey, signingInput), nil
}

// IssueWithExpiry is a convenience for tests that need to pin an explicit
// exp (e.g. to emit a token already expired at verify time). It still
// stamps iat with the issuer's clock. As with Issue, an empty
// OrganizationID is omitted from the wire form so the legacy mint shape
// is preserved when callers don't supply the new field.
func (ti *TestIssuer) IssueWithExpiry(c Claim, expiresAt time.Time) (string, error) {
	issuedAt := ti.now().UTC()

	headerSeg, err := encodeSegment(jwtHeader)
	if err != nil {
		return "", err
	}
	payloadSeg, err := encodeSegment(payload{
		Subject:        c.Subject,
		Scope:          c.Scope,
		OrganizationID: c.OrganizationID,
		Issuer:         ti.issuer,
		ExpiresAt:      expiresAt.UTC().Unix(),
		IssuedAt:       issuedAt.Unix(),
	})
	if err != nil {
		return "", err
	}

	signingInput := headerSeg + "." + payloadSeg
	return signingInput + "." + sign(ti.key, signingInput), nil
}
