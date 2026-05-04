package secrets

import "errors"

// ErrSecretNotFound is returned by [SecretSource.Get] when the requested
// key is not present in the backing store, or when it is present but holds
// an empty value (empty env-var values are treated as "not set" — see
// [EnvSource] godoc for the rationale). Callers should use [errors.Is] to
// test for this sentinel.
var ErrSecretNotFound = errors.New("secrets: secret not found")

// ErrInvalidKey is returned by [SecretSource.Get] synchronously when the
// supplied key is the empty string. An empty key can never identify a
// meaningful secret and is always a programmer error at the call site.
// Callers should use [errors.Is] to test for this sentinel.
var ErrInvalidKey = errors.New("secrets: invalid key")
