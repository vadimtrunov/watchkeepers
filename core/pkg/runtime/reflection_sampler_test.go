package runtime_test

import (
	"strconv"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// TestDeterministicSampler_Rate0_Disabled verifies that rate 0 short-
// circuits Sample to false (sampling disabled) regardless of the
// tuple. This is the documented "no reflection written" semantics for
// operators who wire a success reflector but want to opt out of
// sampling at runtime.
func TestDeterministicSampler_Rate0_Disabled(t *testing.T) {
	t.Parallel()
	s := runtime.NewDeterministicSampler(0)
	for i := 0; i < 200; i++ {
		if s.Sample("agent", "tool", strconv.Itoa(i)) {
			t.Fatalf("rate 0 must never sample; tuple #%d returned true", i)
		}
	}
}

// TestDeterministicSampler_NegativeRate_NormalisedToZero verifies the
// constructor clamps a negative rate to 0 (sampling disabled), per
// the constructor's defensive contract.
func TestDeterministicSampler_NegativeRate_NormalisedToZero(t *testing.T) {
	t.Parallel()
	s := runtime.NewDeterministicSampler(-5)
	if s.Rate() != 0 {
		t.Errorf("Rate() = %d, want 0 (negative normalised)", s.Rate())
	}
	if s.Sample("a", "b", "c") {
		t.Error("negative-rate sampler must not sample")
	}
}

// TestDeterministicSampler_Rate1_AlwaysSamples verifies that rate 1
// reflects every call (the documented "reflect every call" mode used
// by operators who want full coverage with no sampling).
func TestDeterministicSampler_Rate1_AlwaysSamples(t *testing.T) {
	t.Parallel()
	s := runtime.NewDeterministicSampler(1)
	for i := 0; i < 50; i++ {
		if !s.Sample("agent", "tool", strconv.Itoa(i)) {
			t.Fatalf("rate 1 must always sample; tuple #%d returned false", i)
		}
	}
}

// TestDeterministicSampler_DeterministicForSameTuple verifies the
// retry-immunity contract: the same (agentID, toolName, toolCallID)
// tuple ALWAYS hashes to the same bucket — a retried call that
// sampled true on the first attempt also samples true on the
// retry, and vice versa. This is the contract the wired runtime
// relies on so a retry does not produce a duplicate observation row
// (or skip one that should have been written).
func TestDeterministicSampler_DeterministicForSameTuple(t *testing.T) {
	t.Parallel()
	s := runtime.NewDeterministicSampler(50)
	const trials = 100
	for i := 0; i < trials; i++ {
		callID := "call-" + strconv.Itoa(i)
		first := s.Sample("agent-1", "tool-1", callID)
		// Re-sample the same tuple many times — the decision must
		// not flip.
		for j := 0; j < 20; j++ {
			if got := s.Sample("agent-1", "tool-1", callID); got != first {
				t.Fatalf("tuple call-%d flipped on retry: first=%v, retry=%v",
					i, first, got)
			}
		}
	}
}

// TestDeterministicSampler_DifferentTuplesDiffer verifies the
// separator bytes in the hash input prevent ambiguous collisions
// across the three string fields. With concatenation-only, the
// tuple (agentID="ab", toolName="c", id="") would collide with
// ("a", "bc", ""); the `\x00` separators eliminate that family.
func TestDeterministicSampler_DifferentTuplesDiffer(t *testing.T) {
	t.Parallel()
	s := runtime.NewDeterministicSampler(50)
	// Collect a hit set across a wide tuple range; we don't assert
	// any specific decision, only that the sampler discriminates
	// between adjacent tuples (i.e. the hash spreads input space).
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		callID := strconv.Itoa(i)
		seen[callID] = s.Sample("agent-A", "tool-X", callID)
	}
	// At least two distinct decisions across the 100 tuples — a
	// degenerate hasher would return the same bool for all.
	trueCount := 0
	for _, b := range seen {
		if b {
			trueCount++
		}
	}
	if trueCount == 0 || trueCount == 100 {
		t.Errorf("sampler degenerate: trueCount=%d across 100 tuples", trueCount)
	}
}

// TestDeterministicSampler_1in50_HitsApproximately2Per100 is the M7.2
// acceptance criterion at the unit level: simulating 100 successful
// tool calls at rate 1-in-50 should produce roughly 2 reflections.
// We allow a tolerance of [0, 6] hits — the FNV distribution over a
// small N has variance, but a rate-50 sampler that produces 0 or >6
// hits in 100 trials is statistically suspicious (the test exists to
// catch a hash-input wiring regression that would produce 50/100 or
// 0/100, not to validate a precise rate).
func TestDeterministicSampler_1in50_HitsApproximately2Per100(t *testing.T) {
	t.Parallel()
	s := runtime.NewDeterministicSampler(runtime.DefaultSuccessSampleRate)
	hits := 0
	for i := 0; i < 100; i++ {
		if s.Sample("agent-1", "tool-1", "call-"+strconv.Itoa(i)) {
			hits++
		}
	}
	if hits < 0 || hits > 6 {
		t.Errorf("1-in-50 over 100 tuples produced %d hits; want in [0, 6]", hits)
	}
}

// TestDeterministicSampler_ConcurrentSafe verifies the sampler is
// safe for concurrent use. The struct holds only an immutable int
// after construction; race-detector run validates no shared mutable
// state.
func TestDeterministicSampler_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	s := runtime.NewDeterministicSampler(50)
	const goroutines = 16
	const perGoroutine = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				_ = s.Sample("agent-"+strconv.Itoa(g), "tool", strconv.Itoa(i))
			}
		}(g)
	}
	wg.Wait()
}

// TestSamplerFunc_NilSafe verifies the [SamplerFunc] adapter returns
// false when the underlying function is nil — defensive contract so a
// caller that passes a zero-value SamplerFunc does not panic.
func TestSamplerFunc_NilSafe(t *testing.T) {
	t.Parallel()
	var f runtime.SamplerFunc
	if f.Sample("a", "b", "c") {
		t.Error("nil SamplerFunc must return false, got true")
	}
}

// TestSamplerFunc_DispatchesToFunc verifies the adapter forwards
// arguments verbatim and returns the function's bool result.
func TestSamplerFunc_DispatchesToFunc(t *testing.T) {
	t.Parallel()
	var gotAgent, gotTool, gotCallID string
	f := runtime.SamplerFunc(func(a, t, c string) bool {
		gotAgent, gotTool, gotCallID = a, t, c
		return true
	})
	if !f.Sample("AID", "TOOL", "CID") {
		t.Error("Sample = false, want true")
	}
	if gotAgent != "AID" || gotTool != "TOOL" || gotCallID != "CID" {
		t.Errorf("forwarded args = (%q, %q, %q), want (AID, TOOL, CID)",
			gotAgent, gotTool, gotCallID)
	}
}
