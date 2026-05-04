// Package keeperslog is the structured Keeper's Log writer — a thin
// wrapper around [keepclient.Client.LogAppend] that adds correlation-ID
// management, OTel trace-context propagation, and a stable JSON
// envelope schema. ROADMAP §M3 → M3.6.
//
// The package exposes a [Writer] type constructed via [New], the
// [LocalKeepClient] interface that [Writer] consumes, an [Event] input
// shape, and two context helpers ([ContextWithCorrelationID],
// [CorrelationIDFromContext]). Every [Writer.Append] call composes the
// caller's event with metadata sourced from the supplied context and
// forwards a [keepclient.LogAppendRequest] to the configured
// [LocalKeepClient].
//
// # Why a wrapper, not a re-implementation
//
// The Keep service already validates correlation-ID format,
// stamps actor / scope from the verified capability claim, and persists
// the row append-only. Re-implementing those concerns here would
// duplicate the server-side surface and risk drift. Instead, the
// writer:
//
//  1. Reads / mints a correlation id (UUID v7 when absent).
//  2. Reads the OTel span context off the request ctx (no-op when
//     unset).
//  3. Mints a fresh per-event id (UUID v7) and a UTC timestamp.
//  4. Composes a stable JSON envelope { event_id, timestamp,
//     trace_id?, span_id?, causation_id?, data? } and ships it to
//     [keepclient.Client.LogAppend] as the row's payload.
//
// Anything else (auth header, transport retries, sentinel-error
// taxonomy) is the keepclient's job.
//
// # Event envelope schema
//
// The JSON shape persisted to `keepers_log.payload` is:
//
//	{
//	  "event_id":     "<uuid-v7>",          // always present
//	  "timestamp":    "<RFC3339Nano UTC>",  // always present
//	  "trace_id":     "<32 hex chars>",     // OTel TraceID, only when valid span on ctx
//	  "span_id":      "<16 hex chars>",     // OTel SpanID,  only when valid span on ctx
//	  "causation_id": "<opaque string>",    // only when Event.CausationID != ""
//	  "data":         { ... }               // only when Event.Payload != nil
//	}
//
// The envelope intentionally carries NO infrastructure-metadata fields
// (no `deployment_id`, `environment`, `host`, `pod`, etc.) per the M2
// design constraint that Keep holds business knowledge only.
// Multi-environment isolation is achieved by running separate Keep
// instances, never by stamping infrastructure context onto rows.
//
// # Correlation-ID propagation
//
// `correlation_id` is the keepclient column that ties related events
// together (e.g. cron-fired and handler-ran for the same logical
// activation). Resolution order on every [Writer.Append]:
//
//  1. If the ctx carries a correlation id (set via
//     [ContextWithCorrelationID]) it is used verbatim.
//  2. Otherwise the writer mints a fresh UUID v7 via the configured
//     [IDGenerator] (overridable via [WithCorrelationIDGenerator]).
//
// Generated correlation ids are NOT pushed back onto the ctx — the
// caller is responsible for plumbing a correlation id forward when one
// is needed across multiple Append calls. The chain origin
// (cron fire, Slack interaction, watchkeeper boot) is the natural
// owner of the id and should call [ContextWithCorrelationID] there.
//
// # Trace-context propagation
//
// The writer reads the OTel span context via
// [trace.SpanContextFromContext]. If the span context is valid (both
// trace_id and span_id non-zero), the lower-case-hex form of each id
// is embedded in the envelope. If the span context is unset / invalid,
// the trace_id and span_id fields are OMITTED from the JSON payload —
// no empty strings on the wire, no banned-key responses from a
// future stricter server. This is a vendor-neutral integration: any
// tracer wired into the Go process that propagates through ctx (OTel,
// Jaeger via OTel bridge, etc.) flows through transparently.
//
// # No infrastructure metadata
//
// The M2 design constraint forbids `deployment_id`, `environment`,
// `host`, `pod`, and similar infrastructure descriptors in Keep rows.
// This package enforces that constraint passively — the
// [Event.Payload] surface is opaque (`map[string]any`), so the
// caller is responsible. The package's own envelope keys are
// constrained to the schema above; if a future change adds an
// infra-flavoured key it should be rejected at code review.
//
// # Out of scope (deferred)
//
//   - Capability-token wiring — the writer consumes whatever
//     [keepclient.Client] the caller hands in. Token issuance is the
//     [capability] package's job (M3.5) and integration is deferred
//     to the M5 harness consumer (where call sites are concrete).
//   - Batched / bufferred appends — every [Writer.Append] forwards a
//     single keepclient call. Backpressure is the caller's concern;
//     callers needing throughput build a buffered channel + worker.
//   - Cross-process correlation-id distribution — Phase 1 is
//     single-process; cross-process correlation flows through the
//     trace-context propagation already covered above.
package keeperslog
