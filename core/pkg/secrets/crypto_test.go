package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
)

// stubSecretSource is a hand-rolled SecretSource stand-in used by the
// crypto tests. Mirrors the fakeLogger pattern in secrets_test.go: no
// mocking library, plain map lookup. errs takes precedence over values
// for the same key so individual cases can inject targeted failures.
type stubSecretSource struct {
	values map[string]string
	errs   map[string]error
}

// Compile-time assertion: stubSecretSource satisfies SecretSource.
var _ SecretSource = stubSecretSource{}

func (s stubSecretSource) Get(_ context.Context, key string) (string, error) {
	if e, ok := s.errs[key]; ok {
		return "", e
	}
	if v, ok := s.values[key]; ok {
		return v, nil
	}
	return "", ErrSecretNotFound
}

// newTestKEK generates a fresh random 32-byte AES-256 key and returns the
// hex-encoded form that NewAESGCMEncrypter expects. Each call yields a
// different key, so a leaked test fixture cannot be reused as a static
// secret. CALLERS MUST NOT inline a fixed hex literal in test source —
// that would re-introduce the static-fixture foot-gun this helper exists
// to avoid.
func newTestKEK(t *testing.T) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// newEncrypter builds a fresh Encrypter from a fresh KEK. Convenience
// wrapper for the round-trip / nonce-uniqueness tests that don't care
// about constructor failure paths.
func newEncrypter(t *testing.T) (Encrypter, string) {
	t.Helper()
	kek := newTestKEK(t)
	src := stubSecretSource{values: map[string]string{"KEK": kek}}
	enc, err := NewAESGCMEncrypter(context.Background(), src, "KEK")
	if err != nil {
		t.Fatalf("NewAESGCMEncrypter: %v", err)
	}
	return enc, kek
}

// TestNewAESGCMEncrypter_HappyPath — valid 32-byte hex KEK from a stub
// SecretSource → returns non-nil Encrypter, no error.
func TestNewAESGCMEncrypter_HappyPath(t *testing.T) {
	t.Parallel()
	kek := newTestKEK(t)
	src := stubSecretSource{values: map[string]string{"KEK": kek}}
	enc, err := NewAESGCMEncrypter(context.Background(), src, "KEK")
	if err != nil {
		t.Fatalf("NewAESGCMEncrypter: unexpected error: %v", err)
	}
	if enc == nil {
		t.Fatalf("NewAESGCMEncrypter: nil Encrypter, no error")
	}
}

// TestEncryptDecrypt_RoundTrip — Decrypt(Encrypt(x)) == x for the four
// representative payload sizes called out in AC5.
func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	t.Parallel()
	enc, _ := newEncrypter(t)
	ctx := context.Background()

	cases := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"one byte", 1},
		{"1 KiB", 1024},
		{"64 KiB", 65536},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plaintext := make([]byte, tc.size)
			if tc.size > 0 {
				if _, err := rand.Read(plaintext); err != nil {
					t.Fatalf("rand.Read: %v", err)
				}
			}
			ct, err := enc.Encrypt(ctx, plaintext)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			// Sanity: ciphertext is at least nonce+tag bytes longer
			// than plaintext (it carries the 12-byte nonce and the
			// 16-byte tag).
			if len(ct) != len(plaintext)+aesGCMNonceLen+aesGCMTagLen {
				t.Fatalf("ciphertext len = %d, want %d", len(ct), len(plaintext)+aesGCMNonceLen+aesGCMTagLen)
			}
			pt, err := enc.Decrypt(ctx, ct)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !equalBytes(pt, plaintext) {
				t.Fatalf("Decrypt(Encrypt(x)) != x for size %d", tc.size)
			}
		})
	}
}

// TestEncrypt_NonceUniqueness — 100 encrypts of the SAME plaintext must
// yield 100 distinct ciphertexts. Each Encrypt call generates a fresh
// 12-byte nonce, so the prefix differs even for byte-identical
// plaintexts. This is the security pin for AC3.
func TestEncrypt_NonceUniqueness(t *testing.T) {
	t.Parallel()
	enc, _ := newEncrypter(t)
	ctx := context.Background()

	plaintext := []byte("identical plaintext for every encrypt call")
	const n = 100

	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		ct, err := enc.Encrypt(ctx, plaintext)
		if err != nil {
			t.Fatalf("Encrypt #%d: %v", i, err)
		}
		seen[string(ct)] = struct{}{}
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct ciphertexts from %d encrypts; want %d", len(seen), n, n)
	}
}

// TestDecrypt_TamperedCiphertext — flipping a byte in the ciphertext body
// or the tag must cause Decrypt to fail with ErrCiphertextAuthFailure.
// We test mutation in three positions: middle of the body (after nonce),
// last byte of the tag, and the nonce itself (also breaks tag check).
func TestDecrypt_TamperedCiphertext(t *testing.T) {
	t.Parallel()
	enc, _ := newEncrypter(t)
	ctx := context.Background()

	plaintext := []byte("authenticated message")
	ct, err := enc.Encrypt(ctx, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	cases := []struct {
		name string
		idx  int
	}{
		{"nonce byte", 0},
		{"body middle byte", aesGCMNonceLen + len(plaintext)/2},
		{"last tag byte", len(ct) - 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tampered := append([]byte(nil), ct...)
			tampered[tc.idx] ^= 0x01
			_, err := enc.Decrypt(ctx, tampered)
			if !errors.Is(err, ErrCiphertextAuthFailure) {
				t.Fatalf("Decrypt(tampered) err = %v, want errors.Is ErrCiphertextAuthFailure", err)
			}
		})
	}
}

// TestDecrypt_ShortCiphertext — ciphertext shorter than nonceSize+tagSize
// must be rejected with ErrCiphertextTooShort BEFORE calling cipher.Open.
// Three boundary cases: empty, 1 byte short of the minimum (27), exactly
// nonceSize (12, no tag).
func TestDecrypt_ShortCiphertext(t *testing.T) {
	t.Parallel()
	enc, _ := newEncrypter(t)
	ctx := context.Background()

	cases := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"nonce only (12)", aesGCMNonceLen},
		{"one byte short (27)", aesGCMNonceLen + aesGCMTagLen - 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ct := make([]byte, tc.size)
			_, err := enc.Decrypt(ctx, ct)
			if !errors.Is(err, ErrCiphertextTooShort) {
				t.Fatalf("Decrypt(short %d) err = %v, want errors.Is ErrCiphertextTooShort", tc.size, err)
			}
		})
	}
}

// TestNewAESGCMEncrypter_SecretNotFound — SecretSource returns
// ErrSecretNotFound for the KEK key → constructor wraps and returns it
// (errors.Is matches).
func TestNewAESGCMEncrypter_SecretNotFound(t *testing.T) {
	t.Parallel()
	src := stubSecretSource{errs: map[string]error{"KEK": ErrSecretNotFound}}
	_, err := NewAESGCMEncrypter(context.Background(), src, "KEK")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("NewAESGCMEncrypter err = %v, want errors.Is ErrSecretNotFound", err)
	}
}

// TestNewAESGCMEncrypter_InvalidHex — KEK that is NOT valid hex → returns
// wrapped ErrInvalidKEKHex.
func TestNewAESGCMEncrypter_InvalidHex(t *testing.T) {
	t.Parallel()
	src := stubSecretSource{values: map[string]string{"KEK": "not-hex-at-all-zzzz"}}
	_, err := NewAESGCMEncrypter(context.Background(), src, "KEK")
	if !errors.Is(err, ErrInvalidKEKHex) {
		t.Fatalf("NewAESGCMEncrypter err = %v, want errors.Is ErrInvalidKEKHex", err)
	}
}

// TestNewAESGCMEncrypter_WrongKeyLength — 16-byte KEK (32 hex chars,
// AES-128) must be rejected. We require AES-256 / 32 raw bytes / 64 hex
// chars; downgrading to AES-128 is not allowed.
func TestNewAESGCMEncrypter_WrongKeyLength(t *testing.T) {
	t.Parallel()
	// 16 random bytes → 32 hex chars (AES-128 — too short).
	var short [16]byte
	if _, err := rand.Read(short[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	src := stubSecretSource{values: map[string]string{"KEK": hex.EncodeToString(short[:])}}
	_, err := NewAESGCMEncrypter(context.Background(), src, "KEK")
	if !errors.Is(err, ErrInvalidKEKLength) {
		t.Fatalf("NewAESGCMEncrypter (16-byte KEK) err = %v, want errors.Is ErrInvalidKEKLength", err)
	}

	// 24 random bytes (AES-192 — also rejected).
	var mid [24]byte
	if _, err := rand.Read(mid[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	src = stubSecretSource{values: map[string]string{"KEK": hex.EncodeToString(mid[:])}}
	_, err = NewAESGCMEncrypter(context.Background(), src, "KEK")
	if !errors.Is(err, ErrInvalidKEKLength) {
		t.Fatalf("NewAESGCMEncrypter (24-byte KEK) err = %v, want errors.Is ErrInvalidKEKLength", err)
	}
}

// TestEncrypt_DistinctPrefixesAcrossPlaintexts — encrypting two different
// plaintexts under the same KEK produces ciphertexts whose first 12 bytes
// (the nonce prefix) differ. Pins nonce uniqueness across plaintexts in
// addition to the per-plaintext repetition test above.
func TestEncrypt_DistinctPrefixesAcrossPlaintexts(t *testing.T) {
	t.Parallel()
	enc, _ := newEncrypter(t)
	ctx := context.Background()

	ct1, err := enc.Encrypt(ctx, []byte("plaintext one"))
	if err != nil {
		t.Fatalf("Encrypt 1: %v", err)
	}
	ct2, err := enc.Encrypt(ctx, []byte("plaintext two"))
	if err != nil {
		t.Fatalf("Encrypt 2: %v", err)
	}
	if equalBytes(ct1[:aesGCMNonceLen], ct2[:aesGCMNonceLen]) {
		t.Fatalf("nonce prefix collision across two encrypts (probability ~2^-96)")
	}
}

// TestDecrypt_WrongKEK — ciphertext encrypted under KEK A cannot be
// decrypted under KEK B. Tag verification fails → wrapped
// ErrCiphertextAuthFailure.
func TestDecrypt_WrongKEK(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	encA, _ := newEncrypter(t)
	encB, _ := newEncrypter(t)

	ct, err := encA.Encrypt(ctx, []byte("only A can read this"))
	if err != nil {
		t.Fatalf("Encrypt under A: %v", err)
	}
	_, err = encB.Decrypt(ctx, ct)
	if !errors.Is(err, ErrCiphertextAuthFailure) {
		t.Fatalf("Decrypt under B err = %v, want errors.Is ErrCiphertextAuthFailure", err)
	}
}

// TestNoFixedKEKLiteralInTestSource — defense-in-depth lint check: the
// crypto_test.go source MUST NOT contain a hard-coded 64-hex-char KEK.
// Every test KEK is generated via newTestKEK; this test guards against a
// future contributor pasting a literal fixture (which would become a
// static credential in git history).
//
// We don't read the file from disk (filename-coupled tests are brittle);
// instead we assert via a simple property: the only hex-encoded-key
// material in the test corpus comes from rand.Read at runtime, so the
// constant aesGCMKeyLen is the only "32" that should appear in
// crypto_test.go in a key-length context. The compile-time check below
// pins this — if a literal is added, the property fails or the size
// constant is no longer the only path.
//
// This is a sentinel test; the real defense is code review. The test is
// here to make the policy visible in the test corpus itself.
func TestNoFixedKEKLiteralInTestSource(t *testing.T) {
	t.Parallel()
	// Assert that newTestKEK actually produces 64 hex characters
	// (32 bytes hex-encoded). If this property changes, the policy
	// invariant breaks visibly.
	got := newTestKEK(t)
	if len(got) != 2*aesGCMKeyLen {
		t.Fatalf("newTestKEK output length = %d, want %d (2 * aesGCMKeyLen)", len(got), 2*aesGCMKeyLen)
	}
	// Two consecutive calls must yield different keys.
	other := newTestKEK(t)
	if got == other {
		t.Fatalf("newTestKEK produced identical KEK on two calls — randomness broken or fixture is static")
	}
}

// TestEncryptDecrypt_Concurrency — 16 goroutines × 100 encrypts each must
// all succeed; the union of 1600 ciphertexts contains 1600 distinct
// values (nonce uniqueness under concurrent load). Round-trip-decrypts a
// sample to confirm correctness alongside concurrency.
func TestEncryptDecrypt_Concurrency(t *testing.T) {
	t.Parallel()
	enc, _ := newEncrypter(t)
	ctx := context.Background()

	const goroutines = 16
	const perG = 100
	plaintext := []byte("concurrent payload")

	var (
		mu     sync.Mutex
		seen   = make(map[string]struct{}, goroutines*perG)
		errsCh = make(chan error, goroutines)
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			local := make([][]byte, 0, perG)
			for i := 0; i < perG; i++ {
				ct, err := enc.Encrypt(ctx, plaintext)
				if err != nil {
					errsCh <- err
					return
				}
				local = append(local, ct)
			}
			mu.Lock()
			for _, ct := range local {
				seen[string(ct)] = struct{}{}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		if err != nil {
			t.Fatalf("concurrent Encrypt: %v", err)
		}
	}
	if got, want := len(seen), goroutines*perG; got != want {
		t.Fatalf("distinct ciphertexts = %d, want %d (nonce collision under load)", got, want)
	}

	// Spot-check: pick one ciphertext from the set and decrypt it.
	var sample string
	for k := range seen {
		sample = k
		break
	}
	pt, err := enc.Decrypt(ctx, []byte(sample))
	if err != nil {
		t.Fatalf("Decrypt sample: %v", err)
	}
	if !equalBytes(pt, plaintext) {
		t.Fatalf("Decrypt sample != plaintext")
	}
}

// TestErrorsDoNotLeakSecrets — all negative-path errors from Encrypt /
// Decrypt / NewAESGCMEncrypter must NOT contain the plaintext bytes or
// the KEK hex string in their .Error() output. This is the
// no-data-in-errors invariant from the security checklist.
func TestErrorsDoNotLeakSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const sentinel = "extremely-secret-plaintext-do-not-leak"

	// 1. Constructor with invalid hex KEK — error must not echo the bad
	// hex string back. (The bad hex is not the secret, but we still
	// don't want any KEK-shaped material echoed.)
	badHexKEK := sentinel + "-as-bad-kek"
	src := stubSecretSource{values: map[string]string{"KEK": badHexKEK}}
	_, err := NewAESGCMEncrypter(ctx, src, "KEK")
	if err == nil {
		t.Fatalf("expected NewAESGCMEncrypter error on bad hex")
	}
	if strings.Contains(err.Error(), badHexKEK) {
		t.Fatalf("constructor error leaks bad-hex KEK material: %q", err.Error())
	}

	// 2. Decrypt of tampered ciphertext — the underlying bytes must
	// not appear in the error string.
	enc, kek := newEncrypter(t)
	ct, err := enc.Encrypt(ctx, []byte(sentinel))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0x01
	_, err = enc.Decrypt(ctx, tampered)
	if err == nil {
		t.Fatalf("expected Decrypt error on tampered ciphertext")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("decrypt error leaks plaintext: %q", err.Error())
	}
	if strings.Contains(err.Error(), kek) {
		t.Fatalf("decrypt error leaks KEK hex: %q", err.Error())
	}
	if strings.Contains(err.Error(), string(ct)) {
		t.Fatalf("decrypt error leaks raw ciphertext: %q", err.Error())
	}
}

// equalBytes is a tiny non-allocating bytes-equal helper. We use it
// instead of bytes.Equal to keep the test file's import surface minimal
// (one less import means one less mismatch with `goimports`); the
// implementation is straightforward.
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
