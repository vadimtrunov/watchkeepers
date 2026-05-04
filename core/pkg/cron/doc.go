// Package cron is a thin facade over [github.com/robfig/cron/v3] that
// publishes a fresh event onto a [LocalPublisher] (structurally satisfied
// by [*github.com/vadimtrunov/watchkeepers/core/pkg/eventbus.Bus]) on
// every fire. ROADMAP §M3 → M3.3.
//
// The package exposes a [Scheduler] type constructed via functional
// options ([Option]), five sentinel errors, and an [EventFactory] alias.
// Callers register `(spec, topic, factory)` triples via
// [Scheduler.Schedule]; on every fire the registered closure invokes the
// factory to mint a fresh event (typically carrying a fresh
// `correlation_id` + timestamp at fire time) and forwards
// `(ctx, topic, event)` to the publisher.
//
// # Why a factory closure
//
// M3-wide verification requires that cron-fired and handler-ran events
// carry matching correlation ids. A static event captured at
// [Scheduler.Schedule] time would reuse the same id on every fire — wrong.
// The factory pattern lets the caller mint a fresh id (and timestamp)
// per fire while the scheduler stays untyped (`any`).
//
// # Best-effort firing
//
// Per-fire failures (factory panic, publisher error) are recovered and
// logged via the optional [Logger]. They do NOT stop the scheduler — the
// next tick will retry. This mirrors [notebook.PeriodicBackup]'s
// best-effort tick semantics: one bad tick is not a fatal scheduling
// event.
//
// # Lifecycle
//
// A [Scheduler] is single-use:
//
//   - Construct via [New] (panics on nil publisher).
//   - Optionally [Scheduler.Schedule] entries before [Scheduler.Start].
//   - [Scheduler.Start] takes a context whose lifetime parents every
//     per-fire ctx. Subsequent Start calls return [ErrAlreadyStarted].
//   - [Scheduler.Schedule] / [Scheduler.Unschedule] still work after
//     Start; they update the live entry set.
//   - [Scheduler.Stop] cancels the cron and returns the underlying
//     stop-context. Caller can `<-ctx.Done()` to wait for in-flight Job
//     runs to drain. Subsequent calls return the same already-done ctx.
//   - After Stop every Schedule / Unschedule returns [ErrAlreadyStopped].
//
// # Out of scope (deferred)
//
//   - Durable schedule persistence — entries are in-memory only. A
//     restart re-runs the wiring code that registered them.
//   - Distributed-lock / leader election — single-host only in Phase 1.
//   - Clock injection — robfig/cron v3 has no clock seam. Tests use
//     sub-second specs (`* * * * * *` via `cron.WithSeconds()`) plus
//     the polling-deadline assertion pattern documented in
//     `docs/LESSONS.md` (M2b.5).
//   - Event-bus topic management — the caller picks the topic string and
//     is responsible for it.
package cron
