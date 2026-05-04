// Package secrets defines a small, pluggable abstraction for fetching
// secret values. ROADMAP §M3 → M3.4 → M3.4.a.
//
// The central interface is [SecretSource]:
//
//	type SecretSource interface {
//	    Get(ctx context.Context, key string) (string, error)
//	}
//
// Phase 1 ships one concrete implementation, [EnvSource], which resolves
// secrets from environment variables via [os.LookupEnv]. Future
// milestones may add VaultSource, AWS SSM adapters, or other backends;
// all will satisfy the same interface without touching callers.
//
// # Vault-readiness
//
// The interface is intentionally minimal so HTTP-backed sources (Vault,
// AWS Secrets Manager) can implement it directly. The ctx parameter is
// already threaded through to every Get call so future implementations
// can honour HTTP timeouts and cancellation without changing the
// interface.
//
// # Redaction discipline
//
// This package NEVER logs secret values — not on success, not on error,
// not in any diagnostic field. Error-path log entries contain only the
// key name and the error description. The success path produces zero log
// entries. This contract is unconditional and must be honoured by any
// custom [Logger] implementation wired via [WithLogger]:
//
//   - Do not log the return value of Get.
//   - Do not log values retrieved from environment variables or other
//     backing stores, even transiently.
//   - Log only key names and error types.
//
// # Sentinel errors
//
// [ErrSecretNotFound] — key absent or holds an empty value.
// [ErrInvalidKey]    — empty key string (always a programmer error).
//
// Both are matchable via [errors.Is].
//
// # Functional options
//
// [EnvSource] is constructed via [NewEnvSource] with zero or more
// [Option] values. [WithLogger] wires a [Logger] (single-method
// Log(ctx, msg, kv...)) for error diagnostics. A nil logger argument is
// always a no-op — the package never panics on a nil logger.
package secrets
