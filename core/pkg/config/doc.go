// Package config loads operator configuration from a layered source
// stack and surfaces a strongly-typed [*Config] struct to consumers.
// ROADMAP §M3 → M3.4 → M3.4.b.
//
// # Layer order
//
// The loader applies four layers, in order, from the lowest precedence
// to the highest:
//
//  1. Built-in defaults baked into the package (e.g. resolving
//     `$WATCHKEEPER_DATA/notebook` for [NotebookConfig.DataDir] when
//     `WATCHKEEPER_DATA` is set in the environment).
//  2. The YAML file supplied via [WithFile], decoded in strict mode
//     (`yaml.Decoder.KnownFields(true)`) so unknown keys surface as
//     [ErrUnknownField] rather than silently dropping. An empty path is
//     treated as "no file"; a non-empty path that does not exist is an
//     error.
//  3. Env-var overrides for any field whose struct tag carries
//     `env:"NAME"`. Empty env-var values are treated as "not set" and do
//     NOT override earlier layers — set the variable to a non-empty
//     string to take effect. [WithEnvPrefix] lets multi-tenant
//     deployments namespace the variable names.
//  4. `*_secret` resolution. For each string field whose YAML key ends in
//     `_secret`, the value is treated as a secret-reference name and
//     passed to the configured [secrets.SecretSource] (wired via
//     [WithSecretSource]); the resolved value lands in the sibling
//     non-secret field (the field WITHOUT the `Secret` suffix).
//
// After the four layers run, the loader validates that all required
// fields are populated; missing required fields return
// [ErrMissingRequired] wrapped with the offending field path.
//
// # Sentinel errors
//
// [ErrParseYAML]              — YAML file missing, unreadable, or malformed.
// [ErrUnknownField]           — strict YAML decode found an unknown key.
// [ErrMissingRequired]        — validation found an empty required field.
// [ErrNoSecretSource]         — non-empty `*_secret` with no SecretSource.
// [ErrSecretResolutionFailed] — SecretSource.Get returned an error
// (the underlying error is wrapped via `%w` so callers can chain match).
//
// All sentinels are matchable via [errors.Is].
//
// # Functional options
//
// [Load] takes zero or more [Option] values to configure the loader:
//
//   - [WithFile](path) — point at a `config.yaml`; empty path → skip the
//     file layer. A non-empty path that does not exist is an error.
//   - [WithEnvPrefix](prefix) — prepend `prefix` to every env-var name
//     declared in struct tags; empty prefix → no prepending.
//   - [WithSecretSource](src) — wire a [secrets.SecretSource] for
//     `*_secret` resolution; nil source → silent no-op (consistent with
//     the notebook / cron / secrets convention).
//   - [WithLogger](l) — wire a diagnostic [Logger]; nil logger → silent
//     no-op.
//
// # Redaction discipline
//
// This package NEVER logs secret values. The [Logger] (if wired)
// receives only the field path and the error type for any secret-related
// diagnostic. The [secrets.SecretSource] contract carries the same
// guarantee — see the [secrets] package godoc for the unconditional
// rule. Future custom Logger implementations wired via [WithLogger]
// MUST follow the same rule: log only key names and error descriptions.
//
// # Out of scope
//
// Hot reload (config is loaded once at boot; future TASK can add
// [fsnotify]); CLI-flag layer (env-vars cover all overrides for Phase 1);
// environment-specific overrides (a single `config.yaml` per deployment;
// multi-env via separate Keep instances per the project's architectural
// decision); secret rotation / TTL caching (secrets are read once at
// Load).
package config
