package secrets

import (
	"context"
	"os"
)

// EnvSource is a [SecretSource] implementation that reads secret values
// from environment variables via [os.LookupEnv].
//
// # Empty-value convention
//
// An environment variable that is set but holds the empty string is
// treated as "not set" and returns [ErrSecretNotFound]. Most shells
// interpret `FOO=` as "variable exists but is empty"; collapsing the two
// states keeps the contract simple and avoids accidental use of empty
// secrets.
//
// # Redaction discipline
//
// Get NEVER logs secret values — not on success, not on error. Error-path
// log entries contain only the key name and the error. The success path
// produces zero log entries. Future Logger implementations wired via
// [WithLogger] must follow the same rule.
//
// Construct via [NewEnvSource]; the zero value is not usable (no options
// applied, logger is nil — which is safe, but intentional construction is
// preferred for clarity).
type EnvSource struct {
	logger Logger
}

// NewEnvSource constructs an [EnvSource] with the supplied options
// applied. The constructor never panics — environment variables are
// always available; no required dependency can be nil. When no
// [WithLogger] option is supplied the source operates without logging
// (silently drops error diagnostics).
func NewEnvSource(opts ...Option) *EnvSource {
	cfg := config{}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &EnvSource{
		logger: cfg.logger,
	}
}

// Get returns the value of the environment variable named by key.
//
// Validation order:
//  1. Empty key → [ErrInvalidKey] (synchronous, no env read).
//  2. ctx.Err() != nil → ctx.Err() (pre-check; no env read).
//  3. os.LookupEnv(key): not found OR empty value → log key + error,
//     return [ErrSecretNotFound].
//  4. Found and non-empty → return (value, nil). No log entry.
//
// The Logger (if wired) is called ONLY on error paths (steps 1–3 that
// produce a log). Step 4 (success) is intentionally silent — chatty
// success logging would expose access patterns and key names in
// production log streams.
func (e *EnvSource) Get(ctx context.Context, key string) (string, error) {
	if key == "" {
		return "", ErrInvalidKey
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	val, ok := os.LookupEnv(key)
	if !ok || val == "" {
		e.log(ctx, "secrets: secret not found", "key", key, "err", ErrSecretNotFound)
		return "", ErrSecretNotFound
	}
	return val, nil
}

// log forwards a diagnostic message to the optional [Logger]. Nil-logger
// safe: an EnvSource constructed without [WithLogger] silently drops.
func (e *EnvSource) log(ctx context.Context, msg string, kv ...any) {
	if e.logger == nil {
		return
	}
	e.logger.Log(ctx, msg, kv...)
}
