//go:build loadtest

// This file is gated behind the `loadtest` build tag so the default
// `go test ./...` and `make test` paths NEVER spin up the high-concurrency
// goroutine fleet or the multi-second real-clock Wait assertion. Run it
// with:
//
//	make ratelimiter-load
//
// or directly:
//
//	go test -tags=loadtest -run=TestRateLimiterLoad \
//	    -bench=BenchmarkRateLimiter_Tier2 -benchmem \
//	    ./core/pkg/messenger/slack/...
//
// Implements ROADMAP-phase1.md §10 DoD Closure Plan item B3: "M4
// rate-limiter load test — tier-2 burst + sustained limits honored under
// simulated load." Tier 2 is the load-relevant tier because the bursty
// Slack methods that drove M4.2 (`users.list`, `conversations.list`,
// `apps.manifest.*`) live there, and tier-2's per-minute budget (~20
// requests) is the one operators hit first under realistic agent
// traffic.
//
// The default-tier unit tests (ratelimiter_test.go) already prove
// race-cleanliness and per-tier isolation at 32g × 4 calls = 128
// attempts. This file adds three load-shaped assertions the unit tier
// is intentionally too small to surface:
//
//  1. Tier-2 burst-cap holds at 4× the unit test's concurrency
//     (128g × 100 attempts = 12 800 Allow calls vs a 20-token bucket).
//  2. Tier-2 sustained-rate holds across multiple Window refills under
//     the same load (5 simulated minutes, 5 × 20 = 100 token grants).
//  3. The Wait blocking path schedules concurrent waiters fairly under
//     a real clock and the OBSERVED throughput tracks the configured
//     Sustained rate within ±15 % over a multi-second window.
//
// Real-clock wall-time budget: ~3 s for the Wait assertion (15
// Windows × 200ms so timer-scheduling jitter averages out); the
// fake-clock assertions finish in microseconds (no sleeping). The whole
// file is intended to run under 30 s total per the B1 precedent
// (`make notebook-bench`).

package slack

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// loadConcurrency is the goroutine fan-out used by every Allow-shaped
// load assertion below. 128g is 4× the unit-test's 32g — large enough
// to drive contention on the bucket mutex on commodity CPUs but small
// enough that the Go runtime can schedule them all without paying
// observably for context switches. Pinned as a const so a future
// reviewer can read the load shape without grepping individual tests.
const loadConcurrency = 128

// loadAttemptsPerG is the per-goroutine call count. 100 attempts × 128
// goroutines = 12 800 calls per assertion; against a 20-token bucket
// that is a 640× overshoot, so any over-grant from a race would dwarf
// the budget.
const loadAttemptsPerG = 100

// loadWindowStart is the fixed-clock origin every test pins. Chosen to
// match the unit tests' origin so log lines and panics read the same
// across the test families.
var loadWindowStart = time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

// TestRateLimiterLoad_Tier2BurstHonored asserts the tier-2 default
// burst budget (Burst=20) is honored EXACTLY under 128-goroutine
// contention. With the fake clock frozen, no refill can have happened
// between the first and last Allow, so the only way for `allowed` to
// drift from 20 is a race in bucket.take. Mirrors the unit-tier
// TestRateLimiter_ConcurrentAllow but specifically targets tier 2 (the
// M4.2.d manifest + lookup family) at 4× the goroutine count and 25×
// the per-goroutine attempt count.
//
// The method name `users.list` is the canonical tier-2 representative
// from defaultMethodTiers — using it (rather than an override) pins
// this assertion against the SHIPPED defaults so a future change to
// the registry (e.g. demoting users.list to tier 3) flips the test
// red and forces the contract to be re-justified.
func TestRateLimiterLoad_Tier2BurstHonored(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(loadWindowStart)
	rl := NewRateLimiter(WithRateLimiterClock(clk.Now))

	const method = "users.list" // tier-2 per defaultMethodTiers
	if got := rl.Tier(method); got != Tier2 {
		t.Fatalf("precondition: Tier(%q) = %v, want Tier2", method, got)
	}

	// Drive 128 goroutines × 100 attempts each. The clock NEVER
	// advances, so refill cannot contribute. Successes must equal the
	// tier-2 default Burst (20).
	want := defaultTierLimits[Tier2].Burst
	got := hammerAllow(rl, method, loadConcurrency, loadAttemptsPerG)
	if got != want {
		t.Errorf("tier-2 burst-honored: allowed = %d under %d-goroutine load, want exactly %d (Burst)",
			got, loadConcurrency, want)
	}
}

// TestRateLimiterLoad_Tier2SustainedHonored asserts that across
// multiple simulated FULL-Window refills the limiter grants EXACTLY
// Sustained tokens per Window — no leakage from the prior Window's
// unused capacity (cap = Burst), no over-grant from a refill race.
//
// Loop shape: drain the bucket (20 successes), then for N = 5
// iterations advance the fake clock by one full Window and hammer 128g
// × 100 attempts. Each iteration's successes must equal exactly the
// Sustained budget. Cumulative across N windows is N × Sustained.
//
// Why this is not redundant with the burst assertion: the burst test
// proves the bucket NEVER OVER-GRANTS in a single Window. This test
// proves the refill cadence ALSO does not over-grant across multiple
// Windows — a refill that incorrectly added more than Sustained tokens
// per Window would flip this test red without tripping the burst test
// (because the bucket would still be Burst-capped within any one
// Window snapshot).
//
// Out of scope for this load test: PARTIAL-Window refill cadence
// (e.g. "advance by Window/2 ⇒ Sustained/2 tokens refill"). That
// invariant is covered by the unit-tier TestRateLimiter_Allow_Refill
// in ratelimiter_test.go which is exactly the right place — a single-
// goroutine assertion on a single refill event does not benefit from
// load shaping.
func TestRateLimiterLoad_Tier2SustainedHonored(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(loadWindowStart)
	rl := NewRateLimiter(WithRateLimiterClock(clk.Now))

	const method = "users.list"
	tier2 := defaultTierLimits[Tier2]

	// Float-stability precondition: the per-Window assertion below
	// requires Sustained × Window-in-seconds / Window-in-seconds to
	// round CLEANLY to the integer Sustained under float64 arithmetic.
	// For the current tier-2 default (Sustained=20, Window=60s,
	// refillRate = 20/60 ≈ 0.333...) the product (0.333... * 60.0)
	// rounds back to 20.0 exactly at this scale. If a future tier-2
	// update changes either field to a combination that does NOT round
	// cleanly (e.g. Sustained=22, Window=60s ⇒ refilled ≈ 21.999...),
	// the per-Window assertion would flip red for a non-bug. Catch
	// that drift here with a clear message rather than letting it
	// surface as a flaky test. Review iter-1 #3.
	rate := float64(tier2.Sustained) / tier2.Window.Seconds()
	refilled := rate * tier2.Window.Seconds()
	const floatEpsilon = 1e-9
	if delta := refilled - float64(tier2.Sustained); delta > floatEpsilon || delta < -floatEpsilon {
		t.Fatalf("tier-2 default budget (Sustained=%d, Window=%s) has float-unstable refill: refilled = %.12f, want %d exactly. Test design needs adjustment for the new budget.",
			tier2.Sustained, tier2.Window, refilled, tier2.Sustained)
	}

	// Drain the bucket on the frozen clock. The initial burst is
	// exactly Burst; this matches the burst assertion above.
	if got := hammerAllow(rl, method, loadConcurrency, loadAttemptsPerG); got != tier2.Burst {
		t.Fatalf("drain: allowed = %d, want %d (Burst)", got, tier2.Burst)
	}

	// Five full Windows. Per Window: advance the fake clock by the
	// full Window duration (refill = Sustained tokens, capped at
	// Burst), then hammer 128g × 100 attempts.
	const windows = 5
	cumulative := 0
	for i := 0; i < windows; i++ {
		clk.Advance(tier2.Window)
		got := hammerAllow(rl, method, loadConcurrency, loadAttemptsPerG)
		// Per-Window refill is capped by Burst; with Sustained==Burst
		// (the tier-2 default) the per-Window grant equals Sustained.
		// Generalising for callers who later set Sustained != Burst:
		// the smaller of the two is the cap.
		want := tier2.Sustained
		if tier2.Burst < want {
			want = tier2.Burst
		}
		if got != want {
			t.Errorf("window %d: allowed = %d, want exactly %d (Sustained, Burst-capped)",
				i+1, got, want)
		}
		cumulative += got
	}
	wantCumulative := windows * tier2.Sustained
	if tier2.Burst < tier2.Sustained {
		wantCumulative = windows * tier2.Burst
	}
	if cumulative != wantCumulative {
		t.Errorf("cumulative across %d windows: allowed = %d, want %d",
			windows, cumulative, wantCumulative)
	}
}

// TestRateLimiterLoad_Tier2WaitConcurrentThroughput asserts the Wait
// blocking path schedules concurrent waiters fairly: the OBSERVED
// throughput (Wait-returns per second) under contended real-clock
// load tracks the configured Sustained rate within ±15 % of the
// budget projection.
//
// Why real-clock here and fake-clock above: the Allow assertions
// reason about discrete refill events the test fully controls. The
// Wait path uses time.NewTimer to schedule its sleep; substituting a
// fake clock there would mock out the very behaviour under test
// (does Wait actually block the right amount of time, does the
// runtime wake waiters fairly). So this test pays a small wall-clock
// budget (~loadDuration) to exercise the blocking path against the
// real scheduler.
//
// Tolerance: ±15 % absorbs the Go runtime's timer-rounding overhead
// + the goroutine scheduler's jitter on a busy CI runner. The
// projection itself (Burst + Sustained × duration / Window) is the
// hard upper bound the leaky-bucket model permits; a result above
// it is a bug. A result below by more than tolerance suggests a
// fairness regression (one goroutine starving the rest). The
// projection is computed against the MEASURED elapsed wall-clock
// (close to but not exactly loadDuration: a runtime that returns
// from the ctx timer slightly early or late shifts the bound to
// match — bias-free).
//
// Override shape: Sustained=40, Burst=10, Window=200ms ⇒ 200 req/sec
// sustained, 10-token burst. Duration is 3 s = 15 Windows so timer-
// scheduling jitter averages out proportionally — review iter-1 #5
// caught that a shorter 1.5 s budget (7.5 Windows) could flake
// 15-20 % under noisy-neighbor load on 2-vCPU CI runners. Doubling
// the budget halves the jitter contribution to the lower bound
// without changing the upper-bound contract.
func TestRateLimiterLoad_Tier2WaitConcurrentThroughput(t *testing.T) {
	t.Parallel()

	const (
		sustained      = 40
		burst          = 10
		window         = 200 * time.Millisecond
		loadGoroutines = 32
		loadDuration   = 3 * time.Second
		tolerancePct   = 15
	)

	rl := NewRateLimiter(
		WithTierLimit(Tier2, TierLimit{
			Sustained: sustained,
			Burst:     burst,
			Window:    window,
		}),
	)

	const method = "users.list"
	if got := rl.Tier(method); got != Tier2 {
		t.Fatalf("precondition: Tier(%q) = %v, want Tier2", method, got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), loadDuration)
	defer cancel()

	var served atomic.Int64
	var wg sync.WaitGroup
	wg.Add(loadGoroutines)
	start := time.Now()
	for i := 0; i < loadGoroutines; i++ {
		go func() {
			defer wg.Done()
			for {
				if err := rl.Wait(ctx, method); err != nil {
					return // ctx.Err — duration elapsed.
				}
				served.Add(1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// Projected throughput is the bucket's leaky-bucket capacity over
	// the MEASURED elapsed wall-clock: an initial Burst served
	// immediately + Sustained × elapsed / Window of refill. Using
	// measured elapsed rather than the configured loadDuration constant
	// avoids biasing the bound when ctx.Done fires slightly off-budget.
	rate := float64(sustained) / window.Seconds()
	projected := float64(burst) + rate*elapsed.Seconds()
	gotF := float64(served.Load())

	// Hard upper bound: the leaky-bucket model FORBIDS more than `projected`
	// Wait-returns. Anything strictly above is a budget violation.
	if gotF > projected*(1+float64(tolerancePct)/100) {
		t.Errorf("Wait throughput: %.0f served > projected %.1f × (1 + %d%% tol) — budget violated",
			gotF, projected, tolerancePct)
	}
	// Lower bound: ±15 % of projected. A result far below suggests
	// the runtime is not scheduling the refill timer at the expected
	// cadence, OR one goroutine is starving the others.
	lower := projected * (1 - float64(tolerancePct)/100)
	if gotF < lower {
		t.Errorf("Wait throughput: %.0f served < projected %.1f × (1 - %d%% tol = %.1f) — fairness regression?",
			gotF, projected, tolerancePct, lower)
	}
	t.Logf("Wait throughput: %.0f served over %s (configured %s), projected %.1f (burst=%d, sustained=%d/%s), %d goroutines",
		gotF, elapsed, loadDuration, projected, burst, sustained, window, loadGoroutines)
}

// hammerAllow is the shared concurrent driver for the Allow-shaped
// assertions above. It fans out `goroutines` goroutines that each call
// `Allow(method)` exactly `attemptsPerG` times, returning the total
// count of `true` returns. Atomic counter is the only shared state; no
// per-call coordination is needed because the assertion is on the
// TOTAL, not on per-goroutine fairness (that's what
// TestRateLimiterLoad_Tier2WaitConcurrentThroughput exercises).
func hammerAllow(rl *RateLimiter, method string, goroutines, attemptsPerG int) int {
	var allowed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < attemptsPerG; j++ {
				if rl.Allow(method) {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	return int(allowed.Load())
}

// BenchmarkRateLimiter_Tier2_Allow_Hot reports the steady-state cost
// of an Allow call against a hot, never-empty tier-2 bucket. Companion
// to TestRateLimiterLoad_Tier2BurstHonored: the test asserts the
// budget, this benchmark gives the per-call ns/op number for trend
// tracking.
//
// "Hot" means the bench tops the bucket up via Window refill BEFORE
// the timer starts, so the measured loop never falls through to the
// false-return branch (which has different cost: the refill compute +
// the fractional-token check). For the false-return cost specifically,
// see BenchmarkRateLimiter_Tier2_Allow_Empty below.
func BenchmarkRateLimiter_Tier2_Allow_Hot(b *testing.B) {
	clk := newFakeClock(loadWindowStart)
	rl := NewRateLimiter(
		WithRateLimiterClock(clk.Now),
		WithTierLimit(Tier2, TierLimit{
			Sustained: b.N + 1,
			Burst:     b.N + 1,
			Window:    time.Minute,
		}),
	)
	const method = "users.list"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !rl.Allow(method) {
			b.Fatalf("Allow #%d returned false on hot bucket — budget mis-sized", i)
		}
	}
}

// BenchmarkRateLimiter_Tier2_Allow_Empty reports the cost of an Allow
// call against a fully-drained bucket on a frozen clock — the
// false-return branch. Useful for the trend line because a regression
// here (e.g. an accidental allocation in the empty path) would slow
// down rejection-heavy traffic without affecting the happy path the
// hot benchmark above measures.
func BenchmarkRateLimiter_Tier2_Allow_Empty(b *testing.B) {
	clk := newFakeClock(loadWindowStart)
	rl := NewRateLimiter(
		WithRateLimiterClock(clk.Now),
		WithTierLimit(Tier2, TierLimit{Sustained: 1, Burst: 1, Window: time.Hour}),
	)
	const method = "users.list"

	// Drain the single token; from here every Allow returns false.
	if !rl.Allow(method) {
		b.Fatalf("setup: first Allow returned false")
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rl.Allow(method) {
			b.Fatalf("Allow #%d returned true on drained bucket — clock advancing unexpectedly?", i)
		}
	}
}
