// Package wklog is the Watchkeeper structured-logging surface (M10.1).
//
// Every Watchkeeper subsystem logs through a [*slog.Logger] returned by
// [New]. The returned logger writes JSON records to stderr with the
// following pre-baked attributes:
//
//   - time (RFC3339 with nanosecond precision; slog default)
//   - level (DEBUG / INFO / WARN / ERROR)
//   - msg
//   - subsystem (the name passed to New — e.g. "keep.outbox", "spawn")
//   - correlation_id (when [WithCorrelationID] has been attached to the
//     logging call's [context.Context])
//
// # Level configuration
//
// Levels are resolved from environment variables at logger construction:
//
//   - WK_LOG_LEVEL_<UPPERCASE_SUBSYSTEM>: overrides for a single
//     subsystem. Dots in the subsystem name become underscores
//     (e.g. "keep.outbox" → WK_LOG_LEVEL_KEEP_OUTBOX).
//   - WK_LOG_LEVEL: fallback for any subsystem without a per-subsystem
//     override.
//   - Default INFO when neither is set.
//
// Recognised level strings (case-insensitive): debug, info, warn,
// warning, error. Unknown values fall back to INFO and emit one
// startup warning record on stderr describing the offending env var.
//
// # Correlation IDs
//
// Cross-subsystem request tracing is plumbed via [context.Context]:
//
//	ctx = wklog.WithCorrelationID(ctx, "req-123")
//	logger.InfoContext(ctx, "handled request", "route", "/v1/search")
//	// → {"time":"...","level":"INFO","msg":"handled request",
//	//    "subsystem":"keep.server","correlation_id":"req-123",
//	//    "route":"/v1/search"}
//
// Use [CorrelationIDFromContext] to read the id (e.g. when forwarding to
// downstream HTTP calls).
//
// Limitation (iter-1 review M2): the correlation_id attribute is
// added at record-Handle time. Calling [slog.Logger.With] on a wklog
// logger preserves correlation_id at the JSON root; calling
// [slog.Logger.WithGroup] does NOT — slog's group semantics nest every
// subsequent record-level attribute under the group, which includes
// the correlation_id wklog injects at Handle time. Callers that need
// correlation_id at the JSON root MUST avoid WithGroup, or accept that
// it nests under the group they chose. The behaviour is regression-
// pinned by `wklog_test.go:TestWithGroup_CorrelationIDNestsUnderGroup`.
//
// # Audit discipline
//
// wklog is for operator-facing diagnostics ONLY. The Keep's audit chain
// (keeperslog) is the durable, event-shaped record; wklog must never
// claim audit semantics. Test files in M9-onward source-grep their step
// implementations to assert no keeperslog.Append calls leaked into the
// non-audit code path; wklog has no equivalent invariant because it is
// safe to call from any context.
//
// # PII discipline
//
// Callers MUST NOT pass raw user content, OAuth tokens, manifest
// payloads, or PII into log attributes. wklog does not redact —
// redaction is the caller's responsibility per the M7.* PII canary
// harness pattern (see docs/lessons/M7.md).
package wklog
