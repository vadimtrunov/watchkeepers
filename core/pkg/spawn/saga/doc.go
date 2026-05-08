// Package saga is the M7.1.a spawn-saga skeleton: a step-based
// orchestrator + state-persistence DAO that the M7.1.b–.e items will
// plug concrete steps into (Slack interaction handler, Slack App
// provisioning, Notebook provision, runtime launch + intro). The package
// owns the [SagaState] enum, the [Step] interface seam, the [Saga]
// projection, the [SpawnSagaDAO] persistence contract, and the
// [Runner] state-machine that ties them together with the Keeper's Log
// audit chain.
//
// # Architectural seam
//
// M7.1.b–.e plug concrete steps via the [Step] interface; this package
// owns no external calls. No Slack, OAuth, Notebook, or runtime traffic
// originates here — every external side-effect is expected to live
// inside a future [Step.Execute] implementation. The saga core owns:
//
//  1. State transitions (`pending` → `in_flight` → `completed` |
//     `failed`) persisted via [SpawnSagaDAO].
//  2. Audit emission (`saga_step_started`, `saga_step_completed`,
//     `saga_failed`, `saga_completed`) through a narrow [Appender] seam
//     satisfied by [keeperslog.Writer].
//  3. Run-loop ordering: state-update precedes step execute; audit emit
//     precedes the call return path.
//
// # PII discipline (M2b.7 / M6 lessons)
//
// Audit payloads emitted by the saga carry only `saga_id`, `step_name`,
// and (for failure) `last_error_class` (a sentinel string supplied by
// the failing step's typed error chain). They NEVER carry raw step
// parameters, external response bodies, stack traces, or any
// step-internal state. Step authors do NOT touch the DAO or the audit
// sink directly — the [Runner] is the sole emitter, and a
// payload-shape test pins the JSON keys against drift.
//
// # Closed-set event-type vocabulary
//
// The saga emits exactly four event types, hoisted to constants in
// `saga.go` so a typo in one of the emit sites is a compile error
// (and to keep the prefix collision-free with the `llm_turn_cost_*`
// family established in M6.3.e):
//
//   - [EventTypeSagaStepStarted]   = "saga_step_started"
//   - [EventTypeSagaStepCompleted] = "saga_step_completed"
//   - [EventTypeSagaFailed]        = "saga_failed"
//   - [EventTypeSagaCompleted]     = "saga_completed"
//
// # In-memory DAO ships alongside; Postgres adapter deferred
//
// Per the M6.3.b lesson ("ship in-memory DAO + tests with consumer,
// defer Postgres adapter"), this package ships only the [MemorySpawnSagaDAO]
// implementation. The matching `019_spawn_sagas.sql` goose migration
// lands in the same PR so M7.1.b can wire a Postgres-backed adapter
// without a migration churn.
package saga
