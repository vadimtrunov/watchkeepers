# config — layered operator configuration loader

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/config`

This package loads operator configuration from a layered source stack and
returns a strongly-typed `*Config` to consumers. ROADMAP §M3 → M3.4 → M3.4.b.

## Layer order

The loader applies four layers, from lowest to highest precedence:

1. **Defaults** — package-baked values that consult the process environment
   (e.g. `Notebook.DataDir = $WATCHKEEPER_DATA/notebook` when
   `WATCHKEEPER_DATA` is set).
2. **YAML file** via `WithFile` — strict mode (`yaml.Decoder.KnownFields(true)`)
   rejects unknown keys with `ErrUnknownField`. Empty path → skip.
3. **Env-var overrides** — fields with an `env:"NAME"` struct tag are
   overridden by the named env var. Empty values do NOT override.
4. **`*Secret` resolution** — string fields whose **Go name** ends in
   `Secret` are treated as secret-reference names and resolved through
   the configured `secrets.SecretSource` (M3.4.a). By convention the
   YAML key SHOULD also end in `_secret`, but detection is name-based.
   The resolved value lands in the sibling field (the field WITHOUT the
   `Secret` suffix).

After the four layers run, the loader validates that required fields are
populated; missing required fields surface as `ErrMissingRequired`
wrapping the field path.

## Public API

```go
func Load(ctx context.Context, opts ...Option) (*Config, error)

func WithFile(path string) Option
func WithEnvPrefix(prefix string) Option
func WithSecretSource(src secrets.SecretSource) Option
func WithLogger(l Logger) Option

type Config struct {
    Keep     KeepConfig
    Notebook NotebookConfig
}

type KeepConfig struct {
    BaseURL     string // yaml: keep.base_url, env: WATCHKEEPER_KEEP_BASE_URL
    TokenSecret string // yaml: keep.token_secret (secret reference name)
    Token       string // resolved from TokenSecret via SecretSource
}

type NotebookConfig struct {
    DataDir string // yaml: notebook.data_dir, env: WATCHKEEPER_NOTEBOOK_DATA_DIR
}

type Logger interface {
    Log(ctx context.Context, msg string, kv ...any)
}
```

Sentinel errors live in `errors.go`:

- `ErrParseYAML` — file missing, unreadable, or malformed YAML.
- `ErrUnknownField` — strict-mode decode found a key not present on `Config`.
- `ErrMissingRequired` — validation found an empty required field.
- `ErrNoSecretSource` — non-empty `*_secret` with no SecretSource configured.
- `ErrSecretResolutionFailed` — SecretSource returned an error (the
  underlying error is wrapped via `%w` so `errors.Is(err, secrets.ErrSecretNotFound)`
  works through the chain).

All sentinels are matchable via `errors.Is`.

## YAML schema

```yaml
# config.yaml
keep:
  base_url: "https://keep.example.com"
  token_secret: "KEEP_TOKEN" # secret-reference NAME, not the value
notebook:
  data_dir: "/var/lib/watchkeeper/notebook"
```

The `*_secret` convention separates the **reference name** (what
operators put in YAML and check into git via `config.example.yaml`) from
the **resolved value** (what the running process uses). Operators rotate
secrets by changing the value behind the same reference name; no YAML
edit is required.

## Env-var overrides

Every overridable field carries an `env:"NAME"` tag. Set the named env
var to a non-empty value to override the YAML / default for that field:

```bash
export WATCHKEEPER_KEEP_BASE_URL="https://keep-staging.example.com"
```

**Empty env-vars do NOT override.** This is deliberate: a fresh shell
that exports a variable to the empty string (common in CI scripts that
unset secrets defensively) should not silently clear a configured value.

### Multi-tenant prefix

`WithEnvPrefix("ACME_")` prepends `ACME_` to every env-var name:

```go
cfg, _ := config.Load(ctx, config.WithEnvPrefix("ACME_"))
// reads ACME_WATCHKEEPER_KEEP_BASE_URL, etc.
```

Useful when several Watchkeeper deployments share an environment but
need per-tenant config without touching a YAML file.

## Secret resolution

The `*Secret` resolution layer walks `Config` recursively, looking for
string fields whose **Go name** ends in `Secret` (e.g. `TokenSecret`).
Detection is name-based — the YAML tag is irrelevant for triggering
resolution. By convention the YAML key SHOULD also end in `_secret`
(e.g. `yaml:"token_secret"`) for human clarity, but it is not required.

Worked example:

```go
type KeepConfig struct {
    TokenSecret string `yaml:"token_secret"` // Go name ends in "Secret" → resolved
    Token       string `yaml:"-"`            // sibling: receives resolved value
}
```

A field with Go name `MyAPIToken` would NOT trigger resolution even if
its YAML tag ends in `_secret`, because the Go name does not end in
`Secret`.

For each non-empty `*Secret` field, the loader:

1. Calls `src.Get(ctx, value)` on the configured `secrets.SecretSource`.
2. On success, populates the sibling field (`KeepConfig.Token` for the
   `KeepConfig.TokenSecret` reference).
3. On error, wraps the underlying error with `ErrSecretResolutionFailed`
   AND the upstream sentinel (e.g. `secrets.ErrSecretNotFound`) so
   `errors.Is` matches at every layer.

```go
import "github.com/vadimtrunov/watchkeepers/core/pkg/secrets"

src := secrets.NewEnvSource() // resolves from process env vars
cfg, err := config.Load(ctx,
    config.WithFile("config.yaml"),
    config.WithSecretSource(src),
)
```

If a `*_secret` field is non-empty AND no `SecretSource` is wired, the
loader returns `ErrNoSecretSource` rather than silently leaving the
sibling empty. An empty `*_secret` is fine: the loader skips that field.

## Redaction discipline

This package NEVER logs secret values:

- Success paths produce zero log entries.
- Error paths log only the field path (e.g. `Keep.TokenSecret`) and the
  error type — never the secret-reference value, never the resolved
  value.

The `secrets.SecretSource` contract carries the same guarantee for the
stock `secrets.EnvSource`. Custom sources and custom `Logger`
implementations wired via `WithLogger` MUST follow the same rule.

## Example

```go
package main

import (
    "context"
    "log"

    "github.com/vadimtrunov/watchkeepers/core/pkg/config"
    "github.com/vadimtrunov/watchkeepers/core/pkg/secrets"
)

func main() {
    ctx := context.Background()

    cfg, err := config.Load(ctx,
        config.WithFile("/etc/watchkeeper/config.yaml"),
        config.WithEnvPrefix(""), // single-tenant deployment
        config.WithSecretSource(secrets.NewEnvSource()),
    )
    if err != nil {
        log.Fatalf("config: %v", err)
    }

    log.Printf("Keep base URL: %s", cfg.Keep.BaseURL)
    log.Printf("Notebook data dir: %s", cfg.Notebook.DataDir)
    // cfg.Keep.Token holds the resolved secret value — do NOT log it.
}
```

## Out of scope (deferred)

- **Hot reload** — `Load` is a one-shot at boot. A future TASK can wrap
  the loader with `fsnotify` to reload on file change.
- **CLI-flag layer** — env-vars cover all overrides for Phase 1; flags
  add complexity (precedence, parsing) that nothing currently needs.
- **Environment-specific overrides** — a single `config.yaml` per
  deployment; per-environment splits use separate Keep instances.
- **Secret rotation / TTL caching** — secrets are read once at Load.
  A future hardening TASK can add a refresh strategy.
- **Non-string field types** — Phase 1 ships only string fields. The
  env-walk's reflection switch is the extension point for `int`,
  `bool`, etc.
