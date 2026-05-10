// compensator.go defines the M7.3.b [Compensator] optional interface
// and the closed-set audit-event vocabulary the [Runner] emits when it
// rolls back a saga whose later step failed. Steps that implement
// [Compensator] participate in reverse-order rollback; steps that do
// NOT implement it are silently skipped (their forward [Step.Execute]
// had no compensable side effect — e.g. M7.1.c.c BotProfile, which is
// a watchkeeper-row write that the M7.1.c.a CreateApp.Compensate
// teardown of the Slack App makes moot anyway).
//
// # M7.3.b foundation only — no concrete impls
//
// This file ships the seam + the [Runner.Run] reverse-rollback path
// + the audit vocabulary; M7.3.c lands the per-step Compensate
// implementations (CreateApp teardown, OAuthInstall revoke,
// NotebookProvision archive, RuntimeLaunch teardown). A saga whose
// steps do not yet implement [Compensator] aborts with the existing
// `saga_failed` chain plus a single trailing `saga_compensated`
// summary row (zero compensations attempted), so M7.3.b is wire-
// compatible with the M7.1.c–.e step set already on `main`.
//
// # Best-effort rollback discipline
//
// When a [Compensator.Compensate] call returns non-nil the [Runner]
// emits a `saga_compensation_failed` audit row AND continues with
// the remaining compensations (steps still earlier in the forward
// order). Stopping mid-rollback would leave the operator with a
// half-rolled-back saga where the failed-compensation step's audit
// row is the only signal AND the still-pending compensations are
// silent — strictly worse than the best-effort variant where every
// successful-step compensation has its own `saga_step_compensated`
// or `saga_compensation_failed` audit row. The original step error
// (the one that triggered the rollback) remains the [Runner.Run]
// return value; compensation outcomes are audit-chain-only.
//
// # Event-type vocabulary collision-free with the M7.1.a `saga_*` set
//
// `saga_step_compensated`, `saga_compensation_failed`, and the
// `saga_compensated` summary row share the M7.1.a `saga_` prefix so
// the M6.3.e `llm_turn_cost_*` family stays clear. The four M7.1.a
// event types ([EventTypeSagaStepStarted], [EventTypeSagaStepCompleted],
// [EventTypeSagaFailed], [EventTypeSagaCompleted]) are unchanged.
package saga

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Compensator is the OPTIONAL interface a [Step] implementation may
// satisfy to participate in saga rollback. When a later step's
// [Step.Execute] returns non-nil, the [Runner] iterates every
// previously-successful step in REVERSE forward-order and calls
// [Compensator.Compensate] on each implementer; non-implementers are
// silently skipped (no `saga_step_compensated` row, no `saga_compensation_failed`
// row).
//
// # Per-saga state contract: SpawnContext, NEVER receiver-stash
//
// The same step instance MAY serve multiple sagas concurrently — the
// M7.1.a [Step] doc-block pins this explicitly. A
// [Compensator.Compensate] implementation MUST therefore source every
// per-saga value (manifest_version_id, watchkeeper_id, the
// freshly-minted Slack app_id, OAuth tokens, etc.) from the
// [SpawnContext] / [RetireResult] outboxes carried on `ctx`, NOT from
// fields stamped on the step receiver during a prior `Execute`. A
// receiver-stash would race two distinct sagas the moment they hit
// the SAME step instance concurrently and would mis-target a
// catch-up Run on the M7.3.a `Status == pending` resume path
// (where Run #1 may have written app_id_1 onto the receiver, then
// Run #2's catch-up dispatches Compensate against the wrong value).
//
// Concretely, M7.3.c authors land their per-saga forward-state on
// the relevant outbox: M7.1.c.a CreateApp publishes the freshly-
// minted app_id onto a future [SpawnContext]-equivalent outbox (or
// onto a DAO with its own durability story, mirroring the M2b
// `slack_app_creds` row); CreateApp.Compensate reads it back via
// the outbox to call SlackAppRPC.Delete. The [Runner] dispatches one
// saga's compensation chain on a single goroutine, so the outbox
// pointer's "single-writer-then-readers" discipline (M7.2.b lesson
// on pointer-stored ctx outboxes) holds without a per-saga mutex.
//
// The failed step itself does NOT have its [Compensator.Compensate]
// called: a step that returned non-nil from [Step.Execute] is by
// definition not "successfully executed", and its forward partial-
// success cleanup is the step author's responsibility (the M7.1.c–.e
// step shape's "fail-fast precedes side effect" discipline keeps the
// partial-success surface narrow).
//
// # Compensation runs under [context.WithoutCancel]
//
// The [Runner] derives the compensation ctx via
// [context.WithoutCancel] before dispatching any [Compensator.Compensate]
// call (see [Runner.compensate]). Concretely: a request-bound parent
// ctx that fires Cancel mid-saga (HTTP timeout, operator-initiated
// abort, dispatcher tear-down) MUST NOT poison the rollback chain by
// uniformly returning `context.Canceled` from every Compensate.
// Implementations that perform IO inside Compensate inherit a ctx
// that carries the parent's deadline / values but does NOT propagate
// the parent's cancellation; per-Compensate timeouts belong to the
// step author (mirrors the M7.1.c.* step's "Execute owns its own
// retry / timeout policy" pattern).
//
// # Typed-error contract for `last_error_class`
//
// Errors returned by [Compensator.Compensate] SHOULD implement
// [LastErrorClassed] to override the default
// `step_compensate_error` sentinel emitted on the
// `saga_compensation_failed` audit row. Resolved via [errors.As] so
// a wrapped error in the chain is enough. M7.3.c entry condition:
// every concrete Compensate's typed error chain MUST class its
// failures (e.g. `slack_app_delete_404`, `slack_app_delete_unauthorized`)
// so the operator's "which rollback genuinely failed" filter on the
// audit chain branches on the closed-set sentinel rather than on
// surrounding context. The shared [resolveCompensateLastErrorClass]
// helper additionally folds [context.Canceled] / [context.DeadlineExceeded]
// onto distinct sentinels even when the underlying error chain is
// untyped, so the [context.WithoutCancel] discipline above is
// belt-and-suspenders against a step that re-derives a cancelable
// ctx internally.
type Compensator interface {
	// Compensate undoes the externally-visible side effect previously
	// produced by [Step.Execute]. Return nil on a successful undo;
	// return a typed error on failure. The [Runner] continues with
	// the remaining (earlier-in-forward-order) compensations
	// regardless of this return value — best-effort rollback per the
	// file-level discipline.
	Compensate(ctx context.Context) error
}

// LastErrorClassCompensateDefault is the sentinel emitted in the
// `last_error_class` payload key when a [Compensator.Compensate] call
// returns an error that does not implement [LastErrorClassed] AND is
// not a recognised context-cancellation sentinel. Stable across
// releases so downstream `keepers_log` aggregators can branch on the
// value without parsing the underlying error message. Distinct from
// [LastErrorClassDefault] (which classes a [Step.Execute] failure)
// so the two failure surfaces are disambiguated on the audit chain.
const LastErrorClassCompensateDefault = "step_compensate_error"

// LastErrorClassCompensateContextCanceled is emitted when a
// [Compensator.Compensate] returns an error chain that matches
// [context.Canceled]. Distinct from
// [LastErrorClassCompensateDefault] so an operator filtering the
// audit chain can distinguish "the rollback ran out of budget /
// the request-bound parent was torn down" from "the rollback's
// substrate genuinely failed". M7.3.b's [Runner.compensate] derives
// its own ctx via [context.WithoutCancel] precisely so this branch
// SHOULD only fire when a step author re-introduces cancellation
// internally; a recurrent run of this sentinel on the audit chain
// surfaces a Compensate impl that ignores the rollback-ctx
// discipline.
const LastErrorClassCompensateContextCanceled = "compensate_context_cancelled"

// LastErrorClassCompensateContextDeadline is emitted when a
// [Compensator.Compensate] returns an error chain that matches
// [context.DeadlineExceeded]. Distinct from the cancelled and
// default sentinels so a per-step deadline derived inside the
// Compensate impl (e.g. an HTTP RPC's response timeout) is
// branchable on the audit chain.
const LastErrorClassCompensateContextDeadline = "compensate_context_deadline"

// LastErrorClassCompensatePanic is emitted when a
// [Compensator.Compensate] impl panics; the [Runner]'s
// `defer/recover` harness in `safeCompensate` surfaces it as a
// typed error implementing [LastErrorClassed] so the audit-chain
// sentinel is closed-set rather than the recovered value's string
// form (which could carry PII per M2b.7 — the panic-class is
// emitted; the recovered value is NEVER serialised onto an audit
// payload). Pin the bug-class so M7.3.c authors who land a
// panicking Compensate see a distinct row instead of a torn-down
// saga goroutine + missing summary row.
const LastErrorClassCompensatePanic = "compensate_panic"

// Closed-set audit event types emitted by the [Runner] during the
// M7.3.b reverse-rollback path. Hoisted to constants so a typo in one
// of the emit sites is a compile error and so the prefix stays
// collision-free with the `llm_turn_cost_*` family established in
// M6.3.e.
const (
	// EventTypeSagaStepCompensated is emitted AFTER each
	// [Compensator.Compensate] call returns nil. Carries the
	// compensated step's `step_name` so a downstream consumer can
	// reconstruct the rollback chain by joining on `saga_id`.
	EventTypeSagaStepCompensated = "saga_step_compensated"

	// EventTypeSagaCompensationFailed is emitted when a
	// [Compensator.Compensate] returns non-nil. Carries the
	// compensating step's `step_name` AND a stable
	// `last_error_class` sentinel resolved via [LastErrorClassed]
	// (defaulting to [LastErrorClassCompensateDefault]). The
	// [Runner] continues with the remaining compensations after
	// emitting this row.
	EventTypeSagaCompensationFailed = "saga_compensation_failed"

	// EventTypeSagaCompensated is the saga-level summary row emitted
	// AFTER every previously-successful step has had its compensation
	// attempted (or skipped, for steps that do not implement
	// [Compensator]). Distinct from [EventTypeSagaFailed] so a
	// downstream consumer can pin "rollback chain complete" without
	// counting the per-step rows. Carries only `saga_id`; the
	// per-step detail lives on the [EventTypeSagaStepCompensated] /
	// [EventTypeSagaCompensationFailed] rows. A saga whose step list
	// is empty OR whose first step fails (zero successful steps)
	// emits this event with no preceding per-step compensation rows
	// — operator-visible "we attempted no compensations because
	// nothing had succeeded yet".
	EventTypeSagaCompensated = "saga_compensated"
)

// stepCompensatedPayload composes the closed-set
// `saga_step_compensated` payload. Same shape as
// [stepCompletedPayload] — only the event_type differs, so downstream
// consumers branch on the wire `event_type` column rather than
// payload-shape introspection. PII guard: this function is the SOLE
// composer of the payload; if a future change adds a key, code review
// picks it up here and the wire-shape regression test pins it.
func stepCompensatedPayload(sagaID uuid.UUID, stepName string) map[string]any {
	return map[string]any{
		payloadKeySagaID:   sagaID.String(),
		payloadKeyStepName: stepName,
	}
}

// sagaCompensationFailedPayload composes the closed-set
// `saga_compensation_failed` payload. Carries the compensating
// step's `step_name` AND the resolved `last_error_class` sentinel.
// NEVER carries the underlying error message, stack trace, or
// step-internal params (M2b.7).
func sagaCompensationFailedPayload(sagaID uuid.UUID, stepName, lastErrorClass string) map[string]any {
	return map[string]any{
		payloadKeySagaID:         sagaID.String(),
		payloadKeyStepName:       stepName,
		payloadKeyLastErrorClass: lastErrorClass,
	}
}

// sagaCompensatedPayload composes the closed-set `saga_compensated`
// summary payload. Carries only `saga_id`; per-step detail lives on
// [stepCompensatedPayload] / [sagaCompensationFailedPayload] rows.
func sagaCompensatedPayload(sagaID uuid.UUID) map[string]any {
	return map[string]any{
		payloadKeySagaID: sagaID.String(),
	}
}

// resolveCompensateLastErrorClass walks the supplied error chain and
// returns the [LastErrorClassed.LastErrorClass] value when present,
// the context-cancellation / context-deadline sentinels when the
// chain matches the corresponding `context` package error, or
// [LastErrorClassCompensateDefault] otherwise. Resolution priority
// is intentional: a step's own typed error chain wins over a
// stdlib-context-classification (a step that wraps
// [context.Canceled] under a typed sentinel keeps its
// step-specific class; only un-typed propagation collapses to the
// generic context buckets), so M7.3.c authors who plumb
// [LastErrorClassed] through their wrap chain stay branchable on
// their own closed-set vocabulary.
//
// Distinct sentinels on the un-typed branch buy the operator three
// disambiguated buckets — typed step error / context cancellation
// / context deadline / catch-all default — so a query like
// `WHERE last_error_class LIKE 'compensate_%'` filters to genuine
// rollback failures while context-bucket rows surface the
// budget/teardown surface separately.
func resolveCompensateLastErrorClass(err error) string {
	if err == nil {
		return LastErrorClassCompensateDefault
	}
	var classed LastErrorClassed
	if errors.As(err, &classed) {
		if class := classed.LastErrorClass(); class != "" {
			return class
		}
	}
	if errors.Is(err, context.Canceled) {
		return LastErrorClassCompensateContextCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return LastErrorClassCompensateContextDeadline
	}
	return LastErrorClassCompensateDefault
}
