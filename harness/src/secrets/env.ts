/**
 * Watchkeeper TS-side secrets adapter (M5.7.a).
 *
 * Pluggable seam through which the harness reads credentials. The
 * concrete {@link EnvSecretSource} wraps `process.env`; future seams
 * (Vault, AWS Secrets Manager, harnessrpc-bridged Go-core source)
 * implement the same {@link SecretSource} interface so call sites do
 * not change when the backing store does.
 *
 * The shape mirrors the Go-side `core/pkg/secrets.SecretSource`
 * (`Get(ctx, key) -> (string, error)`); the TS counterpart drops the
 * `ctx` argument and replaces the `(value, error)` pair with
 * `string | undefined` because:
 *
 *   - `process.env` reads are synchronous and infallible — there is no
 *     I/O to cancel and no error class to plumb.
 *   - The harness never logs secret values; the redaction discipline
 *     from the Go side is preserved by virtue of having no logger here.
 *
 * Crucially, this file is the SOLE production-code site allowed to
 * carry the `ANTHROPIC_API_KEY` literal — the M5.7.a grep-invariant CI
 * check (see `core/pkg/llm/anthropic_key_invariant_test.go`) fails the
 * build if the literal appears anywhere else under `core/**\/*.go`,
 * `harness/src/**\/*.ts`, `tools-builtin/src/**\/*.ts`, or
 * `cli/**\/*.go` (tests excluded). When a future seam needs the key,
 * it must consume this adapter rather than re-spell the literal.
 */

/**
 * Minimal pluggable interface for fetching a named secret.
 *
 * Returns the raw string value when the secret is present (including
 * the empty string — see {@link EnvSecretSource.get} for the explicit
 * empty-value contract), or `undefined` when the secret is not set.
 *
 * Mirrors the Go `core/pkg/secrets.SecretSource` shape minus the
 * `ctx` argument and the typed-error return; the TS implementation is
 * synchronous and infallible.
 */
export interface SecretSource {
  /**
   * Look up the secret named `name`. Returns the raw string when set,
   * `undefined` when unset. Implementations must NEVER log the
   * returned value.
   */
  get(name: string): string | undefined;
}

/**
 * {@link SecretSource} implementation backed by `process.env`. The
 * sole production-code call site for the `ANTHROPIC_API_KEY` literal
 * is the boot path in `harness/src/index.ts`, which passes the literal
 * through {@link get} after constructing this class.
 *
 * # Empty-string semantics
 *
 * Diverges from the Go `EnvSource`: this adapter returns `""` (empty
 * string) when the env var is set to the empty string, rather than
 * collapsing it to `undefined`. The harness boot path treats `""` as
 * "no usable key" via a length check, so the two implementations agree
 * on the user-visible behaviour while keeping the lower-level contract
 * faithful to `process.env`'s actual shape (which surfaces empty
 * values as `""` distinct from `undefined`). Callers that need the Go
 * collapse can post-filter with `value === undefined || value === ""`.
 */
export class EnvSecretSource implements SecretSource {
  /**
   * Read the env var named `name`. Returns `process.env[name]`
   * verbatim, including `""` when the var is set to empty.
   */
  public get(name: string): string | undefined {
    return process.env[name];
  }
}

/**
 * Convenience wrapper that reads the Claude Code API-key secret from
 * the supplied {@link SecretSource}. Centralised here so the
 * `ANTHROPIC_API_KEY` literal stays inside this single allowlisted
 * production-code file (see module doc-comment for the M5.7.a
 * grep-invariant contract).
 *
 * Returns the raw secret value when set to a non-empty string,
 * otherwise `undefined`. Empty-string env values collapse to
 * `undefined` here so the boot-path "is the key usable?" check stays
 * a single `=== undefined` comparison; callers needing to distinguish
 * unset-vs-empty can drop down to `source.get(...)` directly.
 */
export function getAnthropicApiKey(source: SecretSource): string | undefined {
  const v = source.get("ANTHROPIC_API_KEY");
  if (v === undefined || v === "") return undefined;
  return v;
}
