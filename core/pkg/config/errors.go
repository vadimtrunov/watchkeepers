package config

import "errors"

// ErrParseYAML is returned by [Load] when the configured YAML file cannot
// be opened or its contents cannot be decoded as YAML. This is the
// catch-all sentinel for filesystem and decode failures of the file layer
// (Phase B in the layered load) — concrete causes (missing file,
// permission denied, malformed YAML) are wrapped via `%w` and visible to
// callers via [errors.Unwrap].
//
// `errors.Is(err, ErrParseYAML)` matches on any of these failure modes;
// callers that need to distinguish (e.g. "no file present" vs "file
// present but garbage") should use [errors.As] on the wrapped *os.PathError
// or yaml.TypeError.
var ErrParseYAML = errors.New("config: parse yaml")

// ErrUnknownField is returned by [Load] when the YAML decoder is in
// strict mode (`KnownFields(true)`) and encounters a key in the file
// that does not map to a struct field on [Config] or one of its
// sub-structs. Strict mode catches operator typos (`token_secrt:` instead
// of `token_secret:`) at Load time rather than at runtime when the
// associated feature breaks silently.
//
// Both [ErrUnknownField] and [ErrParseYAML] match on a strict-mode
// decode failure: ErrUnknownField is the more specific cause, returned
// (wrapped) when the underlying yaml error is a "field not found"
// diagnostic. Callers wanting to surface a typo-friendly message should
// branch on `errors.Is(err, ErrUnknownField)` first.
var ErrUnknownField = errors.New("config: unknown yaml field")

// ErrMissingRequired is returned by [Load] when the validation layer
// (Phase E in the layered load) detects a required field that is empty
// after defaults, YAML, env-var, and secret-resolution layers have all
// been applied. The wrapped error names the offending field via
// `fmt.Errorf("%w: %s", ErrMissingRequired, fieldPath)` so the operator
// can fix the config without reading source.
var ErrMissingRequired = errors.New("config: missing required field")

// ErrNoSecretSource is returned by [Load] when at least one `*_secret`
// field on [Config] is non-empty but no [secrets.SecretSource] was wired
// via [WithSecretSource]. A non-empty `*_secret` field is a request for
// secret resolution; without a SecretSource the loader has no way to
// honour it, and silently falling back to the literal would land
// secret-reference strings (not actual secrets) on the wire — a bug.
var ErrNoSecretSource = errors.New("config: secret source not configured")

// ErrSecretResolutionFailed is returned by [Load] when a configured
// [secrets.SecretSource] returns an error while resolving a `*_secret`
// field. The underlying [secrets.SecretSource] error is wrapped via
// `%w` so callers can match both this sentinel AND the deeper cause:
//
//	errors.Is(err, config.ErrSecretResolutionFailed) // → true
//	errors.Is(err, secrets.ErrSecretNotFound)        // → true (chain)
//
// Redaction discipline: the wrapped error from the SecretSource is
// expected to carry only the key name and the error type, never the
// secret value. The [secrets] package contract guarantees this for the
// stock [secrets.EnvSource]; future custom SecretSource implementations
// must honour the same rule.
var ErrSecretResolutionFailed = errors.New("config: secret resolution failed")
