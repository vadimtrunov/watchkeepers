package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// defaultTimestampWindow is the maximum tolerated absolute drift
// between the X-Slack-Request-Timestamp header value and the local
// clock. Slack's published guidance is "five minutes" as a
// replay-attack guard; see
// <https://api.slack.com/authentication/verifying-requests-from-slack>.
//
// Operators that need a tighter or looser bound override via
// [WithTimestampWindow]. The lower bound is 1 second (any tighter and
// legitimate WAN-induced drift would burn requests); there is no upper
// bound enforced by the verifier — the operator owns that policy.
const defaultTimestampWindow = 5 * time.Minute

// signatureVersionPrefix is the literal `v0=` prefix Slack publishes
// on every X-Slack-Signature header. The verifier rejects values
// missing this prefix as malformed (i.e. ErrBadSignature) — a request
// with the wrong version cannot have been signed with our v0
// algorithm, even by accident.
const signatureVersionPrefix = "v0="

// signatureBaseSeparator is the colon Slack uses to glue the version,
// timestamp, and body in the signing-string format
// `v0:<ts>:<raw_body>`. Hoisted to a constant so the algorithm reads
// like the spec.
const signatureBaseSeparator = ":"

// Sentinel errors returned by [verifySignature]. Callers (the HTTP
// handler) match with [errors.Is] to map onto the correct response
// code (401 across the board) and the correct `reason` field on the
// `slack_webhook_rejected` audit row.
var (
	// ErrMissingHeader is returned when X-Slack-Signature or
	// X-Slack-Request-Timestamp is empty. The caller short-circuits
	// the signature compare and emits HTTP 401 with reason
	// `missing_header` on the audit row.
	ErrMissingHeader = errors.New("inbound: missing slack signing header")

	// ErrStaleTimestamp is returned when the request timestamp is
	// outside the configured window OR when the timestamp header is
	// not a parseable integer. The caller emits HTTP 401 with reason
	// `stale_timestamp`.
	ErrStaleTimestamp = errors.New("inbound: stale or unparseable timestamp")

	// ErrBadSignature is returned when the computed HMAC does not
	// match the supplied X-Slack-Signature value (constant-time
	// compare via hmac.Equal). Also covers the case where the header
	// is missing the `v0=` version prefix or contains non-hex bytes.
	// The caller emits HTTP 401 with reason `bad_signature`.
	ErrBadSignature = errors.New("inbound: bad slack signature")
)

// verifySignature implements the Slack v0 request-signature verification
// algorithm:
//
//  1. timestamp := strconv.ParseInt(tsHeader, 10, 64)
//  2. if |now() - timestamp| > window → ErrStaleTimestamp
//  3. base := "v0:" + tsHeader + ":" + raw_body
//  4. expected := "v0=" + hex(hmac_sha256(secret, base))
//  5. hmac.Equal([]byte(expected), []byte(sigHeader)) ? nil : ErrBadSignature
//
// The compare uses [hmac.Equal] rather than `bytes.Equal` or `==` to
// avoid leaking timing information about the secret-bound HMAC value
// (constant-time compare is a documented security obligation in the
// Slack reference and a Phase-1 LESSON for every signed-payload
// integration). The unit test pins the API choice as a regression
// guard.
//
// `now` is injected so tests can drive deterministic timestamps; the
// production caller wires it to [time.Now].
//
// Errors are sentinels — see [ErrMissingHeader], [ErrStaleTimestamp],
// [ErrBadSignature] — so the caller can branch the audit `reason`
// without parsing strings.
func verifySignature(
	secret []byte,
	sigHeader string,
	tsHeader string,
	body []byte,
	window time.Duration,
	now func() time.Time,
) error {
	if sigHeader == "" || tsHeader == "" {
		return ErrMissingHeader
	}

	tsUnix, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		// A non-integer timestamp is bucketed under "stale" rather
		// than "bad signature": the timestamp gate fails BEFORE we
		// reach the HMAC compare, and stale captures every "I cannot
		// trust this timestamp" branch.
		return ErrStaleTimestamp
	}
	if window <= 0 {
		window = defaultTimestampWindow
	}
	delta := now().Sub(time.Unix(tsUnix, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > window {
		return ErrStaleTimestamp
	}

	if !strings.HasPrefix(sigHeader, signatureVersionPrefix) {
		return ErrBadSignature
	}

	expected := computeSignature(secret, tsHeader, body)
	// hmac.Equal is the constant-time comparator. The compare runs
	// over the WHOLE header string ("v0=…") so a header that pads
	// the digest with extra bytes still fails — the length mismatch
	// surfaces as Equal == false, with no early exit on length
	// difference (Equal documents constant-time semantics).
	if !hmac.Equal([]byte(expected), []byte(sigHeader)) {
		return ErrBadSignature
	}
	return nil
}

// computeSignature renders the canonical "v0=<hex>" Slack signature
// string for the supplied (secret, timestamp, body) tuple. Hoisted out
// of [verifySignature] so the handler test fixtures can reuse it via
// the exported [Sign] helper without leaking internal state.
//
// The sequence is `v0:` + timestamp + `:` + raw_body, hashed under
// hmac_sha256(secret), hex-encoded, prefixed with `v0=`.
func computeSignature(secret []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signatureVersionPrefix[:2])) // "v0"
	mac.Write([]byte(signatureBaseSeparator))
	mac.Write([]byte(ts))
	mac.Write([]byte(signatureBaseSeparator))
	mac.Write(body)
	return signatureVersionPrefix + hex.EncodeToString(mac.Sum(nil))
}

// Sign returns the Slack `X-Slack-Signature` header value for the
// supplied (secret, timestamp, body) tuple per the v0 algorithm at
// <https://api.slack.com/authentication/verifying-requests-from-slack>.
//
// The function is exported so callers (test harnesses, integration
// fixtures, the bootstrap script) can construct correctly-signed
// requests using the SAME code path the verifier consults — AC6 of
// M6.3.a pins this discipline ("Tests construct the request with a
// valid signature using the same HMAC code path").
//
// Production code MUST NOT call Sign on the request side: the
// production signer is Slack itself, on the platform side. The
// function exists for tests and for bootstrap fixtures only.
func Sign(secret []byte, ts string, body []byte) string {
	return computeSignature(secret, ts, body)
}
