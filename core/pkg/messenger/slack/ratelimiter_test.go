package slack

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a deterministic time source the rate-limiter tests use
// to avoid sleeping. Time advances only when the test calls Advance;
// concurrent reads of Now() are safe.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// TestTier_String asserts the human-readable form of each tier — log
// entries embedding tier identity rely on the format.
func TestTier_String(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tier Tier
		want string
	}{
		{TierUnknown, "tier-unknown"},
		{Tier1, "tier1"},
		{Tier2, "tier2"},
		{Tier3, "tier3"},
		{Tier4, "tier4"},
	}
	for _, tc := range cases {
		if got := tc.tier.String(); got != tc.want {
			t.Errorf("Tier(%d).String() = %q, want %q", tc.tier, got, tc.want)
		}
	}
}

// TestRateLimiter_Tier_KnownAndUnknown asserts the registry returns
// the expected tier for documented methods and falls through to the
// default for unknown methods.
func TestRateLimiter_Tier_KnownAndUnknown(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()

	if got := rl.Tier("chat.postMessage"); got != Tier4 {
		t.Errorf("chat.postMessage tier = %v, want %v", got, Tier4)
	}
	if got := rl.Tier("users.list"); got != Tier2 {
		t.Errorf("users.list tier = %v, want %v", got, Tier2)
	}
	if got := rl.Tier("apps.connections.open"); got != Tier1 {
		t.Errorf("apps.connections.open tier = %v, want %v", got, Tier1)
	}

	// Unknown method falls through to Tier3.
	if got := rl.Tier("not.a.real.method"); got != defaultTier {
		t.Errorf("unknown method tier = %v, want %v", got, defaultTier)
	}

	// Empty method => TierUnknown.
	if got := rl.Tier(""); got != TierUnknown {
		t.Errorf("empty method tier = %v, want TierUnknown", got)
	}
}

// TestRateLimiter_WithMethodTier_Override asserts a caller can
// reclassify a method via WithMethodTier.
func TestRateLimiter_WithMethodTier_Override(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(WithMethodTier("chat.postMessage", Tier1))
	if got := rl.Tier("chat.postMessage"); got != Tier1 {
		t.Errorf("override tier = %v, want Tier1", got)
	}

	// Empty method override is a no-op.
	rl2 := NewRateLimiter(WithMethodTier("", Tier1))
	if got := rl2.Tier("chat.postMessage"); got != Tier4 {
		t.Errorf("empty-override tier = %v, want Tier4", got)
	}
}

// TestRateLimiter_Allow_Burst asserts that a freshly-constructed
// limiter serves Burst calls without blocking, then refuses the
// (Burst+1)-th call until tokens refill.
func TestRateLimiter_Allow_Burst(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	rl := NewRateLimiter(
		WithRateLimiterClock(clk.Now),
		// Tighten Tier3 to make the assertions cheap.
		WithTierLimit(Tier3, TierLimit{Sustained: 5, Burst: 5, Window: time.Minute}),
	)

	// Tier3 is the default for unknown methods.
	for i := 0; i < 5; i++ {
		if !rl.Allow("not.a.real.method") {
			t.Fatalf("Allow #%d returned false, want true (within burst)", i+1)
		}
	}
	if rl.Allow("not.a.real.method") {
		t.Error("Allow #6 returned true, want false (burst exhausted)")
	}
}

// TestRateLimiter_Allow_Refill asserts that once the bucket is empty,
// advancing the clock by the per-token interval makes Allow succeed
// again. Sustained=12/min => 1 token / 5s.
func TestRateLimiter_Allow_Refill(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	rl := NewRateLimiter(
		WithRateLimiterClock(clk.Now),
		WithTierLimit(Tier3, TierLimit{Sustained: 12, Burst: 1, Window: time.Minute}),
	)

	if !rl.Allow("not.a.real.method") {
		t.Fatal("first Allow returned false, want true")
	}
	if rl.Allow("not.a.real.method") {
		t.Fatal("second Allow returned true, want false (bucket empty)")
	}

	// Advance by 5s — 1 token should refill.
	clk.Advance(5 * time.Second)
	if !rl.Allow("not.a.real.method") {
		t.Error("post-refill Allow returned false, want true")
	}
}

// TestRateLimiter_Allow_PerTierIsolation asserts throttling tier-3
// does NOT consume tier-2 tokens. Verifies the bucket-per-tier model.
func TestRateLimiter_Allow_PerTierIsolation(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	rl := NewRateLimiter(
		WithRateLimiterClock(clk.Now),
		WithTierLimit(Tier2, TierLimit{Sustained: 3, Burst: 3, Window: time.Minute}),
		WithTierLimit(Tier3, TierLimit{Sustained: 1, Burst: 1, Window: time.Minute}),
	)

	// Drain Tier3 (one token).
	if !rl.Allow("not.a.real.method") { // unknown => Tier3
		t.Fatal("tier3 first Allow returned false")
	}
	if rl.Allow("not.a.real.method") {
		t.Fatal("tier3 second Allow returned true, want false (drained)")
	}

	// Tier2 must remain unaffected — `users.list` is Tier2.
	for i := 0; i < 3; i++ {
		if !rl.Allow("users.list") {
			t.Errorf("tier2 Allow #%d returned false despite tier3 drain", i+1)
		}
	}
	if rl.Allow("users.list") {
		t.Error("tier2 Allow #4 returned true, want false (now drained)")
	}
}

// TestRateLimiter_Allow_EmptyMethod asserts an empty method name is
// rejected by Allow without consuming a token.
func TestRateLimiter_Allow_EmptyMethod(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter()
	if rl.Allow("") {
		t.Error("Allow(\"\") returned true, want false")
	}
}

// TestRateLimiter_Wait_Succeeds asserts Wait returns nil when the
// bucket has tokens.
func TestRateLimiter_Wait_Succeeds(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rl.Wait(ctx, "chat.postMessage"); err != nil {
		t.Errorf("Wait: %v", err)
	}
}

// TestRateLimiter_Wait_HonorsCtxCancel asserts Wait returns ctx.Err()
// promptly when the bucket is drained and the ctx is cancelled.
func TestRateLimiter_Wait_HonorsCtxCancel(t *testing.T) {
	t.Parallel()

	// Drain Tier3 (sustained=1, burst=1, window=1m) by pinning the
	// clock — without a real clock advance the bucket cannot refill.
	clk := newFakeClock(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	rl := NewRateLimiter(
		WithRateLimiterClock(clk.Now),
		WithTierLimit(Tier3, TierLimit{Sustained: 1, Burst: 1, Window: time.Minute}),
	)
	if !rl.Allow("not.a.real.method") {
		t.Fatal("Allow returned false; bucket not draining as expected")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel; Wait must surface ctx.Err immediately.
	err := rl.Wait(ctx, "not.a.real.method")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Wait err = %v, want context.Canceled", err)
	}
}

// TestRateLimiter_Wait_HonorsCtxDeadline asserts Wait surfaces a
// deadline-exceeded error when the bucket cannot refill in time.
func TestRateLimiter_Wait_HonorsCtxDeadline(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(
		WithTierLimit(Tier3, TierLimit{Sustained: 1, Burst: 1, Window: time.Hour}),
	)
	if !rl.Allow("not.a.real.method") {
		t.Fatal("Allow returned false; bucket not draining as expected")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := rl.Wait(ctx, "not.a.real.method")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait err = %v, want context.DeadlineExceeded", err)
	}
}

// TestRateLimiter_Wait_EmptyMethod asserts Wait surfaces
// ErrUnknownMethod for an empty method name.
func TestRateLimiter_Wait_EmptyMethod(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	if err := rl.Wait(context.Background(), ""); !errors.Is(err, ErrUnknownMethod) {
		t.Errorf("Wait empty err = %v, want ErrUnknownMethod", err)
	}
}

// TestRateLimiter_Wait_NilCtxNotCancelled is a sanity check: a
// fresh background ctx never blocks Wait when the bucket has tokens.
func TestRateLimiter_Wait_NilCtxNotCancelled(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter()
	if err := rl.Wait(context.Background(), "auth.test"); err != nil {
		t.Errorf("Wait: %v", err)
	}
}

// TestRateLimiter_ConcurrentAllow asserts the bucket is race-clean
// under parallel Allow load. Driven by goroutines hammering the same
// tier; the total count of `true` returns must equal the burst budget.
// Run under `-race` to catch missing synchronization.
func TestRateLimiter_ConcurrentAllow(t *testing.T) {
	t.Parallel()

	const burst = 50
	clk := newFakeClock(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	rl := NewRateLimiter(
		WithRateLimiterClock(clk.Now),
		WithTierLimit(Tier3, TierLimit{Sustained: burst, Burst: burst, Window: time.Hour}),
	)

	const goroutines = 32
	const callsPer = 4
	var allowed atomic.Int32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsPer; j++ {
				if rl.Allow("not.a.real.method") {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	// We expect EXACTLY `burst` allowances (the bucket starts full)
	// and no more — the clock did not advance, so refill cannot have
	// added tokens. A race would either over- or under-count.
	if got := int(allowed.Load()); got != burst {
		t.Errorf("allowed = %d, want %d", got, burst)
	}
}

// TestRateLimiter_TierLimit_DefaultClamping asserts non-positive
// values in a TierLimit fall back to the per-field defaults rather
// than producing zero-rate buckets.
func TestRateLimiter_TierLimit_DefaultClamping(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(
		WithTierLimit(Tier3, TierLimit{Sustained: 0, Burst: 0, Window: 0}),
	)
	// With clamping, Sustained=1 / Burst=1 / Window=1m. The first
	// Allow must succeed, the second must fail.
	if !rl.Allow("not.a.real.method") {
		t.Error("clamped first Allow returned false, want true")
	}
	if rl.Allow("not.a.real.method") {
		t.Error("clamped second Allow returned true, want false")
	}
}

// TestRateLimiter_WithTierLimit_TierUnknown_NoOp asserts that
// passing TierUnknown to WithTierLimit is silently ignored — the
// default tier table is preserved.
func TestRateLimiter_WithTierLimit_TierUnknown_NoOp(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(
		WithTierLimit(TierUnknown, TierLimit{Sustained: 1, Burst: 1, Window: time.Minute}),
	)
	// Default Tier3 budget (Sustained=50/Burst=50) must still apply.
	for i := 0; i < 50; i++ {
		if !rl.Allow("not.a.real.method") {
			t.Fatalf("Allow #%d returned false, want true (TierUnknown override should be no-op)", i+1)
		}
	}
}

// TestRateLimiter_WithRateLimiterClock_NilNoOp asserts a nil clock
// option is silently ignored (the default time.Now stays in place).
func TestRateLimiter_WithRateLimiterClock_NilNoOp(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(WithRateLimiterClock(nil))
	// Just verifying construction doesn't panic and Allow works.
	if !rl.Allow("auth.test") {
		t.Error("Allow returned false, want true")
	}
}
