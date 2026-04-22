// Package auth defines the Keep capability-claim contract and a stdlib-only
// HMAC-SHA256 verifier.
//
// Tokens are compact JWT-like strings of the form
// `base64url(header).base64url(payload).base64url(hmac-sha256(key,
// header+"."+payload))`. The header is fixed to
// `{"alg":"HS256","typ":"JWT"}`, and the payload carries the public Claim
// fields plus standard `exp`/`iat` timestamps. We deliberately use only the
// standard library (`crypto/hmac`, `crypto/sha256`, `crypto/subtle`,
// `encoding/base64`, `encoding/json`) so the verifier has no external JWT
// dependency — this is the contract the future M3.5 capability broker will
// mint against, and pinning a JWT library today would constrain that
// design needlessly.
//
// The Keep side only verifies tokens. Minting happens in tests via
// TestIssuer (see testing.go) and, in production, in the core capability
// broker that is out of scope for this milestone.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MinSigningKeyBytes is the minimum HMAC-SHA256 key length accepted by
// NewHMACVerifier. 32 bytes matches the SHA-256 block/output size; anything
// shorter provides no meaningful security margin for HS256.
const MinSigningKeyBytes = 32

// Sentinel errors that Verify may wrap. Callers (middleware, tests) match
// with errors.Is rather than comparing error text.
var (
	// ErrMissingToken — Authorization header absent or not a Bearer token.
	ErrMissingToken = errors.New("missing token")
	// ErrMalformed — token is not three base64url segments of JSON.
	ErrMalformed = errors.New("malformed token")
	// ErrBadSignature — HMAC check failed (wrong key or tampered payload).
	ErrBadSignature = errors.New("bad signature")
	// ErrBadIssuer — token `iss` did not match configured issuer.
	ErrBadIssuer = errors.New("bad issuer")
	// ErrExpired — `exp` is before the verifier's clock.
	ErrExpired = errors.New("expired token")
	// ErrBadScope — claim.Scope is neither "org" nor has a known "user:" /
	// "agent:" prefix. Also returned by db.WithScope.
	ErrBadScope = errors.New("bad scope")
)

// Claim is the verified capability payload the middleware hands downstream.
// Fields map 1:1 onto the JWT payload, with JSON tags matching the RFC 7519
// standard names where relevant (`sub`, `iss`, `exp`, `iat`) plus our
// custom `scope`. The struct is value-typed and safe to pass by value; it
// never carries the raw token.
type Claim struct {
	// Subject is the stable identifier of the token bearer (e.g. a
	// watchkeeper UUID or a human user id). Maps to JWT `sub`.
	Subject string `json:"sub"`
	// Scope derives the DB role and the watchkeeper.scope GUC. Must be
	// "org", "user:<uuid>", or "agent:<uuid>".
	Scope string `json:"scope"`
	// Issuer is the minting authority. Verified against the configured
	// issuer string. Maps to JWT `iss`.
	Issuer string `json:"iss"`
	// ExpiresAt bounds token validity. Maps to JWT `exp`.
	ExpiresAt time.Time `json:"-"`
	// IssuedAt is the mint time. Maps to JWT `iat`.
	IssuedAt time.Time `json:"-"`
}

// Verifier verifies capability tokens. The middleware holds a single
// Verifier for the process lifetime; implementations must be safe for
// concurrent use.
type Verifier interface {
	// Verify parses and checks the token. On any validation failure it
	// returns a sentinel error from this package wrapped with context.
	// On success the returned Claim is safe to place on a context.
	Verify(ctx context.Context, token string) (Claim, error)
}

// NewHMACVerifier returns a Verifier that accepts HS256 tokens signed with
// signingKey. The verifier checks issuer, exp, and scope shape. now
// supplies the clock for exp; tests inject a fixed-time now to stay
// deterministic.
func NewHMACVerifier(signingKey []byte, issuer string, now func() time.Time) (Verifier, error) {
	if len(signingKey) < MinSigningKeyBytes {
		return nil, fmt.Errorf("auth: signing key must be >= %d bytes, got %d", MinSigningKeyBytes, len(signingKey))
	}
	if issuer == "" {
		return nil, errors.New("auth: issuer must not be empty")
	}
	if now == nil {
		now = time.Now
	}
	return &hmacVerifier{
		key:    append([]byte(nil), signingKey...),
		issuer: issuer,
		now:    now,
	}, nil
}

type hmacVerifier struct {
	key    []byte
	issuer string
	now    func() time.Time
}

// jwtHeader is the fixed HS256/JWT header we accept and emit. Declared
// module-scoped so both mint and verify paths share the same literal.
//
//nolint:gochecknoglobals // intentional module constant; value is fixed.
var jwtHeader = header{Alg: "HS256", Typ: "JWT"}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// payload mirrors the on-wire JSON for a token body. `exp` and `iat` are
// Unix seconds (RFC 7519 NumericDate) rather than Go time.Time so rounding
// is predictable and interop with non-Go signers stays trivial.
type payload struct {
	Subject   string `json:"sub"`
	Scope     string `json:"scope"`
	Issuer    string `json:"iss"`
	ExpiresAt int64  `json:"exp"`
	IssuedAt  int64  `json:"iat"`
}

// Verify implements Verifier. The body is split into three base64url
// segments; the signature is compared in constant time; the payload is
// JSON-decoded and checked against issuer, expiry, and scope shape.
func (v *hmacVerifier) Verify(_ context.Context, token string) (Claim, error) {
	parts := strings.Split(token, ".")
	if token == "" || len(parts) != 3 {
		return Claim{}, fmt.Errorf("%w: want three segments", ErrMalformed)
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claim{}, fmt.Errorf("%w: header base64: %v", ErrMalformed, err)
	}
	var h header
	if err := json.Unmarshal(headerJSON, &h); err != nil {
		return Claim{}, fmt.Errorf("%w: header json: %v", ErrMalformed, err)
	}
	if h.Alg != jwtHeader.Alg || h.Typ != jwtHeader.Typ {
		return Claim{}, fmt.Errorf("%w: header alg=%q typ=%q", ErrMalformed, h.Alg, h.Typ)
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claim{}, fmt.Errorf("%w: payload base64: %v", ErrMalformed, err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claim{}, fmt.Errorf("%w: signature base64: %v", ErrMalformed, err)
	}

	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, v.key)
	mac.Write([]byte(signingInput))
	want := mac.Sum(nil)
	if subtle.ConstantTimeCompare(want, sig) != 1 {
		return Claim{}, ErrBadSignature
	}

	var p payload
	if err := json.Unmarshal(payloadJSON, &p); err != nil {
		return Claim{}, fmt.Errorf("%w: payload json: %v", ErrMalformed, err)
	}
	if p.Issuer != v.issuer {
		return Claim{}, fmt.Errorf("%w: got %q", ErrBadIssuer, p.Issuer)
	}
	if p.ExpiresAt == 0 {
		return Claim{}, fmt.Errorf("%w: missing exp", ErrMalformed)
	}
	exp := time.Unix(p.ExpiresAt, 0).UTC()
	if !v.now().UTC().Before(exp) {
		return Claim{}, ErrExpired
	}
	if !ValidScope(p.Scope) {
		return Claim{}, fmt.Errorf("%w: %q", ErrBadScope, p.Scope)
	}

	return Claim{
		Subject:   p.Subject,
		Scope:     p.Scope,
		Issuer:    p.Issuer,
		ExpiresAt: exp,
		IssuedAt:  time.Unix(p.IssuedAt, 0).UTC(),
	}, nil
}

// ValidScope returns true if s is one of the three recognised scope forms
// — "org", "user:<non-empty>", or "agent:<non-empty>". Exported so other
// packages (middleware, WithScope) share the single source of truth.
func ValidScope(s string) bool {
	if s == "org" {
		return true
	}
	if rest, ok := strings.CutPrefix(s, "user:"); ok && rest != "" {
		return true
	}
	if rest, ok := strings.CutPrefix(s, "agent:"); ok && rest != "" {
		return true
	}
	return false
}

// sign produces the HS256 signature segment for a given signing input.
// Shared between TestIssuer.Issue and (indirectly) unit tests that forge
// tampered tokens.
func sign(key []byte, signingInput string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signingInput))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// encodeSegment base64url-encodes v as JSON with no trailing padding
// (RFC 7515 §2). Used by TestIssuer.
func encodeSegment(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
