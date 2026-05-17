package slack

import (
	"context"
	"sync"
	"time"
)

// Tier identifies one of Slack's documented rate-limit buckets. Each
// Web API method belongs to exactly one tier; tier-N caps a per-app +
// per-team budget at the published `requests/min` rate. See
// https://api.slack.com/apis/rate-limits — the constants below mirror
// the values published there at the time of M4.2.a.
type Tier int

// The four canonical Slack tiers. Tier-1 is the most restrictive
// (~1 req/min sustained, e.g. `users.list`); tier-4 is the most
// generous (~100 req/min, e.g. `chat.postMessage` for some workspaces).
// `TierUnknown` is the zero value and must NOT be used as a tier in
// production wiring — callers either pass an explicit tier or rely on
// the [defaultTier] fallback ([Tier3]).
const (
	TierUnknown Tier = iota
	Tier1
	Tier2
	Tier3
	Tier4
)

// String returns a stable, lower-case, human-readable name for the
// tier. Useful in log entries.
func (t Tier) String() string {
	switch t {
	case Tier1:
		return "tier1"
	case Tier2:
		return "tier2"
	case Tier3:
		return "tier3"
	case Tier4:
		return "tier4"
	default:
		return "tier-unknown"
	}
}

// defaultTier is the fallback tier used when a method name is not in
// the registry. Slack documents tier-3 as the typical default for Web
// API methods that aren't otherwise classified, so we mirror that.
const defaultTier = Tier3

// defaultTierLimits captures Slack's published per-tier sustained
// rates (requests-per-minute) plus a representative burst size. The
// burst defaults are conservative: Slack's published guidance allows
// "short bursts" without committing to a number, so we set the burst
// equal to the per-minute budget, which is a safe upper bound that
// behaves like a leaky-bucket at steady state.
//
// Callers can override every field via [WithTierLimit].
var defaultTierLimits = map[Tier]TierLimit{
	Tier1: {Sustained: 1, Burst: 1, Window: time.Minute},
	Tier2: {Sustained: 20, Burst: 20, Window: time.Minute},
	Tier3: {Sustained: 50, Burst: 50, Window: time.Minute},
	Tier4: {Sustained: 100, Burst: 100, Window: time.Minute},
}

// TierLimit captures the budget configuration for a single [Tier].
// Sustained is the per-Window request count; Burst is the maximum
// in-flight count the bucket can serve before refilling. Internally
// the limiter computes a per-request refill interval as
// `Window / Sustained` (a leaky-bucket model).
type TierLimit struct {
	// Sustained is the request count permitted per Window. Must be > 0.
	Sustained int

	// Burst is the bucket's capacity — the number of requests served
	// at full speed before throttling kicks in. Must be > 0; values
	// below 1 are clamped to 1.
	Burst int

	// Window is the period over which Sustained requests are allowed.
	// Slack documents per-minute budgets, so the default is
	// [time.Minute]. Non-positive values fall back to [time.Minute].
	Window time.Duration
}

// defaultMethodTiers is the small registry mapping common Slack Web
// API method names to their tier. The list is intentionally minimal —
// only methods M4.2.b/c/d are likely to call appear here. Methods
// absent from the map default to [defaultTier] (Tier3), which is
// Slack's documented fallback for unclassified Web API methods.
//
// Source: https://api.slack.com/apis/rate-limits (consulted at
// M4.2.a; revisit when Slack publishes a stable machine-readable
// classification).
var defaultMethodTiers = map[string]Tier{
	// M4.2.b — message + bot profile.
	"chat.postMessage":  Tier4, // tier-4 (~1 msg/sec/channel; tier-4 envelope)
	"chat.update":       Tier3,
	"chat.delete":       Tier3,
	"users.profile.set": Tier3,
	"users.profile.get": Tier4,
	"bots.info":         Tier3,
	// M4.2.c — Socket Mode bootstrap.
	"apps.connections.open": Tier1,
	// M4.2.d — install / OAuth / manifest.
	"apps.manifest.create":   Tier2,
	"apps.manifest.update":   Tier2,
	"apps.manifest.delete":   Tier2,
	"apps.manifest.export":   Tier2,
	"apps.manifest.validate": Tier2,
	"oauth.v2.access":        Tier3,
	// User lookup helpers.
	"users.info":            Tier4,
	"users.list":            Tier2,
	"users.lookupByEmail":   Tier3,
	"conversations.info":    Tier3,
	"conversations.list":    Tier2,
	"conversations.members": Tier4,
	"conversations.history": Tier3,
	"conversations.open":    Tier3,
	"conversations.create":  Tier2,
	"conversations.invite":  Tier3,
	"conversations.archive": Tier2,
	"auth.test":             Tier3,
}

// RateLimiter is the per-tier token-bucket throttle every [Client]
// request flows through. Construct via [NewRateLimiter]; the zero
// value is not usable. Methods are safe for concurrent use across
// goroutines.
//
// The limiter holds one bucket per [Tier]. [RateLimiter.Wait] blocks
// until the bucket for the supplied method's tier has capacity (or
// ctx is cancelled). [RateLimiter.Allow] is the non-blocking probe.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[Tier]*bucket

	// methodTiers is the (immutable after construction) registry
	// classifying method names into tiers. Methods absent from the
	// map fall through to defaultTier.
	methodTiers map[string]Tier

	// clock is the monotonic time source the buckets read. Tests
	// substitute a fake clock via [WithClock].
	clock func() time.Time
}

// bucket is one token-bucket per [Tier]. Token state is kept as a
// fractional count plus a last-refill timestamp so refill is computed
// on demand without a background goroutine.
type bucket struct {
	mu sync.Mutex

	capacity   float64       // Burst (in tokens)
	refillRate float64       // tokens added per second (Sustained / Window-in-seconds)
	tokens     float64       // current token count, in [0, capacity]
	lastRefill time.Time     // last time tokens was refreshed
	window     time.Duration // for diagnostics; not used in math
}

// RateLimiterOption configures a [RateLimiter] at construction time.
// Pass options to [NewRateLimiter]; later options override earlier
// ones for the same field.
type RateLimiterOption func(*rateLimiterConfig)

// rateLimiterConfig is the internal mutable bag the
// [RateLimiterOption] callbacks populate.
type rateLimiterConfig struct {
	tierLimits  map[Tier]TierLimit
	methodTiers map[string]Tier
	clock       func() time.Time
}

// WithTierLimit overrides the [TierLimit] for a single [Tier]. Pass
// the option once per tier you want to adjust; tiers without an
// override use the default. Non-positive values in the supplied limit
// fall back to the per-field defaults documented on [TierLimit].
func WithTierLimit(tier Tier, limit TierLimit) RateLimiterOption {
	return func(cfg *rateLimiterConfig) {
		if tier == TierUnknown {
			return
		}
		if cfg.tierLimits == nil {
			cfg.tierLimits = make(map[Tier]TierLimit, 4)
		}
		cfg.tierLimits[tier] = limit
	}
}

// WithMethodTier overrides the tier classification for a single
// method name. Useful when Slack publishes an updated tier mapping
// or when a custom Slack instance has different limits. An empty
// method name is a no-op so callers can always pass through whatever
// they have.
func WithMethodTier(method string, tier Tier) RateLimiterOption {
	return func(cfg *rateLimiterConfig) {
		if method == "" || tier == TierUnknown {
			return
		}
		if cfg.methodTiers == nil {
			cfg.methodTiers = make(map[string]Tier, 8)
		}
		cfg.methodTiers[method] = tier
	}
}

// WithRateLimiterClock overrides the monotonic time source the
// buckets read. Defaults to [time.Now]. A nil function is a no-op.
// Tests substitute a deterministic clock so refill can be asserted
// without sleeping.
func WithRateLimiterClock(c func() time.Time) RateLimiterOption {
	return func(cfg *rateLimiterConfig) {
		if c != nil {
			cfg.clock = c
		}
	}
}

// NewRateLimiter constructs a [RateLimiter] with the supplied options.
// All four canonical Slack tiers are pre-populated with their default
// budgets ([defaultTierLimits]) merged with any [WithTierLimit]
// overrides; the method registry is the union of [defaultMethodTiers]
// and any [WithMethodTier] overrides.
func NewRateLimiter(opts ...RateLimiterOption) *RateLimiter {
	cfg := rateLimiterConfig{
		clock: time.Now,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	merged := make(map[Tier]TierLimit, len(defaultTierLimits))
	for t, l := range defaultTierLimits {
		merged[t] = l
	}
	for t, l := range cfg.tierLimits {
		merged[t] = l
	}

	methods := make(map[string]Tier, len(defaultMethodTiers)+len(cfg.methodTiers))
	for m, t := range defaultMethodTiers {
		methods[m] = t
	}
	for m, t := range cfg.methodTiers {
		methods[m] = t
	}

	rl := &RateLimiter{
		buckets:     make(map[Tier]*bucket, len(merged)),
		methodTiers: methods,
		clock:       cfg.clock,
	}
	for t, lim := range merged {
		rl.buckets[t] = newBucket(lim, rl.clock())
	}
	return rl
}

// newBucket builds a bucket for `lim`, normalising non-positive
// values per [TierLimit]. Initial token count equals the bucket
// capacity (full-burst on first request).
func newBucket(lim TierLimit, now time.Time) *bucket {
	sustained := lim.Sustained
	if sustained <= 0 {
		sustained = 1
	}
	burst := lim.Burst
	if burst <= 0 {
		burst = 1
	}
	window := lim.Window
	if window <= 0 {
		window = time.Minute
	}
	return &bucket{
		capacity:   float64(burst),
		refillRate: float64(sustained) / window.Seconds(),
		tokens:     float64(burst),
		lastRefill: now,
		window:     window,
	}
}

// Tier returns the configured tier for the supplied method name.
// Methods absent from the registry return [defaultTier] (Tier3) — the
// documented Slack fallback. An empty method name returns
// [TierUnknown] so the caller can fail fast.
func (rl *RateLimiter) Tier(method string) Tier {
	if method == "" {
		return TierUnknown
	}
	if t, ok := rl.methodTiers[method]; ok {
		return t
	}
	return defaultTier
}

// Allow is the non-blocking probe: it consumes a token from the
// bucket for `method`'s tier and returns true on success, false when
// the bucket is empty. An empty method name returns false (no token
// consumed). Safe for concurrent use.
func (rl *RateLimiter) Allow(method string) bool {
	tier := rl.Tier(method)
	if tier == TierUnknown {
		return false
	}
	b := rl.bucketFor(tier)
	if b == nil {
		return false
	}
	return b.take(rl.clock())
}

// Wait consumes a token from the bucket for `method`'s tier, blocking
// until one is available or ctx is cancelled. Returns ctx.Err() when
// ctx fires before a token is available; returns [ErrUnknownMethod]
// synchronously for an empty method name.
//
// The wait is implemented with a [time.Timer] that fires at the next
// projected refill — no busy-poll, no leaked goroutines.
//
//nolint:contextcheck // intentional: we honour ctx.Done() inside the wait loop.
func (rl *RateLimiter) Wait(ctx context.Context, method string) error {
	if method == "" {
		return ErrUnknownMethod
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tier := rl.Tier(method)
	if tier == TierUnknown {
		return ErrUnknownMethod
	}
	b := rl.bucketFor(tier)
	if b == nil {
		return ErrUnknownMethod
	}
	for {
		now := rl.clock()
		ok, sleep := b.takeOrDelay(now)
		if ok {
			return nil
		}
		// Negative or zero sleep is a programmer error inside take.
		if sleep <= 0 {
			sleep = time.Millisecond
		}
		t := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
			// Loop and try again. The timer is consumed; explicit
			// Stop is unnecessary.
		}
	}
}

// bucketFor returns the bucket for `tier`, or nil when the limiter
// has no entry for it (should not happen with [NewRateLimiter] which
// pre-populates all four tiers, but defensive against a TierUnknown
// slipping through).
func (rl *RateLimiter) bucketFor(tier Tier) *bucket {
	rl.mu.Lock()
	b := rl.buckets[tier]
	rl.mu.Unlock()
	return b
}

// take attempts to consume one token from the bucket. Returns true on
// success, false when the bucket is empty (no token consumed). Refill
// happens on demand based on elapsed wall-clock time since
// lastRefill.
func (b *bucket) take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(now)
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// takeOrDelay is the blocking variant: returns (true, 0) on a
// successful take, otherwise (false, sleep) where `sleep` is the
// duration until the bucket projects to have ≥1 token. Caller is
// expected to wait for `sleep` then retry.
func (b *bucket) takeOrDelay(now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(now)
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	missing := 1 - b.tokens
	if b.refillRate <= 0 {
		// Defensive: a zero refill rate would project an infinite
		// wait. Surface as a long but finite sleep so the caller can
		// observe the misconfiguration via ctx cancellation.
		return false, b.window
	}
	seconds := missing / b.refillRate
	return false, time.Duration(seconds * float64(time.Second))
}

// refill advances the bucket's token count to reflect elapsed time.
// Called under b.mu.
func (b *bucket) refill(now time.Time) {
	elapsed := now.Sub(b.lastRefill)
	if elapsed <= 0 {
		// Clock did not advance (or moved backwards — possible with a
		// fake clock in tests). Treat as a no-op refill but still
		// move lastRefill forward so a subsequent strictly-greater
		// reading produces the correct delta.
		b.lastRefill = now
		return
	}
	added := elapsed.Seconds() * b.refillRate
	b.tokens += added
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.lastRefill = now
}
