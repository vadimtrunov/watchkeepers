package toolregistry

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Handler is the callback shape consumed by the [Subscriber] seam.
// Aliased exactly to `eventbus.Handler` (the concrete `*eventbus.Bus`
// satisfies [Subscriber] via structural typing); the compile-time
// assertion in `registry_test.go` pins the equality.
type Handler = func(ctx context.Context, event any)

// Subscriber is the [eventbus.Bus] subset the [Registry] consumes —
// only the [Subscriber.Subscribe] method. Defined here so the
// registry stays decoupled from the concrete `*eventbus.Bus` import
// (mirrors the [Publisher] and `cron.LocalPublisher` shape).
//
// Subscribe registers `handler` against `topic` and returns a
// single-shot unsubscribe callback. The eventbus dispatches each
// subscribed topic sequentially from a dedicated worker goroutine, so
// the handler MUST NOT block indefinitely — a slow handler stalls
// every subsequent event on the same topic.
//
// Contract on the returned unsubscribe callback: it MUST be non-nil
// even when Subscribe returns an error. Callers idiomatically write
// `unsub, err := sub.Subscribe(...); defer unsub()` and a nil
// callback would panic on `defer`. [eventbus.Bus] satisfies this
// contract; alternative fakes MUST do the same.
type Subscriber interface {
	Subscribe(topic string, handler Handler) (func(), error)
}

// RegistryDeps groups the required dependencies of a [Registry].
// Construct via [NewRegistry]; nil / zero values for required fields
// panic or return an error.
type RegistryDeps struct {
	// FS is the file-system seam the [Registry] consumes via the
	// scanner ([ScanSourceDir] / [BuildEffective]). Required;
	// non-nil.
	FS FS

	// DataDir is the operator-configured `$DATA_DIR` root. The
	// scanner reads from `<DataDir>/tools/<sourceName>/` for each
	// configured source. Required; non-empty (whitespace-only is
	// rejected with [ErrInvalidDataDir]).
	DataDir string

	// Clock is the time seam — sourced by [Registry.Recompute] for
	// [EffectiveToolset.BuiltAt] and the correlation id. Required;
	// non-nil.
	Clock Clock

	// GracePeriod is the duration the [Registry] tracks a retiring
	// snapshot (the previous one, post-swap) before forgetting it.
	// During the grace window the registry's diagnostic surface
	// exposes the retiring entry's refcount; after the window
	// elapses the registry drops its tracking reference and a
	// `effective_toolset_retired` log fires. The on-disk snapshot
	// itself stays alive as long as any in-flight caller holds a
	// reference (Go GC reclaims it once every reference drops).
	//
	// Zero is allowed and behaves as "forget on the next Recompute"
	// — useful in tests. Negative values are rejected with
	// [ErrInvalidGracePeriod].
	GracePeriod time.Duration
}

// RegistryOption configures a [Registry] at construction time.
type RegistryOption func(*Registry)

// WithRegistryPublisher wires an optional [Publisher] for emitting
// [TopicEffectiveToolsetUpdated]. A nil publisher disables emission
// — callers that consume the snapshot via pull-based [Registry.Snapshot]
// / [Registry.Acquire] do not need the event.
func WithRegistryPublisher(p Publisher) RegistryOption {
	return func(r *Registry) {
		if p != nil {
			r.publisher = p
		}
	}
}

// WithRegistryLogger wires an optional [Logger]. Nil-logger safe.
func WithRegistryLogger(l Logger) RegistryOption {
	return func(r *Registry) {
		if l != nil {
			r.logger = l
		}
	}
}

// registryEntry binds an immutable [EffectiveToolset] to a refcount
// the [Registry] uses to track in-flight [Registry.Acquire] holders.
// The entry pointer is stable — swapped atomically into / out of
// [Registry.current] in one shot so an Acquire that observed
// `entry.snapshot` always sees a coherent (refcount, snapshot) pair.
//
// `retiredAt` is set ONCE when the entry is demoted from current to
// retiring, under [Registry.retireMu]. Read-only after that.
type registryEntry struct {
	snapshot  *EffectiveToolset
	refcount  atomic.Int32
	retiredAt time.Time
}

// Registry maintains the current [EffectiveToolset], recomputed on
// each successful tool-source sync. Construct via [NewRegistry]; the
// zero value is not usable. Safe for concurrent
// [Registry.Acquire] / [Registry.Snapshot] / [Registry.Recompute]
// from many goroutines.
//
// # Lifecycle
//
//  1. [NewRegistry] installs a synthetic empty snapshot at revision
//     0 so [Registry.Acquire] never returns nil even before the first
//     [Registry.Recompute].
//  2. [Registry.Start] subscribes to [TopicSourceSynced]; each event
//     fires [Registry.Recompute] in the eventbus worker goroutine.
//  3. [Registry.Recompute] rescans every configured source's
//     directory via [BuildEffective], swaps the registry's atomic
//     pointer, retires the previous entry with a grace-period
//     deadline, and (if a [Publisher] is wired) emits
//     [TopicEffectiveToolsetUpdated].
//
// # In-flight vs new boundary
//
// [Registry.Acquire] returns a `(snapshot, release)` pair. The
// snapshot is captured by `atomic.Pointer` load; subsequent
// [Registry.Recompute] calls swap the pointer for new acquirers but
// leave the captured snapshot pointer untouched in the caller's
// stack. In-flight `InvokeTool` callers therefore complete on the
// snapshot they captured; new callers see the new snapshot. The
// refcount tracked on the retiring entry is purely diagnostic — it
// records how many in-flight calls are still on the old version for
// telemetry and the future M9.3 cleanup hooks, but no cleanup is
// forced.
type Registry struct {
	deps    RegistryDeps
	sources []SourceConfig

	publisher Publisher
	logger    Logger

	revCounter atomic.Int64
	current    atomic.Pointer[registryEntry]

	// recomputeMu serialises concurrent [Registry.Recompute] calls
	// across the build + revision-allocation + atomic-swap +
	// retire-bookkeeping critical section. The eventbus's per-topic
	// worker already serialises subscriber dispatches, but external
	// callers (tests, operator CLI in future milestones) may invoke
	// Recompute directly while the subscriber is also firing — this
	// mutex keeps the revision counter and the retiring list
	// consistent.
	recomputeMu sync.Mutex

	// publishMu serialises the publish-loop phase of concurrent
	// [Registry.Recompute] calls. Held AFTER recomputeMu has been
	// released (so the in-memory swap remains the M9.1.b fast path —
	// a slow / backpressured publisher does NOT serialise other
	// recomputes' scan + swap) but BEFORE any of the per-shadow
	// [TopicToolShadowed] publishes OR the final
	// [TopicEffectiveToolsetUpdated] publish. M9.2 widened the
	// post-swap publish window from O(1) to O(len(shadows)); without
	// this second mutex, revision N+1's shadow events could
	// interleave on the bus with revision N's still-in-flight shadow
	// events, breaking the per-revision FIFO contract on each topic.
	publishMu sync.Mutex

	// retireMu guards [Registry.retiring]. Held briefly during
	// Recompute (append + sweep) and during diagnostic accessors.
	retireMu sync.Mutex
	retiring []*registryEntry
}

// NewRegistry constructs a [Registry] with `deps` and `sources`
// validated. Panics on nil required deps; returns an error on bad
// config values.
//
// The initial snapshot is a synthetic empty [EffectiveToolset] at
// revision 0 with `BuiltAt = deps.Clock.Now()`. The first
// [Registry.Recompute] call increments the revision to 1; subsequent
// recomputes advance monotonically.
func NewRegistry(deps RegistryDeps, sources []SourceConfig, opts ...RegistryOption) (*Registry, error) {
	if deps.FS == nil {
		panic("toolregistry: NewRegistry: deps.FS must not be nil")
	}
	if deps.Clock == nil {
		panic("toolregistry: NewRegistry: deps.Clock must not be nil")
	}
	if strings.TrimSpace(deps.DataDir) == "" {
		return nil, ErrInvalidDataDir
	}
	if deps.GracePeriod < 0 {
		return nil, fmt.Errorf("%w: %s", ErrInvalidGracePeriod, deps.GracePeriod)
	}
	if err := ValidateSources(sources); err != nil {
		return nil, err
	}

	r := &Registry{
		deps:    deps,
		sources: CloneSources(sources),
	}
	for _, opt := range opts {
		opt(r)
	}

	initial := newEffectiveToolset(0, deps.Clock.Now(), nil)
	r.current.Store(&registryEntry{snapshot: initial})
	return r, nil
}

// Sources returns a defensive copy of the configured source list.
// Convenience accessor mirroring [Scheduler.Sources]; callers can
// inspect the configured set without grabbing a reference to the
// registry's internal slice.
func (r *Registry) Sources() []SourceConfig {
	return CloneSources(r.sources)
}

// Snapshot returns the current [EffectiveToolset] without
// incrementing any refcount. Use this when you only need a read of
// the current state and you are OK with the snapshot being retired
// before you finish (the snapshot itself stays alive via Go GC, but
// the [Registry]'s in-flight-call tracking will not count this
// observation).
//
// For an InvokeTool-style consumer that must observe a single
// consistent snapshot for the duration of a call AND have its
// presence counted in the retiring entry's refcount, use
// [Registry.Acquire] instead.
func (r *Registry) Snapshot() *EffectiveToolset {
	return r.current.Load().snapshot
}

// Acquire returns the current [EffectiveToolset] and an idempotent
// release callback. The snapshot is the result of an
// `atomic.Pointer` load; subsequent [Registry.Recompute] calls swap
// the pointer for new acquirers but leave this snapshot's contents
// untouched. The release callback decrements the refcount on the
// entry the caller captured.
//
// Release is idempotent — calling it twice is a no-op. The
// [Registry]'s retire-sweep is purely time-based: it does NOT gate
// retirement on the refcount, so a leaked release does NOT keep the
// retiring entry tracked beyond the configured
// [RegistryDeps.GracePeriod]. The sweep's `refcount_at_retirement`
// log field surfaces any non-zero leak to operators when the entry
// is dropped from the retiring list; downstream M9.3 will use this
// surface to detect runaway in-flight calls.
//
// Acquire MAY return a snapshot belonging to a RETIRED entry under
// a tight race with [Registry.Recompute]: the caller loads the
// pointer, [Registry.Recompute] swaps + retires, then Acquire's
// `refcount.Add(1)` increments the retired entry's counter. This is
// the intended in-flight-vs-new contract — the caller's snapshot is
// the one current at the moment of load, and the snapshot's
// contents stay valid via Go GC.
func (r *Registry) Acquire() (*EffectiveToolset, func()) {
	entry := r.current.Load()
	entry.refcount.Add(1)
	var released atomic.Bool
	return entry.snapshot, func() {
		if released.CompareAndSwap(false, true) {
			entry.refcount.Add(-1)
		}
	}
}

// Recompute rescans every configured source's directory, builds a
// new [EffectiveToolset], atomically installs it as current, and (if
// a [Publisher] is wired) emits [TopicEffectiveToolsetUpdated]. The
// previous entry is appended to the retiring list with `retiredAt`
// set; entries past [RegistryDeps.GracePeriod] are swept and logged.
//
// Returns:
//
//   - nil + the new snapshot on success (with or without a wired
//     publisher).
//   - ctx-cancel / ctx-deadline-exceeded if the scan was interrupted
//     by the caller BEFORE the atomic swap; the previous snapshot
//     stays installed and the revision counter is NOT advanced
//     (revisions are consumed only on successful builds, so a
//     subscriber that watches `Revision` sees a contiguous sequence
//     across surviving recomputes).
//   - A non-nil error wrapping [ErrPublishAfterSwap] if the swap
//     succeeded but the subsequent [TopicEffectiveToolsetUpdated]
//     [Publisher.Publish] failed (the wrapped chain still satisfies
//     `errors.Is(..., context.Canceled)` when the failure was a
//     cancellation). The atomic swap has already committed, so the
//     next [Registry.Acquire] reads the new snapshot — callers MUST
//     check `errors.Is(err, ErrPublishAfterSwap)` to distinguish
//     "state committed, notification missed" from "scan aborted,
//     no change."
//   - A non-nil error wrapping [ErrPublishToolShadowed] if the swap
//     and [TopicEffectiveToolsetUpdated] publish both succeeded but
//     one or more per-shadow [TopicToolShadowed] publishes failed.
//     M9.2 emits shadow events BEFORE the toolset-updated event, and
//     each shadow is independent — Recompute publishes every shadow
//     in the list and returns the FIRST publish failure (subsequent
//     failures land in the [Logger] only). The state is fully
//     committed; subscribers that missed a `tool_shadowed` event
//     have NO automatic recovery — `errors.Is(err, ErrPublishToolShadowed)`
//     is the external retry / paging hook.
//
// Concurrency: [Registry.recomputeMu] serialises the revision
// allocation + atomic swap + retiring-list mutation. The mutex is
// RELEASED before [Publisher.Publish] runs — a slow / backpressured
// subscriber MUST NOT serialise every recompute. Consequence: the
// retiring-list state observed via [Registry.RetiringRefcounts]
// reflects swaps already committed even when an in-flight Publish
// has not yet completed. The publisher-error-after-swap path does
// NOT roll back the swap or the retire bookkeeping; the snapshot is
// the durable signal, the event is the wake-up.
func (r *Registry) Recompute(ctx context.Context) (*EffectiveToolset, error) {
	builtAt := r.deps.Clock.Now()

	r.recomputeMu.Lock()
	snap, shadows, err := BuildEffective(ctx, r.deps.FS, r.deps.DataDir, r.sources, builtAt, r.logger)
	if err != nil {
		r.recomputeMu.Unlock()
		return nil, err
	}
	revision := r.revCounter.Add(1)
	snap.Revision = revision
	correlationID := strconv.FormatInt(builtAt.UnixNano(), 10)

	newEntry := &registryEntry{snapshot: snap}
	oldEntry := r.current.Swap(newEntry)
	r.retireEntry(ctx, oldEntry, builtAt)
	publisher := r.publisher
	r.recomputeMu.Unlock()

	if publisher == nil {
		// Surface shadows even when no publisher is wired so a
		// future caller building [Registry] without [Publisher]
		// (M9.4 dry-run, M9.5 local-patch verification, operator CLI)
		// does NOT silently lose the shadow signal. The summary log
		// includes per-shadow (tool_name, winner, shadowed) so a
		// reader can reconstruct the conflict matrix without
		// stitching multiple subsystems together.
		if len(shadows) > 0 {
			r.logShadowsDropped(ctx, revision, shadows)
		}
		return snap, nil
	}

	// publishMu serialises the publish phase across concurrent
	// [Registry.Recompute] calls: while two concurrent Recomputes
	// can scan + swap in parallel (the M9.1.b fast path), only one
	// publishes events to the bus at a time. This keeps per-topic
	// FIFO order consistent with revision order — without it, a
	// concurrent Recompute that swapped revision N+1 could begin
	// publishing its shadow events on the same topic as revision N
	// is still walking, and the topic's worker would dispatch them
	// interleaved. The mutex is RELEASED before this function
	// returns; a slow / backpressured publisher only blocks
	// publishing, not scanning + swapping.
	r.publishMu.Lock()
	defer r.publishMu.Unlock()

	// Shadow events go out FIRST so a subscriber that observes both
	// topics learns about every shadow BEFORE seeing the revision
	// bump on the same topic worker. NOTE on cross-topic delivery
	// order: this is the publish-CALL order at the bus boundary,
	// not the subscriber DELIVERY order — [eventbus.Bus] runs a
	// per-topic worker goroutine, so a subscriber registered on
	// both topics can still observe `tool_shadowed` AFTER
	// `effective_toolset_updated` for the same revision if the
	// updated-topic worker drains its buffered channel faster than
	// the shadowed-topic worker. Subscribers requiring a strict
	// before/after relationship MUST correlate via [ToolShadowed.CorrelationID]
	// + [EffectiveToolsetUpdated.CorrelationID] (set to the same
	// value here).
	//
	// Partial-delivery is preferable to first-error-aborts: each
	// shadow event is independent and a subscriber that gets K of N
	// notifications is strictly better off than zero. The FIRST
	// publish failure is wrapped onto the return; subsequent
	// failures are logged only.
	var firstShadowErr error
	for _, sh := range shadows {
		if err := publisher.Publish(ctx, TopicToolShadowed, newToolShadowedEvent(sh, revision, builtAt, correlationID)); err != nil {
			r.log(
				ctx, "toolregistry: publish tool_shadowed failed",
				"revision", revision,
				"tool_name", sh.ToolName,
				"shadowed_source", sh.ShadowedSource,
				"err_type", leafErrType(err),
			)
			if firstShadowErr == nil {
				firstShadowErr = err
			}
		}
	}

	ev := EffectiveToolsetUpdated{
		Revision:      revision,
		BuiltAt:       builtAt,
		ToolCount:     snap.Len(),
		SourceCount:   len(r.sources),
		CorrelationID: correlationID,
	}
	if err := publisher.Publish(ctx, TopicEffectiveToolsetUpdated, ev); err != nil {
		r.log(
			ctx, "toolregistry: publish effective_toolset_updated failed",
			"revision", revision,
			"err_type", leafErrType(err),
		)
		return snap, fmt.Errorf("%w: revision %d: %w", ErrPublishAfterSwap, revision, err)
	}
	if firstShadowErr != nil {
		return snap, fmt.Errorf("%w: revision %d: %w", ErrPublishToolShadowed, revision, firstShadowErr)
	}
	return snap, nil
}

// logShadowsDropped emits a per-recompute summary log for the
// no-publisher branch — one log entry per shadow + a single
// "shadows dropped" header line. Without this, a caller that
// constructs a [Registry] without a [Publisher] would silently lose
// every cross-source conflict signal. The discipline mirrors the
// M9.1.b sentinel log discipline (one line per actionable diagnostic,
// no error MESSAGE bodies — type-only).
func (r *Registry) logShadowsDropped(ctx context.Context, revision int64, shadows []ShadowedTool) {
	r.log(
		ctx, "toolregistry: shadows dropped (no publisher wired)",
		"revision", revision,
		"shadow_count", len(shadows),
	)
	for _, sh := range shadows {
		r.log(
			ctx, "toolregistry: shadow detected",
			"revision", revision,
			"tool_name", sh.ToolName,
			"winner_source", sh.WinnerSource,
			"winner_version", sh.WinnerVersion,
			"shadowed_source", sh.ShadowedSource,
			"shadowed_version", sh.ShadowedVersion,
		)
	}
}

// Start subscribes the registry to [TopicSourceSynced] on `sub` and
// returns an unsubscribe callback. Every [SourceSynced] event
// triggers [Registry.Recompute] synchronously inside the eventbus
// worker goroutine; the eventbus's per-topic sequential dispatch +
// the registry's [recomputeMu] together prevent overlapping
// recomputes from the subscriber path.
//
// Recompute errors are logged but do not unsubscribe — a single
// transient scan failure (e.g., concurrent file-system shuffling)
// must not stop future updates. Persistent failures surface via the
// log entry's `err_type`.
//
// The `ctx` parameter is reserved for future lifecycle wiring
// (auto-unsubscribe on cancellation) and is currently unused; the
// returned unsubscribe callback is the only termination path today.
// The callback is non-nil even when Subscribe returns an error
// (matches the [Subscriber] interface contract), so callers can
// idiomatically `defer unsub()`.
func (r *Registry) Start(_ context.Context, sub Subscriber) (func(), error) {
	if sub == nil {
		panic("toolregistry: Registry.Start: subscriber must not be nil")
	}
	handler := func(ctx context.Context, _ any) {
		if _, err := r.Recompute(ctx); err != nil {
			r.log(
				ctx, "toolregistry: recompute on source_synced failed",
				"err_type", leafErrType(err),
			)
		}
	}
	return sub.Subscribe(TopicSourceSynced, handler)
}

// RetiringRefcounts returns a per-revision snapshot of the refcounts
// of every retiring entry the registry is still tracking. Useful for
// tests and operator dashboards — diagnostic surface only. The
// returned map is keyed by [EffectiveToolset.Revision]; Go maps
// have no iteration order so callers that need a sorted view must
// sort the keys themselves.
func (r *Registry) RetiringRefcounts() map[int64]int32 {
	r.retireMu.Lock()
	defer r.retireMu.Unlock()
	out := make(map[int64]int32, len(r.retiring))
	for _, e := range r.retiring {
		out[e.snapshot.Revision] = e.refcount.Load()
	}
	return out
}

// retireEntry appends `old` to the retiring list and sweeps any
// entries past the grace deadline. Called from [Registry.Recompute]
// under [Registry.recomputeMu]; the inner [Registry.retireMu] gates
// concurrent access to the retiring slice from diagnostic accessors.
func (r *Registry) retireEntry(ctx context.Context, old *registryEntry, now time.Time) {
	if old == nil {
		return
	}
	r.retireMu.Lock()
	defer r.retireMu.Unlock()
	old.retiredAt = now
	r.retiring = append(r.retiring, old)
	r.sweepRetiring(ctx, now)
}

// sweepRetiring drops retiring entries whose `retiredAt + GracePeriod`
// is on or before `now`, logging each retirement. Caller MUST hold
// [Registry.retireMu]. The retiring slice is rewritten in-place so
// surviving entries keep their relative order.
func (r *Registry) sweepRetiring(ctx context.Context, now time.Time) {
	if len(r.retiring) == 0 {
		return
	}
	kept := r.retiring[:0]
	for _, e := range r.retiring {
		if now.Sub(e.retiredAt) >= r.deps.GracePeriod {
			r.log(
				ctx, "toolregistry: effective_toolset retired",
				"revision", e.snapshot.Revision,
				"retired_at", e.retiredAt,
				"refcount_at_retirement", e.refcount.Load(),
			)
			continue
		}
		kept = append(kept, e)
	}
	r.retiring = kept
}

// log forwards a diagnostic message to the optional [Logger].
func (r *Registry) log(ctx context.Context, msg string, kv ...any) {
	if r.logger == nil {
		return
	}
	r.logger.Log(ctx, msg, kv...)
}
