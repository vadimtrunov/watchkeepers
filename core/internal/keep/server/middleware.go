package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
)

// ctxKey is unexported so only this package can attach or extract a claim
// on the context. The single-element struct avoids collision with any
// other context value in the process.
type ctxKey struct{}

// claimKey is the singleton value used by AuthMiddleware and
// ClaimFromContext. Module-scoped per the stdlib convention for
// context.Value keys.
//
//nolint:gochecknoglobals // intentional module-scoped context key.
var claimKey = ctxKey{}

// ClaimFromContext returns the verified Claim attached by AuthMiddleware.
// It returns ok=false when the context has no claim (e.g. the handler was
// not wrapped, or is handling /health). Exported so handlers that live
// outside this package can fetch the claim without importing the key type.
func ClaimFromContext(ctx context.Context) (auth.Claim, bool) {
	c, ok := ctx.Value(claimKey).(auth.Claim)
	return c, ok
}

// AuthMiddleware verifies `Authorization: Bearer <token>` on every request.
// On success the resolved Claim is placed on the request context under an
// unexported key (retrievable via ClaimFromContext). On failure a 401 JSON
// body `{"error":"unauthorized","reason":"<code>"}` is returned and the
// downstream handler is not called.
//
// Reason codes are stable enum values (missing_token / bad_signature /
// expired / bad_scope / bad_token) so CI assertions and future client
// retry logic never depend on error text.
func AuthMiddleware(v auth.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				writeAuthError(w, "missing_token")
				return
			}
			claim, err := v.Verify(r.Context(), token)
			if err != nil {
				writeAuthError(w, reasonFor(err))
				return
			}
			ctx := context.WithValue(r.Context(), claimKey, claim)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken extracts the opaque token from an `Authorization: Bearer …`
// header value. Returns ok=false if the header is missing or uses any
// scheme other than Bearer (case-insensitive per RFC 7235).
func bearerToken(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(header[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// reasonFor maps a verifier error to the stable JSON reason code the
// middleware surfaces on 401. Unknown errors fall through to the generic
// "bad_token" sentinel so the middleware never leaks internal state.
func reasonFor(err error) string {
	switch {
	case errors.Is(err, auth.ErrMissingToken):
		return "missing_token"
	case errors.Is(err, auth.ErrBadSignature):
		return "bad_signature"
	case errors.Is(err, auth.ErrExpired):
		return "expired"
	case errors.Is(err, auth.ErrBadScope):
		return "bad_scope"
	case errors.Is(err, auth.ErrBadIssuer):
		return "bad_issuer"
	case errors.Is(err, auth.ErrMalformed):
		return "bad_token"
	default:
		return "bad_token"
	}
}

// writeAuthError emits the canonical 401 envelope. The body is
// hand-assembled rather than JSON-encoded so the output is byte-stable:
// the test plan and future clients depend on the field order never
// changing, and the payload has no untrusted input to escape.
func writeAuthError(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(w, `{"error":"unauthorized","reason":"`+reason+`"}`)
}
