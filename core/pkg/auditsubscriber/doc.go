// Package auditsubscriber bridges the M9 in-process [eventbus.Bus] to
// the [keeperslog.Writer] audit-log surface. It is the single producer
// of `keepers_log` rows for the Tool-Registry / Authoring / Approval /
// Local-Patch / Hosted-Export / Tool-Share lifecycle events listed by
// M9.7 in `docs/ROADMAP-phase1.md`.
//
// # Architectural inversion
//
// Every M9 emitter package (`toolregistry`, `approval`, `localpatch`,
// `hostedexport`, `toolshare`) publishes structured payloads on the
// [eventbus.Bus] but is FORBIDDEN from importing [keeperslog] or
// calling [keeperslog.Writer.Append]. The boundary is enforced
// per-package by an `audit_grep_test.go` source-grep AC. The audit
// write lives HERE — in this package alone — so the emitters stay
// ignorant of audit persistence.
//
// This is the inverse-shape companion test: a positive
// `TestAuditGrep_KeeperslogImportPresent_AppendCallPresent` in this
// package asserts the bridge IS wired (production code DOES import
// `keeperslog` and DOES call `.Append`). Together the two tests pin
// the one-way audit flow:
//
//	emitter → eventbus.Bus → auditsubscriber → keeperslog.Writer → keepers_log
//
// # Topic vocabulary
//
// Eleven topics are subscribed verbatim today:
//
//	toolregistry.source_synced          → keeperslog event_type "source_synced"
//	toolregistry.source_failed          → keeperslog event_type "source_failed"
//	toolregistry.tool_shadowed          → keeperslog event_type "tool_shadowed"
//	approval.tool_proposed              → keeperslog event_type "tool_proposed"
//	approval.tool_approved              → keeperslog event_type "tool_approved"
//	approval.tool_rejected              → keeperslog event_type "tool_rejected"
//	approval.tool_dry_run_executed      → keeperslog event_type "tool_dry_run_executed"
//	localpatch.local_patch_applied      → keeperslog event_type "local_patch_applied"
//	hostedexport.hosted_tool_exported   → keeperslog event_type "hosted_tool_exported"
//	toolshare.tool_share_proposed       → keeperslog event_type "tool_share_proposed"
//	toolshare.tool_share_pr_opened      → keeperslog event_type "tool_share_pr_opened"
//
// The roadmap M9.7 entry lists 19 event names; eight are deferred (no
// emitter has landed yet) — see the scope-boundary section of
// `docs/lessons/M9.md` (M9.7 entry).
//
// # Resolution order (per topic dispatch)
//
//  1. Topic worker pops the envelope; passes it to the subscriber's
//     wrapped handler.
//  2. Handler type-asserts the bus payload to the expected struct.
//     On mismatch, the optional [Logger] sees a metadata-only entry
//     (topic + expected type + actual `%T`) and the dispatch returns —
//     the event is dropped, NOT retried.
//  3. Handler extracts the payload's `CorrelationID` field and stamps
//     it onto the ctx via [keeperslog.ContextWithCorrelationID]. An
//     empty value is a no-op (the writer mints a fresh UUID v7).
//  4. Handler invokes [Writer.Append] with the closed-set audit-
//     vocabulary `EventType` and the verbatim payload struct as
//     `Payload`. On error, the optional [Logger] sees a metadata-only
//     entry (topic + event_type + err_type) and the dispatch returns —
//     best-effort audit, do NOT block the topic worker.
//
// # PII discipline
//
// Each emitter package has already applied its own PII-allowlist to
// the bus payload (see e.g. [localpatch.LocalPatchApplied] doc-block).
// This subscriber passes the payload through to [keeperslog.Writer]
// VERBATIM — it does NOT add, strip, or transform fields. The
// invariant: if a field is safe on the bus, it is safe in the
// `keepers_log.payload` JSON envelope. If the upstream PII boundary
// changes, the canary suite in `piicanary_test.go` will surface it.
//
// Diagnostic [Logger] entries NEVER carry payload bodies — only
// topic name, event type, error type, and the offending payload's
// Go type. Same discipline as [keeperslog.Logger].
//
// # Concurrency
//
// A [Subscriber] is safe for concurrent use after [Subscriber.Start]
// returns nil. The eventbus dispatches handlers sequentially within a
// topic; per-topic order is preserved. Cross-topic order is NOT
// guaranteed (see [eventbus.Bus] godoc).
//
// # Lifecycle
//
// [Subscriber.Start] subscribes to all eleven topics in a single
// atomic transaction: on any [Bus.Subscribe] failure, every prior
// subscription is unsubscribed before returning the wrapped error.
// [Subscriber.Stop] unsubscribes every handler; it is idempotent.
// Once Stop has been called, Start returns [ErrStopped] — the
// Subscriber is single-use.
package auditsubscriber
