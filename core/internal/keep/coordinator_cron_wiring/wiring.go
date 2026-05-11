// Package coordinatorcronwiring is the M8.3 production composition
// entrypoint for the Coordinator's two cron-driven event ticks: the
// daily briefing tick and the morning overdue sweep tick. Each tick
// publishes a topic event onto the shared eventbus; downstream
// subscribers (an LLM-driven Coordinator runtime call composed by the
// future Coordinator binary) consume the event and dispatch the
// corresponding tool chain — `post_daily_briefing` for the briefing
// tick, `find_overdue_tickets` + `nudge_reviewer` for the sweep tick.
//
// DEFERRED WIRING (intentional): the subscriber side that drives the
// LLM runtime turn is NOT yet wired from any running binary. This
// package ships ahead so the cron registration shape + event topics
// are pinned + smoke-tested before the Coordinator binary lands.
// Mirrors the M7.1.b `approvalwiring.ComposeApprovalDispatcher`
// deferred-wiring precedent.
//
// Package location note: this helper lives under `core/internal/keep/`
// so it stays usable by both the keep binary (smoke-test fixture) AND
// the future Coordinator binary without a circular-import detour
// through `core/cmd/`. The package is internal to keep + spawn-side
// binaries; M9+ tool-registry surfaces consume the topic constants
// directly off the eventbus without importing this package.
package coordinatorcronwiring

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/cron"
)

// TopicDailyBriefingTick is the eventbus topic the daily briefing
// cron tick publishes onto. Subscribers consume the event payload to
// drive an LLM-runtime turn that composes `post_daily_briefing` tool
// args (with the briefing's "Pending lessons (24h cooling-off)"
// section pulled via `list_pending_lessons`) and dispatches. Pinned
// here as a const so a future subscriber package can `import _`
// reference the same string the publisher emits — drift fails build,
// not production.
const TopicDailyBriefingTick = "coordinator.daily_briefing.tick"

// TopicOverdueSweepTick is the eventbus topic the morning overdue
// sweep cron tick publishes onto. Subscribers drive an LLM-runtime
// turn that calls `find_overdue_tickets` and, per ticket,
// `nudge_reviewer`. Pinned as a const for the same drift-rejection
// reason as [TopicDailyBriefingTick].
const TopicOverdueSweepTick = "coordinator.overdue_sweep.tick"

// DefaultDailyBriefingSpec is the cron spec used when [Config.DailyBriefingSpec]
// is empty. 6-field robfig form (`sec min hour dom mon dow` — the
// shared [cron.Scheduler] is built with `WithSeconds()`); fires at
// 09:00 UTC every weekday. Production overrides this via [Config]
// per the deployment's working-hours convention; the default is a
// reasonable engineering-time-zone placeholder.
const DefaultDailyBriefingSpec = "0 0 9 * * 1-5"

// DefaultOverdueSweepSpec is the cron spec used when
// [Config.OverdueSweepSpec] is empty. Fires at 08:30 UTC every
// weekday — 30 minutes before the briefing so the LLM run that
// composes the briefing can see the sweep's outcomes if both ticks
// are wired to a shared notebook + audit trail.
const DefaultOverdueSweepSpec = "0 30 8 * * 1-5"

// TickEvent is the payload published onto both topics. Each tick
// mints a fresh [TickEvent] with a correlation id + UTC clock-stamp
// so downstream subscribers can correlate the event with the LLM
// runtime turn they dispatch (the M3-wide "correlation id matches
// across publish + handler" discipline).
type TickEvent struct {
	CorrelationID string
	FiredAt       time.Time

	// Topic — Future-use (iter-1 critic Minor #2): currently
	// redundant with the topic argument the publisher receives on
	// Publish(ctx, topic, event). Pinned on the payload so a future
	// fan-in subscriber (e.g. an audit-pipe consuming BOTH ticks
	// through a single channel) can disambiguate without re-parsing
	// the topic name. The cost is one string field per fire; the
	// alternative is a fan-out subscriber per topic. Keep this field
	// across future PRs that might prune "unused" struct fields.
	Topic string
}

// Config is the construction-time bag [RegisterCoordinatorCronTicks]
// consumes. Held as a struct so a future addition (e.g. per-tenant
// briefing spec override) replaces a single field rather than
// churning the function signature.
type Config struct {
	// DailyBriefingSpec is the 6-field cron spec for the daily
	// briefing tick. Empty falls back to [DefaultDailyBriefingSpec].
	DailyBriefingSpec string

	// OverdueSweepSpec is the 6-field cron spec for the morning
	// overdue sweep tick. Empty falls back to
	// [DefaultOverdueSweepSpec].
	OverdueSweepSpec string

	// Clock overrides the wall-clock used to stamp [TickEvent.FiredAt].
	// Defaults to [time.Now]. Tests pin a deterministic clock; nil
	// is a no-op so callers can always pass through whatever they
	// have.
	Clock func() time.Time

	// NewCorrelationID overrides the function used to mint
	// [TickEvent.CorrelationID]. Defaults to a UUID v7 generator;
	// tests pin a deterministic id source. Nil is a no-op.
	NewCorrelationID func() string
}

// RegisteredEntries is the [cron.EntryID] tuple returned by
// [RegisterCoordinatorCronTicks] so callers can [cron.Scheduler.Unschedule]
// individually if a per-Coordinator restart path needs it. Held as a
// named struct (rather than a `[]cron.EntryID`) so adding a third
// tick later is a field add, not a positional-index churn.
type RegisteredEntries struct {
	// DailyBriefing is the entry id for the daily briefing tick.
	DailyBriefing cron.EntryID
	// OverdueSweep is the entry id for the morning overdue sweep tick.
	OverdueSweep cron.EntryID
}

// RegisterCoordinatorCronTicks registers the two Coordinator cron
// entries onto `sched` and returns their entry ids. The scheduler
// MUST be live (constructed via [cron.New], not yet [cron.Scheduler.Stop]);
// a nil scheduler is a programmer error and panics with a clear
// message per the M*.c.* nil-dep discipline.
//
// On any per-entry [cron.Scheduler.Schedule] error, the helper
// rolls back already-registered entries via [cron.Scheduler.Unschedule]
// and returns the wrapped error. This keeps the eventbus from
// silently carrying half the ticks when the second [cron.Scheduler.Schedule]
// rejects a malformed spec — mirrors the fail-fast-and-rollback
// discipline of the M7.1.b approval-wiring helper.
//
// The closures the helper hands to [cron.Scheduler.Schedule] capture
// the configured clock + correlation-id source. Per-fire factory
// invocations mint a fresh [TickEvent] so subsequent publish hits
// land on the M3 correlation-id contract.
func RegisterCoordinatorCronTicks(sched *cron.Scheduler, cfg Config) (RegisteredEntries, error) {
	if sched == nil {
		panic("coordinatorcronwiring: RegisterCoordinatorCronTicks: scheduler must not be nil")
	}

	briefingSpec := cfg.DailyBriefingSpec
	if briefingSpec == "" {
		briefingSpec = DefaultDailyBriefingSpec
	}
	sweepSpec := cfg.OverdueSweepSpec
	if sweepSpec == "" {
		sweepSpec = DefaultOverdueSweepSpec
	}

	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	newCorrelationID := cfg.NewCorrelationID
	if newCorrelationID == nil {
		newCorrelationID = defaultCorrelationID
	}

	briefingID, err := sched.Schedule(
		briefingSpec,
		TopicDailyBriefingTick,
		makeTickFactory(TopicDailyBriefingTick, clock, newCorrelationID),
	)
	if err != nil {
		return RegisteredEntries{}, fmt.Errorf("coordinatorcronwiring: register daily briefing tick: %w", err)
	}

	sweepID, err := sched.Schedule(
		sweepSpec,
		TopicOverdueSweepTick,
		makeTickFactory(TopicOverdueSweepTick, clock, newCorrelationID),
	)
	if err != nil {
		// Rollback the briefing registration — partial wiring is
		// worse than no wiring; the operator gets a clean failure
		// they can correlate against the spec they passed.
		//
		// [cron.Scheduler.Unschedule] returns nil except when the
		// scheduler is already Stopped ([cron.ErrAlreadyStopped]).
		// A concurrent caller Stopping the scheduler between our
		// successful Schedule and this rollback is the only path
		// to a non-nil error here — and in that case the rollback
		// is moot (Stop drains every entry). Swallowing is
		// correct; the second-Schedule error is the load-bearing
		// signal for the operator.
		_ = sched.Unschedule(briefingID)
		return RegisteredEntries{}, fmt.Errorf("coordinatorcronwiring: register overdue sweep tick: %w", err)
	}

	return RegisteredEntries{
		DailyBriefing: briefingID,
		OverdueSweep:  sweepID,
	}, nil
}

// makeTickFactory returns the [cron.EventFactory] each registered
// entry invokes per fire. Held as a helper (rather than inlined twice
// in [RegisterCoordinatorCronTicks]) so the per-fire shape is asserted
// once in unit tests and reused verbatim by both registrations.
func makeTickFactory(
	topic string,
	clock func() time.Time,
	newCorrelationID func() string,
) cron.EventFactory {
	return func(_ context.Context) any {
		return TickEvent{
			CorrelationID: newCorrelationID(),
			FiredAt:       clock().UTC(),
			Topic:         topic,
		}
	}
}

// defaultCorrelationID returns a fresh UUID v7 string. v7 carries an
// embedded timestamp so a sorted list of correlation ids is a sorted
// list of fire times — useful for the audit-pipe correlator. Falls
// back to v4 on the (vanishingly rare) v7 mint failure so the cron
// fire never panics inside the factory closure; the v4 fallback
// keeps the id unique even if it loses the timestamp-sort property.
func defaultCorrelationID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}
