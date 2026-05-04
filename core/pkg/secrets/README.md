# secrets — pluggable secret-value interface

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/secrets`

This package defines a small, pluggable abstraction for fetching secret
values. Phase 1 ships one concrete implementation, `EnvSource`, that reads
literal environment variables. Future milestones may add Vault, AWS Secrets
Manager, or GCP Secret Manager adapters; all will satisfy the same
`SecretSource` interface without touching callers. ROADMAP §M3 → M3.4 →
M3.4.a.

## Public API

```go
type SecretSource interface {
    Get(ctx context.Context, key string) (string, error)
}

type Logger interface {
    Log(ctx context.Context, msg string, kv ...any)
}

func NewEnvSource(opts ...Option) *EnvSource
func WithLogger(l Logger) Option
```

Sentinel errors in `errors.go`:

- `ErrSecretNotFound` — key absent or holds an empty value.
- `ErrInvalidKey` — empty key string (programmer error).

## SecretSource interface contract

`Get(ctx, key)` returns the secret value for `key`, or an error:

| Condition                      | Return                              |
| ------------------------------ | ----------------------------------- |
| `key == ""`                    | `("", ErrInvalidKey)` — synchronous |
| `ctx.Err() != nil`             | `("", ctx.Err())` — pre-check       |
| Key not found OR empty value   | `("", ErrSecretNotFound)`           |
| Key found with non-empty value | `(value, nil)`                      |

Both sentinel errors are matchable via `errors.Is`.

## EnvSource semantics

`EnvSource` resolves secrets from environment variables via
`os.LookupEnv`. The key is the exact environment variable name (no
implicit prefix, no transformation).

**Empty-value convention**: an environment variable that is set but
holds the empty string (`FOO=`) is treated as "not set" and returns
`ErrSecretNotFound`. Most shells interpret `FOO=` as "variable exists
but is empty"; collapsing both states keeps the contract simple and
prevents accidental use of empty secrets downstream.

```go
src := secrets.NewEnvSource(secrets.WithLogger(myLogger))

val, err := src.Get(ctx, "DATABASE_PASSWORD")
if errors.Is(err, secrets.ErrSecretNotFound) {
    // key absent or empty — handle missing config
}
```

## Redaction discipline

**`Get` NEVER logs secret values.** This contract is unconditional:

- The **success path** produces zero log entries.
- **Error-path** log entries contain only the key name and the error
  description — never the value.
- Even an "empty" value is a value and must not appear in any log field.

Future custom `Logger` implementations wired via `WithLogger` **must**
follow the same rule:

- Do **not** log the return value of `Get`.
- Do **not** log values retrieved from the backing store, even transiently.
- Log **only** key names and error types.

This rule exists because secret values in log streams create a class of
vulnerabilities (log injection, log aggregator breaches, audit-trail
pollution) that are trivially avoided by never logging values in the
first place.

## Functional options

`EnvSource` is constructed via `NewEnvSource` with zero or more `Option`
values:

```go
// Without logger — silent on errors.
src := secrets.NewEnvSource()

// With logger — error diagnostics emitted, success paths silent.
src := secrets.NewEnvSource(secrets.WithLogger(myLogger))
```

`WithLogger` accepts a nil argument as a no-op, so callers can always
pass through whatever logger they have:

```go
src := secrets.NewEnvSource(secrets.WithLogger(cfg.Logger)) // cfg.Logger may be nil
```

## Vault-ready example

The `SecretSource` interface is intentionally minimal so HTTP-backed
sources can satisfy it. A future `VaultSource` would look like:

```go
// VaultSource is a future implementation — NOT yet in this package.
type VaultSource struct {
    client *vault.Client
    mount  string
}

// Compile-time assertion: *VaultSource satisfies SecretSource.
var _ secrets.SecretSource = (*VaultSource)(nil)

func (v *VaultSource) Get(ctx context.Context, key string) (string, error) {
    if key == "" {
        return "", secrets.ErrInvalidKey
    }
    if err := ctx.Err(); err != nil {
        return "", err
    }
    secret, err := v.client.KVv2(v.mount).Get(ctx, key)
    if err != nil {
        // log key + err, NOT the value
        return "", fmt.Errorf("%w: %s", secrets.ErrSecretNotFound, key)
    }
    val, ok := secret.Data["value"].(string)
    if !ok || val == "" {
        return "", secrets.ErrSecretNotFound
    }
    return val, nil
}
```

No code changes in the config loader or other callers are needed — they
depend only on `SecretSource`.

## Concurrency

`*EnvSource` is safe for concurrent use. `os.LookupEnv` itself is safe
for concurrent reads; the `EnvSource` struct holds no mutable state after
construction.

## Out of scope (deferred)

- **VaultSource / AWS SSM / GCP Secret Manager** — interface is ready,
  concrete implementations deferred to future milestones.
- **Secret rotation / TTL caching** — a future hardening TASK.
- **Secret-value redaction wrapper** — the package never logs values;
  a middleware wrapper for third-party loggers is deferred.
- **`*_secret` config-field resolution** — consumed by M3.4.b config
  loader; this package is the seam, not the wiring.
