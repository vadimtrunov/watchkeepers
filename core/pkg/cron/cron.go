package cron

import (
	"context"
	"fmt"
	"sync"
	"time"

	robfigcron "github.com/robfig/cron/v3"
)

// EntryID re-exports robfig/cron v3's identifier type so callers do not
// have to import the underlying package directly. It is the value
// returned by [Scheduler.Schedule] and consumed by [Scheduler.Unschedule].
type EntryID = robfigcron.EntryID

// EventFactory mints a fresh event for each fire. The scheduler invokes
// the factory under a per-fire ctx (parented to the ctx passed to
// [Scheduler.Start]) so factories can honour cancellation if they do
// heavyweight work — though most factories should stay cheap and just
// build the event payload with a fresh correlation id + timestamp.
//
// Returning a nil event is allowed; the publisher is called with the nil
// any value. Returning a value that the publisher cannot serialise is
// the caller's problem and surfaces as a logged publish error per fire,
// not a scheduler-halting condition.
type EventFactory = func(ctx context.Context) any

// LocalPublisher is the minimal subset of the eventbus surface that
// [Scheduler] consumes. Defined as an interface in this package so tests
// can substitute a hand-rolled fake without standing up a Bus, and so
// production code never has to import the concrete `*eventbus.Bus` type
// at all — only the one method this package actually calls.
// `*eventbus.Bus` satisfies the interface as-is; the compile-time
// assertion lives in `cron_test.go`, mirroring the
// lifecycle.LocalKeepClient + notebook.archivestore one-way
// import-cycle-break pattern documented in `docs/LESSONS.md` (M2b.6 /
// M3.2.b).
type LocalPublisher interface {
	Publish(ctx context.Context, topic string, event any) error
}

// Logger is the audit-emit / diagnostic sink wired in via [WithLogger].
// The shape mirrors notebook's [Logger] subset: a single
// `Log(ctx, msg, kv...)` call so callers can substitute structured
// loggers (e.g. slog.Logger.LogAttrs wrapper) without losing type
// compatibility. Reserved for per-fire publish-error and panic-recovery
// diagnostics — successful fires are intentionally not logged to avoid
// chatty output on tight cron schedules.
//
// The variadic `kv` slice carries flat key,value pairs (`"err", err,
// "topic", "watchkeeper.cron"`). A nil logger silently drops the
// message — the package never panics on a nil logger.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// Option configures a [Scheduler] at construction time. Pass options to
// [New]; later options override earlier ones for the same field.
type Option func(*config)

// config is the internal mutable bag the [Option] callbacks populate.
// Held in a separate type so [Scheduler] itself stays immutable after
// [New] returns.
type config struct {
	logger   Logger
	location *time.Location
}

// WithLogger wires a diagnostic sink onto the returned [*Scheduler].
// When set, the scheduler calls `Log(ctx, msg, kv...)` on per-fire
// publish errors and recovered factory panics. A nil logger is a no-op
// so callers can always pass through whatever they have.
func WithLogger(l Logger) Option {
	return func(c *config) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithLocation overrides the time-zone used by the underlying
// robfig/cron parser when interpreting cron specs. Defaults to
// [time.UTC]. A nil location is a no-op so callers can always pass
// through whatever they have.
//
// The location applies to ALL entries scheduled on the resulting
// Scheduler; per-entry locations are not supported (mirrors robfig's
// constructor signature). Callers needing mixed time zones build
// separate Schedulers.
func WithLocation(loc *time.Location) Option {
	return func(c *config) {
		if loc != nil {
			c.location = loc
		}
	}
}

// schedulerState is a 3-state lifecycle (not-started / started /
// stopped). The states are linear (no resurrection), so a single int
// guarded by [Scheduler.mu] is sufficient.
type schedulerState int

const (
	stateNotStarted schedulerState = iota
	stateStarted
	stateStopped
)

// Scheduler is the cron-spec scheduler facade. Construct via [New]; the
// zero value is not usable. Methods are safe for concurrent use; state
// transitions and entry registration are guarded by an internal mutex.
//
// The receiver wraps a single robfig/cron *Cron instance. Production
// code in this package depends only on the [LocalPublisher] interface;
// the concrete `*eventbus.Bus` is referenced only from `cron_test.go`
// for the compile-time assertion.
type Scheduler struct {
	pub    LocalPublisher
	logger Logger
	cron   *robfigcron.Cron

	mu    sync.Mutex
	state schedulerState
	// runCtx is the ctx supplied to [Scheduler.Start]. It parents every
	// per-fire ctx the entry closures pass to factory + publisher. Set
	// once on Start and read on every fire under [Scheduler.mu] only at
	// the time of [Scheduler.Schedule] (the closure captures it once).
	runCtx context.Context //nolint:containedctx // intentional: parent ctx for fire-time closures (see Start godoc)
	// stopCtx is the ctx returned by robfig/cron's Stop. Cached so the
	// idempotent second [Scheduler.Stop] call returns the same value
	// without re-entering the underlying Stop (which would no-op but
	// allocate a fresh derived ctx).
	stopCtx context.Context //nolint:containedctx // intentional: returned to callers from Stop()
}

// New constructs a [Scheduler] backed by `pub`. `pub` is required; passing
// a nil publisher is a programmer error and panics with a clear message
// — matches `lifecycle.New` + `keepclient.WithBaseURL` panic discipline.
// A Scheduler with no publisher could not do anything useful, and
// silently no-oping every fire would mask the bug.
//
// The defaults `logger = nil` (diagnostics disabled) and
// `location = time.UTC` are applied first; supplied options override
// them. The internal robfig/cron is constructed with `WithSeconds()` so
// every entry uses the 6-field spec (`sec min hour dom mon dow`); this
// is required for the polling-deadline test pattern documented in
// `docs/LESSONS.md` (M2b.5) and is fine for production callers, who
// simply prepend a `0 ` to a 5-field spec.
func New(pub LocalPublisher, opts ...Option) *Scheduler {
	if pub == nil {
		panic("cron: New: publisher must not be nil")
	}
	cfg := config{
		location: time.UTC,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	s := &Scheduler{
		pub:    pub,
		logger: cfg.logger,
		cron: robfigcron.New(
			robfigcron.WithSeconds(),
			robfigcron.WithLocation(cfg.location),
		),
	}
	return s
}

// Schedule registers a `(spec, topic, factory)` entry. On every fire
// the scheduler invokes `factory(ctx)` to mint a fresh event, then
// forwards `(ctx, topic, event)` to the [LocalPublisher].
//
// Validation is synchronous and happens BEFORE robfig is touched:
//
//   - Empty `spec` → [ErrInvalidSpec].
//   - Empty `topic` → [ErrInvalidTopic].
//   - Nil `factory` → [ErrInvalidFactory].
//   - Scheduler already Stopped → [ErrAlreadyStopped].
//
// Invalid cron-spec syntax surfaces as
// `fmt.Errorf("cron: parse spec %q: %w", spec, err)` wrapping the
// robfig parser error — `errors.Is(err, ErrInvalidSpec)` does NOT match
// (this is a parser error, not the synchronous-validation case).
//
// Schedule may be called before or after [Scheduler.Start]. When called
// before Start the entry is registered and will fire once Start runs;
// when called after Start robfig adds it to the running schedule
// immediately.
//
// On a per-fire failure (factory panic, publisher error) the scheduler
// recovers, logs via the optional [Logger], and continues — best-effort
// firing semantics, mirroring `notebook.PeriodicBackup`. The next tick
// retries.
func (s *Scheduler) Schedule(spec string, topic string, factory EventFactory) (EntryID, error) {
	if spec == "" {
		return 0, ErrInvalidSpec
	}
	if topic == "" {
		return 0, ErrInvalidTopic
	}
	if factory == nil {
		return 0, ErrInvalidFactory
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == stateStopped {
		return 0, ErrAlreadyStopped
	}
	// Capture the runCtx (or context.Background if not yet started) at
	// register time. If Schedule is called before Start, runCtx is nil;
	// the closure below dereferences s.runCtx lazily on each fire so
	// subsequent Start populates the value transparently.
	id, err := s.cron.AddFunc(spec, s.makeFireFn(topic, factory))
	if err != nil {
		return 0, fmt.Errorf("cron: parse spec %q: %w", spec, err)
	}
	return id, nil
}

// makeFireFn returns the closure that robfig invokes on every fire. The
// closure recovers panics (factory or publish), logs via the optional
// [Logger], and never returns an error to robfig (a returning closure
// could not surface one anyway — robfig calls it as a plain func()).
func (s *Scheduler) makeFireFn(topic string, factory EventFactory) func() {
	return func() {
		// Resolve the per-fire ctx under the lock. The runCtx field is
		// written exactly once in Start; reading it under the same lock
		// makes the data race detector happy without burdening the
		// fire path with atomic.Value plumbing for a one-shot write.
		s.mu.Lock()
		ctx := s.runCtx
		s.mu.Unlock()
		if ctx == nil {
			// Defensive: an entry that fires before Start should never
			// happen (robfig only fires entries on a started cron), but
			// fall back to Background so the closure has a usable ctx.
			ctx = context.Background()
		}

		defer func() {
			if r := recover(); r != nil {
				s.log(ctx, "cron: factory or publish panic recovered",
					"topic", topic,
					"panic", r,
				)
			}
		}()

		event := factory(ctx)
		if err := s.pub.Publish(ctx, topic, event); err != nil {
			s.log(ctx, "cron: publish failed",
				"topic", topic,
				"err", err,
			)
		}
	}
}

// log forwards a diagnostic message to the optional [Logger]. Nil-logger
// safe: a Scheduler constructed without [WithLogger] silently drops.
func (s *Scheduler) log(ctx context.Context, msg string, kv ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Log(ctx, msg, kv...)
}

// Unschedule removes the entry identified by `id` from the running
// schedule. Idempotent: calling twice with the same id (or with an id
// that was never registered) is a no-op — robfig's Remove is
// itself a no-op on missing ids.
//
// Returns [ErrAlreadyStopped] if the scheduler has been Stopped. Once
// Stopped, no entry-set mutation makes sense — the cron is dead.
func (s *Scheduler) Unschedule(id EntryID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == stateStopped {
		return ErrAlreadyStopped
	}
	s.cron.Remove(id)
	return nil
}

// Start begins firing entries. The supplied `ctx` is stored as the
// parent context for every per-fire ctx; cancelling `ctx` does NOT
// stop the scheduler — call [Scheduler.Stop] for that — but
// per-fire factory + publisher calls observe the cancellation through
// the ctx they receive.
//
// Idempotent in the strict sense: a second call returns
// [ErrAlreadyStarted] without re-entering robfig's Start (which would
// otherwise spawn a duplicate runner goroutine).
//
// After Stop, Start cannot resurrect the scheduler. The state machine
// is linear: not-started → started → stopped.
//
//nolint:contextcheck // intentional: ctx is stored for fire-time use, not threaded into robfig's ctx-less Start.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case stateStarted:
		return ErrAlreadyStarted
	case stateStopped:
		return ErrAlreadyStopped
	case stateNotStarted:
		// fallthrough to start
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.runCtx = ctx
	s.cron.Start()
	s.state = stateStarted
	return nil
}

// Stop halts the scheduler and returns robfig's stop-context. Callers
// can `<-ctx.Done()` to wait for any in-flight fire-time closure to
// return — useful for graceful-shutdown tests and production wiring
// that wants to flush the publisher before exiting.
//
// Idempotent: a second call returns the same already-done ctx without
// re-entering robfig's Stop. Stop on a never-Started scheduler is also
// safe — the underlying cron has nothing to drain and the returned ctx
// is immediately done.
//
// After Stop every [Scheduler.Schedule] / [Scheduler.Unschedule]
// returns [ErrAlreadyStopped].
func (s *Scheduler) Stop() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == stateStopped {
		return s.stopCtx
	}
	if s.state == stateStarted {
		s.stopCtx = s.cron.Stop()
	} else {
		// Never started — return an already-done ctx so callers'
		// `<-ctx.Done()` does not block forever waiting on a drain
		// that will never happen.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.stopCtx = ctx
	}
	s.state = stateStopped
	return s.stopCtx
}
