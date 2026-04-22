package auth

import (
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
// owns those to avoid test-side drift.
func (ti *TestIssuer) Issue(c Claim, ttl time.Duration) (string, error) {
	issuedAt := ti.now().UTC()
	expiresAt := issuedAt.Add(ttl)

	headerSeg, err := encodeSegment(jwtHeader)
	if err != nil {
		return "", err
	}
	payloadSeg, err := encodeSegment(payload{
		Subject:   c.Subject,
		Scope:     c.Scope,
		Issuer:    ti.issuer,
		ExpiresAt: expiresAt.Unix(),
		IssuedAt:  issuedAt.Unix(),
	})
	if err != nil {
		return "", err
	}

	signingInput := headerSeg + "." + payloadSeg
	return signingInput + "." + sign(ti.key, signingInput), nil
}

// IssueWithExpiry is a convenience for tests that need to pin an explicit
// exp (e.g. to emit a token already expired at verify time). It still
// stamps iat with the issuer's clock.
func (ti *TestIssuer) IssueWithExpiry(c Claim, expiresAt time.Time) (string, error) {
	issuedAt := ti.now().UTC()

	headerSeg, err := encodeSegment(jwtHeader)
	if err != nil {
		return "", err
	}
	payloadSeg, err := encodeSegment(payload{
		Subject:   c.Subject,
		Scope:     c.Scope,
		Issuer:    ti.issuer,
		ExpiresAt: expiresAt.UTC().Unix(),
		IssuedAt:  issuedAt.Unix(),
	})
	if err != nil {
		return "", err
	}

	signingInput := headerSeg + "." + payloadSeg
	return signingInput + "." + sign(ti.key, signingInput), nil
}
