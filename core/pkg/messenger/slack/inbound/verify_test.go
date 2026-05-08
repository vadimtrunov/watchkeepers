package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"
	"time"
)

// TestVerifySignature_HappyPath asserts a request signed with the same
// signing secret + the documented Slack v0 algorithm verifies cleanly.
// Pins the algorithm against the Slack reference at
// https://api.slack.com/authentication/verifying-requests-from-slack.
func TestVerifySignature_HappyPath(t *testing.T) {
	t.Parallel()

	secret := []byte("8f742231b10e8888abcd99yyyzzz85a5")
	body := []byte(`{"token":"xyz","challenge":"abc","type":"url_verification"}`)
	ts := time.Unix(1_700_000_000, 0)
	tsStr := strconv.FormatInt(ts.Unix(), 10)
	sig := computeReferenceSignature(t, secret, tsStr, body)

	now := func() time.Time { return ts.Add(30 * time.Second) }
	if err := verifySignature(secret, sig, tsStr, body, defaultTimestampWindow, now); err != nil {
		t.Fatalf("verifySignature: unexpected error: %v", err)
	}
}

// TestVerifySignature_BadHMAC asserts a tampered signature returns
// ErrBadSignature.
func TestVerifySignature_BadHMAC(t *testing.T) {
	t.Parallel()

	secret := []byte("8f742231b10e8888abcd99yyyzzz85a5")
	body := []byte(`{"type":"url_verification"}`)
	tsStr := strconv.FormatInt(time.Unix(1_700_000_000, 0).Unix(), 10)
	good := computeReferenceSignature(t, secret, tsStr, body)
	// flip the last hex nibble
	tampered := good[:len(good)-1] + "0"
	if tampered == good {
		tampered = good[:len(good)-1] + "1"
	}

	now := func() time.Time { return time.Unix(1_700_000_010, 0) }
	err := verifySignature(secret, tampered, tsStr, body, defaultTimestampWindow, now)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("verifySignature: err = %v, want ErrBadSignature", err)
	}
}

// TestVerifySignature_MissingV0Prefix asserts a header without the
// `v0=` Slack version prefix is rejected as ErrBadSignature without
// running the constant-time compare against a malformed value.
func TestVerifySignature_MissingV0Prefix(t *testing.T) {
	t.Parallel()

	secret := []byte("secret")
	body := []byte(`{}`)
	tsStr := strconv.FormatInt(time.Unix(1_700_000_000, 0).Unix(), 10)
	good := computeReferenceSignature(t, secret, tsStr, body)
	stripped := good[3:] // drop "v0="

	now := func() time.Time { return time.Unix(1_700_000_010, 0) }
	err := verifySignature(secret, stripped, tsStr, body, defaultTimestampWindow, now)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("verifySignature: err = %v, want ErrBadSignature", err)
	}
}

// TestVerifySignature_StaleTimestamp asserts a request older than the
// configured window returns ErrStaleTimestamp without spending a
// signature compare. The 5-minute window is the Slack-documented
// replay-attack guard.
func TestVerifySignature_StaleTimestamp(t *testing.T) {
	t.Parallel()

	secret := []byte("secret")
	body := []byte(`{}`)
	ts := time.Unix(1_700_000_000, 0)
	tsStr := strconv.FormatInt(ts.Unix(), 10)
	sig := computeReferenceSignature(t, secret, tsStr, body)

	// 6 minutes after the request timestamp; default window is 5 min.
	now := func() time.Time { return ts.Add(6 * time.Minute) }
	err := verifySignature(secret, sig, tsStr, body, defaultTimestampWindow, now)
	if !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("verifySignature: err = %v, want ErrStaleTimestamp", err)
	}
}

// TestVerifySignature_FutureTimestamp asserts a timestamp far in the
// future (clock-skew or attacker tampering) is also rejected as stale.
// The window is symmetric: |now - ts| > window → reject.
func TestVerifySignature_FutureTimestamp(t *testing.T) {
	t.Parallel()

	secret := []byte("secret")
	body := []byte(`{}`)
	ts := time.Unix(1_700_000_000, 0)
	tsStr := strconv.FormatInt(ts.Unix(), 10)
	sig := computeReferenceSignature(t, secret, tsStr, body)

	// Request timestamp 6 min in the future relative to "now".
	now := func() time.Time { return ts.Add(-6 * time.Minute) }
	err := verifySignature(secret, sig, tsStr, body, defaultTimestampWindow, now)
	if !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("verifySignature: err = %v, want ErrStaleTimestamp (future), got %v", err, err)
	}
}

// TestVerifySignature_MalformedTimestamp asserts a non-numeric
// timestamp header is rejected as ErrStaleTimestamp (the call cannot
// proceed without a parseable integer).
func TestVerifySignature_MalformedTimestamp(t *testing.T) {
	t.Parallel()

	secret := []byte("secret")
	body := []byte(`{}`)
	now := func() time.Time { return time.Unix(1_700_000_000, 0) }
	err := verifySignature(secret, "v0=deadbeef", "not-a-number", body, defaultTimestampWindow, now)
	if !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("verifySignature: err = %v, want ErrStaleTimestamp", err)
	}
}

// TestVerifySignature_EmptyHeaders asserts both empty signature and
// empty timestamp short-circuit to ErrMissingHeader.
func TestVerifySignature_EmptyHeaders(t *testing.T) {
	t.Parallel()

	secret := []byte("secret")
	body := []byte(`{}`)
	now := func() time.Time { return time.Unix(1_700_000_000, 0) }

	if err := verifySignature(secret, "", "1700000000", body, defaultTimestampWindow, now); !errors.Is(err, ErrMissingHeader) {
		t.Fatalf("empty sig: err = %v, want ErrMissingHeader", err)
	}
	if err := verifySignature(secret, "v0=deadbeef", "", body, defaultTimestampWindow, now); !errors.Is(err, ErrMissingHeader) {
		t.Fatalf("empty ts: err = %v, want ErrMissingHeader", err)
	}
}

// TestVerifySignature_ConstantTimeCompare pins that verifySignature
// uses crypto/hmac.Equal — not bytes.Equal or `==` — for the
// signature comparison. Regression guard against a future "performance
// optimisation" introducing a timing oracle.
//
// The check is structural: we read the verify.go source and assert the
// constant-time API call appears. This is brittle but cheap; the
// alternative (a statistical timing test) would be flaky in CI.
func TestVerifySignature_ConstantTimeCompare(t *testing.T) {
	t.Parallel()

	src, err := readVerifyGoSource()
	if err != nil {
		t.Fatalf("read verify.go: %v", err)
	}
	if !contains(src, "hmac.Equal(") {
		t.Errorf("verify.go must use hmac.Equal for signature comparison")
	}
	if contains(src, "bytes.Equal(") {
		t.Errorf("verify.go must NOT use bytes.Equal — timing oracle risk")
	}
}

// computeReferenceSignature is the test-side reference implementation
// of the Slack v0 algorithm. Tests use it to construct VALID signatures
// per AC6 ("Tests construct the request with a valid signature using
// the same HMAC code path"). The implementation under test
// (verifySignature) MUST agree with this reference for the happy-path
// test to pass.
func computeReferenceSignature(t *testing.T, secret []byte, ts string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("v0:"))
	_, _ = mac.Write([]byte(ts))
	_, _ = mac.Write([]byte(":"))
	_, _ = mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}
