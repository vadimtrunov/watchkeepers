package coordinator

// record_watch_order — Coordinator write tool (M8.3).
//
// Resolution order (handler closure body):
//
//  1. ctx.Err() pre-check (no recorder call on cancel).
//  2. Summary arg → typed string + non-empty + rune-length cap.
//     Refusal text NEVER echoes the raw value.
//  3. Due-at arg → optional ISO-8601 timestamp; reject malformed or
//     non-UTC offsets (UTC-only discipline mirrors the M8.2.b
//     `updated < -Nd` convention).
//  4. Source-ref arg → optional opaque caller-supplied trace string
//     (e.g. the Slack `ts` of the lead's DM) with rune-length cap.
//     Refusal text NEVER echoes the raw value.
//  5. Persist via [WatchOrderRecorder.Record] with the parsed
//     [WatchOrder] payload + the per-fire clock.
//  6. Project the returned id + clock-stamped `recorded_at` into the
//     success Output. The agent's lead-DM round-trip uses the id +
//     persistence ack to confirm the order was logged.
//
// Audit discipline: handler returns a [agentruntime.ToolResult] only;
// the runtime's tool-result reflection layer (M5.6.b) is the audit
// boundary. NO direct keeperslog.Append from this file (asserted via
// source-grep AC). The downstream [WatchOrderRecorder] composes the
// notebook write + its own audit emit; the handler stays a thin
// validation + projection layer.
//
// PII discipline: every refusal text uses the [recordWatchRefusalPrefix]
// + constant suffix; raw user-supplied arg values NEVER appear.
// `summary` is INTENTIONALLY OMITTED from the success Output scope
// echo — the agent has it in its call args and the M5.6.b reflector
// would otherwise ingest a Watch Order body verbatim on the success
// path. `source_ref` is similarly omitted; the Output surfaces only a
// `source_ref_present` boolean so the agent can audit its own
// trace-passing discipline without re-emitting the opaque string.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// RecordWatchOrderName is the manifest tool name the Coordinator
// dispatcher registers this handler under. Mirrors the toolset entry
// in `deploy/migrations/028_coordinator_manifest_v5_seed.sql`.
const RecordWatchOrderName = "record_watch_order"

// recordWatchRefusalPrefix is the leading namespace for every
// [agentruntime.ToolResult.Error] string this handler surfaces. Per
// the M8.2.b convention each Coordinator handler carries a per-tool
// prefix const so package-scoped prefixes do not collide.
const recordWatchRefusalPrefix = "coordinator: " + RecordWatchOrderName + ": "

// record_watch_order argument keys.
const (
	// ToolArgSummary carries the natural-language Watch Order body the
	// agent distilled from the lead's DM. Required; non-empty; rune
	// count ≤ [maxWatchOrderSummaryChars].
	ToolArgSummary = "summary"

	// ToolArgDueAt carries an optional ISO-8601 due-by timestamp
	// (e.g. `"2026-05-20T17:00:00Z"`). Optional; when present must
	// parse as RFC3339 in UTC (suffix `Z` or `+00:00`); other offsets
	// reject — the storage layer pins UTC throughout (mirrors the
	// M8.2.b JQL `updated < -Nd` convention).
	ToolArgDueAt = "due_at"

	// ToolArgSourceRef carries an optional opaque trace string the
	// caller can use to correlate the recorded order back to its
	// source (typically the Slack `ts` of the lead's DM). Optional;
	// rune count ≤ [maxWatchOrderSourceRefChars]. The handler does NOT
	// validate the shape — the value is opaque to record_watch_order;
	// downstream surfaces that consume it (e.g. an operator audit UI)
	// own the shape contract.
	ToolArgSourceRef = "source_ref"
)

// Length caps for record_watch_order.
const (
	// maxWatchOrderSummaryChars caps the summary rune-length. 2000
	// covers a multi-paragraph order while keeping the notebook
	// `entry.content` row size predictable (the per-tenant DB is a
	// SQLite file; bounding the per-row size keeps backup + restore
	// times predictable).
	maxWatchOrderSummaryChars = 2000

	// maxWatchOrderSourceRefChars caps the source-ref rune-length. 500
	// covers a Slack `ts` + a permalink path with headroom; large
	// values would indicate a misuse (e.g. the agent passed the full
	// message body, which belongs in summary).
	maxWatchOrderSourceRefChars = 500
)

// WatchOrder is the parsed Watch Order payload [NewRecordWatchOrderHandler]
// hands off to the [WatchOrderRecorder]. Held as a struct (rather than
// a long parameter list) so a future addition (e.g. priority, tags)
// adds a field without churning every implementor.
//
// All fields are validated by the handler before this struct is
// constructed — implementations of [WatchOrderRecorder.Record] MAY
// trust them without re-validating.
type WatchOrder struct {
	// Summary is the natural-language Watch Order body. Non-empty,
	// rune count ≤ [maxWatchOrderSummaryChars].
	Summary string

	// DueAt is the optional due-by timestamp in UTC. Zero value means
	// "no due_at supplied"; non-zero is guaranteed UTC by the handler.
	DueAt time.Time

	// SourceRef is the optional opaque caller-supplied trace string.
	// Empty when the caller omitted the arg.
	SourceRef string
}

// WatchOrderRecord is the typed result returned by
// [WatchOrderRecorder.Record]. Iter-1 codex Minor: returning a
// typed struct (vs `(id string, err error)`) lets the recorder
// supply the authoritative `RecordedAt` — the storage layer's
// commit-time clock — instead of the handler fabricating it from
// its own clock pre-persistence. The DM round-trip ack the agent
// sends to the lead echoes [RecordedAt] verbatim from the
// recorder so the value the lead sees matches the value the
// notebook stored.
type WatchOrderRecord struct {
	// ID is the canonical UUID v7 of the persisted notebook row.
	ID string

	// RecordedAt is the wall-clock at which the recorder committed
	// the row, in UTC. Production wiring binds this to the
	// `notebook.Entry.CreatedAt` value at commit time so retries /
	// internal-clock skew never cause the DM ack to drift from
	// storage.
	RecordedAt time.Time
}

// WatchOrderRecorder is the single-method interface
// [NewRecordWatchOrderHandler] consumes for the persistence side
// effect. The interface lives at the consumer (this package) per the
// project's "interfaces belong to the consumer" convention, mirroring
// [JiraFieldUpdater] / [JiraSearcher] / [SlackIMOpener] / etc.
//
// Production wiring (M8.3 deferred wiring): a `*WatchOrderStore` in
// the future Coordinator-binary wiring helper composes an [Embedder]
// + `*notebook.DB` and writes a [notebook.CategoryPendingTask] entry
// whose `Subject` carries a short title derived from
// [WatchOrder.Summary], `Content` carries the full Summary +
// optional DueAt + SourceRef, and `ActiveAfter` is zero (Watch
// Orders are surface-able immediately; the 24h cooling-off applies to
// lessons, not pending tasks). The Embedder folds Summary into a
// vector so future Recall queries land on semantically-related
// orders. The interface stays single-method so the Coordinator test
// fakes never have to stub the Embedder.
//
// The recorder's returned [WatchOrderRecord.RecordedAt] MUST be in
// UTC; the handler echoes it verbatim into the success Output.
type WatchOrderRecorder interface {
	Record(ctx context.Context, ord WatchOrder) (WatchOrderRecord, error)
}

// defaultRecordWatchOrderNow is the production clock the public
// factory binds. Tests reach for the unexported
// [newRecordWatchOrderHandlerWithClock] to substitute a fixed
// `time.Time` so the per-test override stays scoped to one handler
// instance — no package-level mutable shared state, no race under
// `-parallel`. Mirrors the M8.2.b/c clock-injection precedent.
var defaultRecordWatchOrderNow = time.Now

// NewRecordWatchOrderHandler constructs the [agentruntime.ToolHandler]
// the Coordinator dispatcher registers under [RecordWatchOrderName].
// Panics on a nil `recorder` per the M*.c.* / M8.2 "panic on nil
// deps" discipline.
//
// Args contract (read from [agentruntime.ToolCall.Arguments]):
//
//   - `summary`    (string, required): non-empty Watch Order body,
//     rune count ≤ [maxWatchOrderSummaryChars].
//   - `due_at`     (string, optional): RFC3339 UTC timestamp (suffix
//     `Z` or `+00:00`). Other offsets refuse.
//   - `source_ref` (string, optional): opaque trace string, rune count
//     ≤ [maxWatchOrderSourceRefChars]. The handler does NOT validate
//     the shape.
//
// Refusal contract — returned via [agentruntime.ToolResult.Error]
// (NOT a Go error so the agent can re-plan; mirrors the M8.2.a/b/c
// channel discipline). Refusal text NEVER echoes a raw arg value.
//
// Output (success) — keys on the returned [agentruntime.ToolResult.Output]:
//
//   - `watch_order_id`      (string): canonical UUID v7 from the
//     [WatchOrderRecorder]; the agent uses this id in the lead-DM
//     round-trip ("Recorded as <id> — anything to amend?").
//   - `recorded_at`         (string): RFC3339 UTC clock-stamp at
//     handler entry; the agent surfaces this in the round-trip so the
//     lead can disambiguate two consecutive orders.
//   - `due_at_recorded`     (string): RFC3339 UTC echo of the parsed
//     due-at — present iff the arg was supplied AND parsed cleanly.
//     Empty string when the arg was omitted.
//   - `source_ref_present`  (bool): true iff the caller supplied a
//     non-empty `source_ref`. The opaque string itself is
//     INTENTIONALLY OMITTED from Output — same M8.2.b/c lesson #10
//     PII discipline.
//   - `summary_chars`       (int): rune count of the recorded summary.
//     The summary text itself is INTENTIONALLY OMITTED from Output —
//     the agent already has it in [agentruntime.ToolCall.Arguments]
//     and the M5.6.b reflector would otherwise ingest a Watch Order
//     body verbatim on the success path.
//
// Forwarded errors — returned as Go `error`:
//
//   - [WatchOrderRecorder.Record] errors wrap with
//     `"coordinator: record_watch_order: %w"` so the M5.6.b reflector
//     layer can ingest the type information without re-prefixing.
func NewRecordWatchOrderHandler(recorder WatchOrderRecorder) agentruntime.ToolHandler {
	return newRecordWatchOrderHandlerWithClock(recorder, defaultRecordWatchOrderNow)
}

// newRecordWatchOrderHandlerWithClock is the test-internal factory
// that lets tests inject a fixed clock without mutating package
// state. Same nil-recorder panic discipline; clock MUST also be
// non-nil.
func newRecordWatchOrderHandlerWithClock(
	recorder WatchOrderRecorder,
	clock func() time.Time,
) agentruntime.ToolHandler {
	if recorder == nil {
		panic("coordinator: NewRecordWatchOrderHandler: recorder must not be nil")
	}
	if clock == nil {
		panic("coordinator: NewRecordWatchOrderHandler: clock must not be nil")
	}
	return func(ctx context.Context, call agentruntime.ToolCall) (agentruntime.ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return agentruntime.ToolResult{}, err
		}

		summary, refusal := readWatchOrderSummaryArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		dueAt, dueAtPresent, refusal := readWatchOrderDueAtArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		sourceRef, refusal := readWatchOrderSourceRefArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		// Iter-1 codex Minor: the handler's local clock is used only
		// as a fallback (when the recorder returns a zero RecordedAt
		// — a fail-soft for an implementation that has not yet been
		// upgraded). Production wiring binds the recorder to the
		// notebook commit-time clock and returns a non-zero value;
		// the test-internal `clock` parameter then drives the
		// fallback path so test assertions are deterministic.
		id, recordedAt, err := dispatchWatchOrderRecord(ctx, recorder, WatchOrder{
			Summary:   summary,
			DueAt:     dueAt,
			SourceRef: sourceRef,
		}, clock)
		if err != nil {
			return agentruntime.ToolResult{}, err
		}

		out := map[string]any{
			"watch_order_id":     id,
			"recorded_at":        recordedAt.Format(time.RFC3339),
			"due_at_recorded":    "",
			"source_ref_present": sourceRef != "",
			"summary_chars":      runeLen(summary),
		}
		if dueAtPresent {
			out["due_at_recorded"] = dueAt.UTC().Format(time.RFC3339)
		}
		return agentruntime.ToolResult{Output: out}, nil
	}
}

// dispatchWatchOrderRecord calls the recorder and projects its result
// into (id, recordedAt, err). On a non-zero
// [WatchOrderRecord.RecordedAt] the recorder's clock is authoritative;
// on a zero value (recorder didn't supply one) the handler's local
// `clock` provides the fallback so the success Output always carries
// a meaningful timestamp. Surfaces recorder errors with the standard
// `coordinator: record_watch_order:` wrap.
func dispatchWatchOrderRecord(
	ctx context.Context,
	recorder WatchOrderRecorder,
	ord WatchOrder,
	clock func() time.Time,
) (string, time.Time, error) {
	res, err := recorder.Record(ctx, ord)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("coordinator: record_watch_order: %w", err)
	}
	recordedAt := res.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = clock()
	}
	return res.ID, recordedAt.UTC(), nil
}

// readWatchOrderSummaryArg projects the `summary` arg into a typed
// string. Returns (summary, "") on success; ("", refusalText) on
// validation failure. Refusal text NEVER echoes the raw value.
func readWatchOrderSummaryArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgSummary]
	if !present {
		return "", recordWatchRefusalPrefix + "missing required arg: " + ToolArgSummary
	}
	str, ok := raw.(string)
	if !ok {
		return "", recordWatchRefusalPrefix + ToolArgSummary + " must be a string"
	}
	if str == "" {
		return "", recordWatchRefusalPrefix + ToolArgSummary + " must be non-empty"
	}
	if runeLen(str) > maxWatchOrderSummaryChars {
		return "", recordWatchRefusalPrefix + ToolArgSummary +
			" must be ≤ " + strconv.Itoa(maxWatchOrderSummaryChars) + " characters (rune count)"
	}
	return str, ""
}

// readWatchOrderDueAtArg projects the optional `due_at` arg into a
// typed [time.Time] in UTC. Returns (zero, false, "") when the arg
// was omitted; (t, true, "") on success; (zero, false, refusalText)
// on validation failure. Refusal text NEVER echoes the raw value.
//
// UTC-only rationale: storing local-zone timestamps in a multi-tenant
// audit row is a category of bug that surfaces months later when the
// operator audits an order placed in a non-default timezone. Pinning
// UTC at the boundary makes the storage layer's `created_at` /
// `due_at` comparison trivially correct.
func readWatchOrderDueAtArg(args map[string]any) (time.Time, bool, string) {
	raw, present := args[ToolArgDueAt]
	if !present {
		return time.Time{}, false, ""
	}
	str, ok := raw.(string)
	if !ok {
		return time.Time{}, false, recordWatchRefusalPrefix + ToolArgDueAt + " must be a string"
	}
	if str == "" {
		return time.Time{}, false, recordWatchRefusalPrefix + ToolArgDueAt + " must be non-empty when present"
	}
	t, err := time.Parse(time.RFC3339, str)
	if err != nil {
		return time.Time{}, false, recordWatchRefusalPrefix + ToolArgDueAt +
			" must be an RFC3339 UTC timestamp (e.g. 2026-05-20T17:00:00Z)"
	}
	if _, off := t.Zone(); off != 0 {
		return time.Time{}, false, recordWatchRefusalPrefix + ToolArgDueAt +
			" must be in UTC (suffix Z or +00:00)"
	}
	return t.UTC(), true, ""
}

// readWatchOrderSourceRefArg projects the optional `source_ref` arg
// into a typed string. Returns ("", "") when the arg was omitted;
// (ref, "") on success; ("", refusalText) on validation failure.
// Refusal text NEVER echoes the raw value.
//
// Iter-1 codex Minor: an explicitly-supplied empty string MUST
// refuse rather than collapse to "omitted". Collapsing creates an
// ambiguity in the `source_ref_present` output flag — "supplied but
// blank" would read identically to "omitted entirely", which breaks
// the trace/audit contract documented in the V5 system prompt.
// Mirrors the [readWatchOrderDueAtArg] "non-empty when present"
// discipline.
func readWatchOrderSourceRefArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgSourceRef]
	if !present {
		return "", ""
	}
	str, ok := raw.(string)
	if !ok {
		return "", recordWatchRefusalPrefix + ToolArgSourceRef + " must be a string"
	}
	if str == "" {
		return "", recordWatchRefusalPrefix + ToolArgSourceRef + " must be non-empty when present"
	}
	if runeLen(str) > maxWatchOrderSourceRefChars {
		return "", recordWatchRefusalPrefix + ToolArgSourceRef +
			" must be ≤ " + strconv.Itoa(maxWatchOrderSourceRefChars) + " characters (rune count)"
	}
	return str, ""
}
