// saga.go defines the [Step] interface seam, the [Runner]
// state-machine, and the four closed-set audit event types the saga
// emits via the Keeper's Log. The [Runner] is the sole composer of
// audit payloads; step authors do NOT touch the DAO or the audit
// sink directly (AC4).
package saga

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
)

// Closed-set Keeper's Log event types emitted by the [Runner]. Hoisted
// to constants so a typo in one of the emit sites is a compile error
// and so the prefix stays collision-free with the `llm_turn_cost_*`
// family established in M6.3.e.
const (
	// EventTypeSagaStepStarted is emitted BEFORE each [Step.Execute]
	// call, AFTER the DAO has transitioned the row to `in_flight`
	// with the new `current_step`. The state-write-precedes-execute
	// ordering preserves the M6.3.c "render-before-persist" analogue:
	// a crash mid-execute leaves the row pointing at the step that
	// was running, which the audit row matches.
	EventTypeSagaStepStarted = "saga_step_started"

	// EventTypeSagaStepCompleted is emitted AFTER each [Step.Execute]
	// call returns nil, BEFORE the [Runner] proceeds to the next
	// step (or to [SpawnSagaDAO.MarkCompleted]). Pinned to fire on
	// the success path only — the failure path emits
	// [EventTypeSagaFailed] instead.
	EventTypeSagaStepCompleted = "saga_step_completed"

	// EventTypeSagaFailed is emitted when a [Step.Execute] returns
	// non-nil, AFTER [SpawnSagaDAO.MarkFailed] has persisted the
	// terminal state. Carries the `last_error_class` sentinel
	// derived from the failing step's typed error chain.
	EventTypeSagaFailed = "saga_failed"

	// EventTypeSagaCompleted is emitted exactly once per successful
	// saga, AFTER [SpawnSagaDAO.MarkCompleted] has persisted the
	// terminal state. Zero-step sagas emit only this event.
	EventTypeSagaCompleted = "saga_completed"
)

// Closed-set audit payload keys. Hoisted to constants so the
// payload-shape regression test pins the wire vocabulary (AC5 / M2b.7
// PII discipline). The [Runner] is the SOLE composer of saga audit
// payloads; if a future change adds a key, code review picks it up
// here and the wire-shape test pins it.
const (
	payloadKeySagaID         = "saga_id"
	payloadKeyStepName       = "step_name"
	payloadKeyLastErrorClass = "last_error_class"
)

// LastErrorClassDefault is the sentinel emitted in the
// `last_error_class` payload key when a step's [Step.Execute] returns
// an error that does not implement [LastErrorClassed]. Stable across
// releases so downstream `keepers_log` aggregators can branch on the
// value without parsing the underlying error message.
const LastErrorClassDefault = "step_execute_error"

// LastErrorClassed is the optional interface a step's typed error may
// implement to override the default `last_error_class` sentinel
// emitted in the failure audit payload. The returned string MUST be a
// stable closed-set value (snake_case, no PII, no params, no stack);
// the [Runner] forwards it verbatim. When a step's error chain does
// not implement this interface, the [Runner] falls back to
// [LastErrorClassDefault].
//
// Resolved via [errors.As] so a wrapped error in the chain is enough.
type LastErrorClassed interface {
	LastErrorClass() string
}

// Step is the interface seam M7.1.b–.e plug concrete steps into. A
// [Step] is a single externally-side-effecting unit of work the saga
// invokes between state transitions. The [Runner] calls [Step.Execute]
// at most once per saga run; idempotency / compensation belong to
// future M7.3 work.
//
// Naming: [Step.Name] returns a stable closed-set identifier (e.g.
// "slack_app_create", "notebook_provision", "runtime_launch") used
// as the `current_step` DAO field AND as the `step_name` audit
// payload key. MUST NOT carry PII or per-saga state — the same step
// instance may serve multiple sagas concurrently.
//
// Concurrency: implementations MUST be safe for concurrent calls
// across distinct sagas (the [Runner] holds no per-saga step-level
// lock).
type Step interface {
	// Name returns the stable closed-set identifier for this step.
	// Used by the [Runner] as the `current_step` DAO column and as
	// the `step_name` audit payload key.
	Name() string

	// Execute performs the step's external side effect. Return nil
	// on success; return a typed error on failure. Errors that
	// implement [LastErrorClassed] override the default
	// `last_error_class` audit-payload sentinel.
	Execute(ctx context.Context) error
}

// Appender is the minimal subset of [keeperslog.Writer] the [Runner]
// consumes — only the [keeperslog.Writer.Append] method is touched.
// Defined as an interface in this package so unit tests can substitute
// a hand-rolled fake that asserts the audit-row contract directly,
// and so production code never depends on the concrete *keeperslog.Writer
// type at all (mirrors the messenger.AuditAppender + cost.Appender +
// keeperslog.LocalKeepClient import-cycle-break pattern documented in
// `docs/LESSONS.md`).
//
// `*keeperslog.Writer` satisfies this interface as-is; the compile-time
// assertion lives in `saga_test.go`.
type Appender interface {
	Append(ctx context.Context, evt keeperslog.Event) (string, error)
}

// Dependencies is the construction-time bag wired into [NewRunner].
// Held in a struct so a future addition (e.g. a clock, a tracer)
// lands as a new field without breaking the constructor signature.
type Dependencies struct {
	// DAO is the persistence seam. Required; a nil DAO is rejected
	// at construction with a clear panic message — a runner with no
	// DAO cannot do anything useful and silently no-oping every call
	// would mask the bug.
	DAO SpawnSagaDAO

	// Logger is the audit-emit seam. Required; a nil Logger is
	// rejected at construction. Mirrors the cost.Dependencies.Logger
	// discipline (M6.3.e): audit recordings are load-bearing for the
	// saga's behavioural contract — a missing sink is a programmer
	// error, not a silent degradation.
	Logger Appender
}

// Runner is the state-machine that drives a [Step] slice through the
// `pending → in_flight → completed | failed` transition graph,
// persisting each transition via [SpawnSagaDAO] and emitting the
// matching Keeper's Log audit row via [Appender].
//
// Construct via [NewRunner]; the zero value is not usable.
//
// Concurrency: safe for concurrent use after construction. Holds only
// immutable configuration; per-call state lives on the goroutine
// stack. Concurrent [Runner.Run] calls on distinct saga ids never
// block each other beyond a normal map read/write inside the DAO.
type Runner struct {
	dao    SpawnSagaDAO
	logger Appender
}

// NewRunner constructs a [Runner] with the supplied [Dependencies].
// Both `deps.DAO` and `deps.Logger` are required; a nil value for
// either panics with a clear message — matches the panic discipline
// of [keeperslog.New] and [cost.NewLoggingProvider].
func NewRunner(deps Dependencies) *Runner {
	if deps.DAO == nil {
		panic("saga: NewRunner: deps.DAO must not be nil")
	}
	if deps.Logger == nil {
		panic("saga: NewRunner: deps.Logger must not be nil")
	}
	return &Runner{
		dao:    deps.DAO,
		logger: deps.Logger,
	}
}

// Run drives the saga identified by `sagaID` through the supplied
// `steps` slice. The saga row MUST be pre-`Insert`ed by the caller
// (M7.1.a does not own the spawn entrypoint; that wiring lands in
// M7.1.b).
//
// Sequence per AC4:
//
//  1. Resolve the saga via [SpawnSagaDAO.Get]. On miss, return a
//     wrapped [ErrSagaNotFound]; emit no audit events (fail-fast
//     before the first step).
//  2. For each step in order:
//     a. Call [SpawnSagaDAO.UpdateStep] with the step's name
//     (state-write precedes execute).
//     b. Emit `saga_step_started` (audit-emit precedes execute).
//     c. Call [Step.Execute].
//     d. On error: call [SpawnSagaDAO.MarkFailed] with the resolved
//     `last_error_class` sentinel, emit `saga_failed`, then run
//     the M7.3.b reverse-rollback chain over every previously-
//     successful step that implements [Compensator] (per-step
//     `saga_step_compensated` / `saga_compensation_failed` rows
//     followed by a saga-level `saga_compensated` summary row),
//     and return the wrapped step error. Compensations run AFTER
//     `saga_failed` so the audit chain stays causally readable.
//     e. On success: emit `saga_step_completed`.
//  3. After the last step succeeds (or immediately for an empty
//     `steps` slice): call [SpawnSagaDAO.MarkCompleted] and emit
//     `saga_completed`.
//
// All audit payloads carry only `saga_id`, `step_name`, and (failure
// only) `last_error_class` per AC5. The returned error wraps the
// original [Step.Execute] error chain on the failure path so callers
// can `errors.Is` / `errors.As` on the underlying type. Compensation
// outcomes are audit-chain-only — they never alter the [Run] return
// value, so the operator's "what made the saga fail?" question is
// answered by the original step error regardless of how many
// compensations succeeded or failed.
func (r *Runner) Run(ctx context.Context, sagaID uuid.UUID, steps []Step) error {
	if _, err := r.dao.Get(ctx, sagaID); err != nil {
		if errors.Is(err, ErrSagaNotFound) {
			return fmt.Errorf("saga: run: %w", err)
		}
		return fmt.Errorf("saga: run: get: %w", err)
	}

	// executed tracks the steps whose [Step.Execute] returned nil,
	// in forward order. When a later step fails, the M7.3.b
	// reverse-rollback chain iterates this slice in reverse. Steps
	// that do NOT implement [Compensator] are still added (so the
	// reverse-walk's positional logic stays correct); the
	// per-step Compensate dispatch in [Runner.compensate] simply
	// skips non-implementers without emitting any audit row.
	executed := make([]Step, 0, len(steps))

	for _, step := range steps {
		stepName := step.Name()

		if err := r.dao.UpdateStep(ctx, sagaID, stepName); err != nil {
			return fmt.Errorf("saga: run: update step %q: %w", stepName, err)
		}

		r.emit(ctx, keeperslog.Event{
			EventType: EventTypeSagaStepStarted,
			Payload:   stepStartedPayload(sagaID, stepName),
		})

		if execErr := step.Execute(ctx); execErr != nil {
			lastErrorClass := resolveLastErrorClass(execErr)
			if markErr := r.dao.MarkFailed(ctx, sagaID, lastErrorClass); markErr != nil {
				return fmt.Errorf("saga: run: mark failed after step %q: %w", stepName, markErr)
			}
			r.emit(ctx, keeperslog.Event{
				EventType: EventTypeSagaFailed,
				Payload:   sagaFailedPayload(sagaID, stepName, lastErrorClass),
			})
			r.compensate(ctx, sagaID, executed)
			return fmt.Errorf("saga: run: step %q: %w", stepName, execErr)
		}

		r.emit(ctx, keeperslog.Event{
			EventType: EventTypeSagaStepCompleted,
			Payload:   stepCompletedPayload(sagaID, stepName),
		})
		executed = append(executed, step)
	}

	if err := r.dao.MarkCompleted(ctx, sagaID); err != nil {
		return fmt.Errorf("saga: run: mark completed: %w", err)
	}
	r.emit(ctx, keeperslog.Event{
		EventType: EventTypeSagaCompleted,
		Payload:   sagaCompletedPayload(sagaID),
	})
	return nil
}

// emit forwards an audit event to the configured [Appender]. The
// Appender failure is intentionally swallowed — the saga's behavioural
// contract is "state-machine progressed; audit best-effort" mirroring
// the M6.3.e logger-error policy. The Appender's own diagnostic sink
// (if wired via [keeperslog.WithLogger]) records the failure on a
// separate channel.
func (r *Runner) emit(ctx context.Context, evt keeperslog.Event) {
	_, _ = r.logger.Append(ctx, evt)
}

// compensate is the M7.3.b reverse-rollback chain. The [Runner] calls
// it AFTER `saga_failed` has been emitted on the [Step.Execute]
// failure path, with `executed` populated with every previously-
// successful step in forward order. The chain walks `executed` in
// REVERSE; for each step that implements [Compensator] it calls
// [Compensator.Compensate] and emits a `saga_step_compensated` row on
// nil OR a `saga_compensation_failed` row on non-nil. Steps that do
// not implement [Compensator] are silently skipped — their forward
// [Step.Execute] had no compensable side effect (e.g. M7.1.c.c
// BotProfile, whose only side effect is a watchkeeper-row write that
// the M7.1.c.a CreateApp.Compensate teardown of the Slack App makes
// moot anyway).
//
// Best-effort: a non-nil [Compensator.Compensate] does NOT abort the
// chain. The [Runner] continues with the remaining (earlier-in-
// forward-order) compensations. The original [Step.Execute] error
// stays the [Run] return value; compensation outcomes are
// audit-chain-only.
//
// A saga-level [EventTypeSagaCompensated] summary row is emitted at
// the end of the chain regardless of how many per-step compensations
// fired (zero on a first-step failure; len(executed) on a later
// failure). Operators who watch the audit chain see the boundary
// without counting per-step rows.
func (r *Runner) compensate(ctx context.Context, sagaID uuid.UUID, executed []Step) {
	// Derive a rollback ctx that carries the parent's deadline +
	// values but does NOT propagate the parent's cancellation. A
	// request-bound parent ctx that fires Cancel mid-saga (HTTP
	// timeout, operator-initiated abort, dispatcher tear-down) MUST
	// NOT poison the rollback chain by uniformly returning
	// `context.Canceled` from every Compensate. Per-Compensate
	// timeouts belong to the step author (mirrors the M7.1.c.*
	// step's "Execute owns its own retry / timeout policy"
	// pattern). The audit chain still uses the ORIGINAL `ctx` for
	// the saga-level summary row so the row inherits the parent's
	// trace identifiers — only the per-step Compensate dispatch
	// uses `compensateCtx`.
	compensateCtx := context.WithoutCancel(ctx)

	for i := len(executed) - 1; i >= 0; i-- {
		step := executed[i]
		compensator, ok := step.(Compensator)
		if !ok {
			continue
		}
		stepName := step.Name()
		err := safeCompensate(compensateCtx, compensator)
		if err != nil {
			r.emit(ctx, keeperslog.Event{
				EventType: EventTypeSagaCompensationFailed,
				Payload: sagaCompensationFailedPayload(
					sagaID,
					stepName,
					resolveCompensateLastErrorClass(err),
				),
			})
			continue
		}
		r.emit(ctx, keeperslog.Event{
			EventType: EventTypeSagaStepCompensated,
			Payload:   stepCompensatedPayload(sagaID, stepName),
		})
	}
	r.emit(ctx, keeperslog.Event{
		EventType: EventTypeSagaCompensated,
		Payload:   sagaCompensatedPayload(sagaID),
	})
}

// safeCompensate dispatches [Compensator.Compensate] under a
// `defer/recover` so a panicking step impl converts to a typed
// error rather than tearing down the saga goroutine and skipping
// the [EventTypeSagaCompensated] summary row. The recovered panic
// surfaces as [errPanicDuringCompensate], which
// [resolveCompensateLastErrorClass] resolves via
// [LastErrorClassed] to the stable
// [LastErrorClassCompensatePanic] sentinel — distinct from the
// `step_compensate_error` default so an operator filtering the
// audit chain can pin "rollback impl panicked" against the
// closed-set vocabulary.
func safeCompensate(ctx context.Context, c Compensator) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = errPanicDuringCompensate{recovered: rec}
		}
	}()
	return c.Compensate(ctx)
}

// errPanicDuringCompensate is the typed error
// [safeCompensate] returns when a [Compensator.Compensate] impl
// panics. Implements [LastErrorClassed] so the
// `saga_compensation_failed` audit row classes it as
// [LastErrorClassCompensatePanic] without parsing the recovered
// value's string form. The recovered value itself is intentionally
// NOT included in the audit payload (M2b.7 PII discipline — a
// panicking impl could carry secrets in its recovered value).
type errPanicDuringCompensate struct{ recovered any }

func (e errPanicDuringCompensate) Error() string {
	return fmt.Sprintf("saga: compensate panicked: %v", e.recovered)
}

func (e errPanicDuringCompensate) LastErrorClass() string {
	return LastErrorClassCompensatePanic
}

// resolveLastErrorClass walks the supplied error chain and returns
// the first [LastErrorClassed.LastErrorClass] value it finds, or
// [LastErrorClassDefault] when no link in the chain implements the
// interface. Resolved via [errors.As] so wrapped errors are enough.
func resolveLastErrorClass(err error) string {
	return resolveLastErrorClassWithDefault(err, LastErrorClassDefault)
}

// resolveLastErrorClassWithDefault is the shared implementation behind
// [resolveLastErrorClass] and the M7.3.b
// [resolveCompensateLastErrorClass]. The two surfaces differ only in
// the fallback sentinel emitted when the supplied error chain does
// not implement [LastErrorClassed]: the [Step.Execute] failure path
// defaults to [LastErrorClassDefault] (`step_execute_error`); the
// [Compensator.Compensate] failure path defaults to
// [LastErrorClassCompensateDefault] (`step_compensate_error`). The
// two are deliberately distinct so a downstream consumer can branch
// on the audit-chain sentinel without parsing the surrounding
// `event_type`.
func resolveLastErrorClassWithDefault(err error, fallback string) string {
	var classed LastErrorClassed
	if errors.As(err, &classed) {
		if class := classed.LastErrorClass(); class != "" {
			return class
		}
	}
	return fallback
}

// stepStartedPayload composes the closed-set `saga_step_started`
// payload. PII guard: this function is the SOLE composer of the
// payload; if a future change adds a key, code review picks it up
// here and the wire-shape regression test pins it. Returns a
// `map[string]any` because that is the shape [keeperslog.Event.Payload]
// consumes; the keepers_log writer JSON-marshals it under the
// envelope `data` key.
func stepStartedPayload(sagaID uuid.UUID, stepName string) map[string]any {
	return map[string]any{
		payloadKeySagaID:   sagaID.String(),
		payloadKeyStepName: stepName,
	}
}

// stepCompletedPayload composes the closed-set `saga_step_completed`
// payload. Same shape as `stepStartedPayload` — only the event_type
// differs, so downstream consumers branch on the wire `event_type`
// column rather than payload-shape introspection.
func stepCompletedPayload(sagaID uuid.UUID, stepName string) map[string]any {
	return map[string]any{
		payloadKeySagaID:   sagaID.String(),
		payloadKeyStepName: stepName,
	}
}

// sagaFailedPayload composes the closed-set `saga_failed` payload.
// Carries the failing step's `step_name` AND the resolved
// `last_error_class` sentinel. NEVER carries the underlying error
// message, stack trace, or step-internal params (M2b.7).
func sagaFailedPayload(sagaID uuid.UUID, stepName, lastErrorClass string) map[string]any {
	return map[string]any{
		payloadKeySagaID:         sagaID.String(),
		payloadKeyStepName:       stepName,
		payloadKeyLastErrorClass: lastErrorClass,
	}
}

// sagaCompletedPayload composes the closed-set `saga_completed`
// payload. Carries only `saga_id`; no `step_name` because the event
// is fired at the saga-level, not the step-level.
func sagaCompletedPayload(sagaID uuid.UUID) map[string]any {
	return map[string]any{
		payloadKeySagaID: sagaID.String(),
	}
}
