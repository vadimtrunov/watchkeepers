package secrets

import "context"

// SecretSource is the pluggable interface for fetching secret values.
// Callers depend only on this interface; concrete implementations
// ([EnvSource], and future VaultSource / AWS Secrets Manager adapters)
// satisfy it without touching the caller.
//
// # Redaction discipline
//
// Implementations MUST NEVER log secret values — not on success, not on
// error, not in any diagnostic field. The Logger (if wired) receives only
// the key name and the error type. Future custom Logger implementations
// must follow the same rule. This contract is unconditional: even an
// "empty" value is a value and must not appear in any log payload.
//
// # Key contract
//
// An empty key ("") is always invalid and returns [ErrInvalidKey]
// synchronously without touching the backing store.
//
// # Context contract
//
// Implementations must honour ctx cancellation. The minimum expected
// behaviour is a pre-check: if ctx.Err() != nil the call returns
// ctx.Err() before any I/O. Future HTTP-backed implementations
// (VaultSource, AWS SSM) will thread ctx into the outbound request.
type SecretSource interface {
	Get(ctx context.Context, key string) (string, error)
}

// Logger is the diagnostic sink wired in via [WithLogger]. The shape
// mirrors the cron and notebook Logger interfaces: a single
// Log(ctx, msg, kv...) method so callers can substitute structured
// loggers (e.g. an slog wrapper) without losing type compatibility.
//
// The variadic kv slice carries flat key,value pairs
// ("key", keyName, "err", err). A nil logger silently drops the message
// — the package never panics on a nil logger.
//
// IMPORTANT: implementations must never log secret values. Only key names
// and error descriptions are acceptable log fields. See [SecretSource]
// redaction discipline.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// Option configures an [EnvSource] at construction time. Pass options to
// [NewEnvSource]; later options override earlier ones for the same field.
type Option func(*config)

// config is the internal mutable bag the [Option] callbacks populate.
type config struct {
	logger Logger
}

// WithLogger wires a diagnostic sink onto the returned [*EnvSource].
// When set, Get calls Log on error paths only — never on success.
// A nil logger argument is a no-op so callers can always pass through
// whatever logger they have without a nil-guard at the call site.
func WithLogger(l Logger) Option {
	return func(c *config) {
		if l != nil {
			c.logger = l
		}
	}
}
