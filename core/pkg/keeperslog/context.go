package keeperslog

import "context"

// correlationIDKey is the unexported context-key type for the
// correlation id. Using a typed unexported key (per the
// `revive:context-keys-type` rule) prevents accidental collision with
// keys from other packages.
type correlationIDKey struct{}

// ContextWithCorrelationID returns a derived context carrying `id` as
// the correlation identifier consumed by [Writer.Append]. An empty
// `id` is a no-op — the original ctx is returned unchanged so a
// downstream Append still auto-generates a fresh correlation id.
//
// Callers at chain origins (cron fire, Slack interaction, watchkeeper
// boot) should mint a UUID v7 and call this once at the top of their
// handler; the writer reads the value on every subsequent Append.
func ContextWithCorrelationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, correlationIDKey{}, id)
}

// CorrelationIDFromContext returns the correlation id stored on `ctx`
// via [ContextWithCorrelationID]. The boolean second return is `true`
// when a value was found, `false` otherwise (mirrors the standard
// `value, ok := m[k]` shape so callers can distinguish "absent" from
// "empty").
func CorrelationIDFromContext(ctx context.Context) (string, bool) {
	v := ctx.Value(correlationIDKey{})
	if v == nil {
		return "", false
	}
	id, ok := v.(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}
