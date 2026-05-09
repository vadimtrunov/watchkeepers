package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Sizing constants for AES-256-GCM. Hard-coded to 32-byte keys (AES-256
// only — AES-128/192 are intentionally rejected; see [ErrInvalidKEKLength]).
// The 12-byte nonce and 16-byte authentication tag are the standard GCM
// parameters returned by [cipher.NewGCM]; we do not configure non-standard
// sizes.
const (
	aesGCMKeyLen   = 32
	aesGCMNonceLen = 12
	aesGCMTagLen   = 16
)

// ErrInvalidKEKLength is returned by [NewAESGCMEncrypter] when the
// hex-decoded KEK is not exactly 32 bytes (AES-256). 16-byte (AES-128) and
// 24-byte (AES-192) keys are rejected: this package commits to AES-256 only
// so the strength of the encryption is not silently downgraded by an
// operator setting the wrong-length env var.
var ErrInvalidKEKLength = errors.New("secrets: KEK must be 32 bytes (AES-256)")

// ErrInvalidKEKHex is returned by [NewAESGCMEncrypter] when the value
// resolved from the [SecretSource] is not valid hex (odd length or
// non-hex characters). The KEK is stored hex-encoded so it can travel
// through CLI-friendly env vars without binary-safety concerns.
var ErrInvalidKEKHex = errors.New("secrets: KEK is not valid hex")

// ErrCiphertextTooShort is returned by [Encrypter.Decrypt] when the input
// is shorter than nonceSize+tagSize (28 bytes) — the minimum length of a
// well-formed ciphertext. Returned BEFORE calling [cipher.AEAD.Open] so we
// surface a precise diagnostic instead of bubbling up a generic AEAD error.
var ErrCiphertextTooShort = errors.New("secrets: ciphertext too short for nonce + tag")

// ErrCiphertextAuthFailure is returned by [Encrypter.Decrypt] when GCM tag
// verification fails — either the ciphertext was mutated, the wrong KEK
// was used, or the nonce was corrupted. Wraps the underlying
// [cipher.AEAD.Open] error so callers can match by sentinel via
// [errors.Is] without depending on the standard library's internal error
// string.
var ErrCiphertextAuthFailure = errors.New("secrets: ciphertext authentication failure")

// Encrypter is the symmetric authenticated-encryption seam. Phase 1 ships
// a single AES-256-GCM implementation (constructed via
// [NewAESGCMEncrypter]); future implementations (envelope encryption with
// per-row data keys, KMS-backed primitives, key-rotation wrappers) will
// satisfy this interface without touching callers.
//
// # Ciphertext format
//
// The wire format is `nonce(12 bytes) || sealed`, where `sealed` is the
// output of [cipher.AEAD.Seal] — i.e. ciphertext concatenated with a
// 16-byte authentication tag. Decrypters parse the first 12 bytes as the
// nonce and pass the remainder to [cipher.AEAD.Open]; tag verification
// is constant-time and gates the returned plaintext.
//
// # Nonce uniqueness
//
// Encrypt MUST generate a fresh random nonce per call via
// [crypto/rand.Read]. Nonce reuse with the same key is catastrophic for
// GCM (it leaks the authentication subkey and breaks confidentiality).
// Implementations that cannot guarantee uniqueness MUST refuse to
// implement this interface.
//
// # Logging discipline
//
// Implementations MUST NEVER log plaintext, ciphertext, or the KEK. Error
// values returned to callers must carry only the error class (sentinel),
// never the protected bytes.
type Encrypter interface {
	Encrypt(ctx context.Context, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error)
}

// aesGCMEncrypter is the unexported AES-256-GCM implementation of
// [Encrypter]. The struct holds the bound [cipher.AEAD] (which captures
// the KEK); it is safe for concurrent use because [cipher.AEAD]
// implementations in the standard library are themselves concurrency-safe
// (Seal/Open allocate fresh buffers per call when dst==nil).
type aesGCMEncrypter struct {
	aead cipher.AEAD
}

// Compile-time assertion that the unexported type satisfies the public
// interface. Mirrors the pattern used elsewhere in the package
// (var _ SecretSource = (*EnvSource)(nil)).
var _ Encrypter = (*aesGCMEncrypter)(nil)

// NewAESGCMEncrypter constructs an [Encrypter] backed by AES-256-GCM. The
// KEK is fetched from src under kekKey, hex-decoded, and validated to be
// exactly 32 bytes; any deviation returns a wrapped sentinel error
// ([ErrSecretNotFound], [ErrInvalidKEKHex], or [ErrInvalidKEKLength]).
//
// The constructor never logs the KEK or any derived material. Errors
// surface only the error class — operators inspect logs to discover that
// the KEK env var is mis-configured, never to recover the key itself.
func NewAESGCMEncrypter(ctx context.Context, src SecretSource, kekKey string) (Encrypter, error) {
	if src == nil {
		return nil, errors.New("secrets: SecretSource is nil")
	}
	hexKEK, err := src.Get(ctx, kekKey)
	if err != nil {
		// Wrap so callers can errors.Is to ErrSecretNotFound while still
		// learning that the failure happened during KEK lookup. The error
		// chain carries no key material.
		return nil, fmt.Errorf("secrets: KEK lookup: %w", err)
	}
	keyBytes, err := hex.DecodeString(hexKEK)
	if err != nil {
		// Return bare sentinel only — the stdlib hex.DecodeString error
		// embeds the offending rune directly (e.g. "invalid byte: U+0057 'W'"),
		// so wrapping it would leak a KEK byte into the error string.
		return nil, ErrInvalidKEKHex
	}
	if len(keyBytes) != aesGCMKeyLen {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKEKLength, len(keyBytes))
	}
	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("secrets: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: cipher.NewGCM: %w", err)
	}
	return &aesGCMEncrypter{aead: aead}, nil
}

// Encrypt seals plaintext under AES-256-GCM with a fresh 12-byte random
// nonce read from [crypto/rand.Reader]. The output is
// `nonce || aead.Seal(nil, nonce, plaintext, nil)` — i.e. nonce
// concatenated with ciphertext-and-tag. The ctx parameter is reserved for
// future cancellation hooks (e.g. KMS-backed Encrypters); the in-memory
// AES-GCM impl returns synchronously.
func (e *aesGCMEncrypter) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, aesGCMNonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secrets: nonce read: %w", err)
	}
	// Seal returns ciphertext||tag. Pre-allocate a buffer big enough for
	// nonce||ciphertext||tag and let Seal append into it; this avoids one
	// intermediate allocation and one extra copy compared to
	// append(nonce, e.aead.Seal(nil, ...)...).
	out := make([]byte, aesGCMNonceLen, aesGCMNonceLen+len(plaintext)+aesGCMTagLen)
	copy(out, nonce)
	return e.aead.Seal(out, nonce, plaintext, nil), nil
}

// Decrypt parses ciphertext as `nonce(12) || sealed` and verifies the GCM
// authentication tag. Returns wrapped [ErrCiphertextTooShort] if the input
// is shorter than 28 bytes (nonce+tag), and wrapped
// [ErrCiphertextAuthFailure] if tag verification fails (mutated
// ciphertext, wrong KEK, corrupted nonce). The underlying
// [cipher.AEAD.Open] is constant-time wrt the verification step; we add no
// timing-leaky branches around it.
func (e *aesGCMEncrypter) Decrypt(_ context.Context, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < aesGCMNonceLen+aesGCMTagLen {
		return nil, fmt.Errorf("%w: got %d bytes", ErrCiphertextTooShort, len(ciphertext))
	}
	nonce, sealed := ciphertext[:aesGCMNonceLen], ciphertext[aesGCMNonceLen:]
	plaintext, err := e.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		// Wrap with sentinel only — the underlying error message is
		// "cipher: message authentication failed", which is fine to
		// surface to logs but NOT the ciphertext or any byte of it.
		return nil, fmt.Errorf("%w: %v", ErrCiphertextAuthFailure, err)
	}
	return plaintext, nil
}
