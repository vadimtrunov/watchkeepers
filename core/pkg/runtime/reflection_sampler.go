package runtime

import (
	"hash/fnv"
	"strconv"
	"sync/atomic"
)

// DefaultSuccessSampleRate is the default 1-in-N rate the
// [ToolSuccessReflector] uses to decide whether a successful tool
// invocation should produce an `observation` reflection entry. Pinned
// at 50 per the M7.2 acceptance criteria: simulating 100 successful
// tool calls at rate 1-in-50 should yield approximately two
// reflections.
//
// The rate is the denominator of a 1-in-N decision — N=50 means
// roughly 1 reflection per 50 successful calls on average. A value of
// 0 disables sampling (no reflection written); a value of 1 reflects
// every call. Negative values are treated as 0.
const DefaultSuccessSampleRate = 50

// MetadataKeyToolCallID is the [ToolCall.Metadata] key the
// [ToolSuccessReflector]'s sampler hashes into its 1-in-N decision so
// retries of the same call (same agent + tool + id) do NOT re-sample
// — a retry that lost on the first attempt also loses on the next,
// and a retry that won on the first attempt also wins.
//
// Callers SHOULD populate this key with a stable per-invocation
// identifier (e.g. the upstream Watchmaster's tool-call id, or a
// UUID minted at dispatch time). When the key is absent the sampler
// falls back to hashing just (agentID + toolName) which means
// successive calls to the same tool from the same agent within a
// single run hash to the same bucket — sufficient for the
// determinism contract but coarser than a per-call id.
const MetadataKeyToolCallID = "tool_call_id"

// ReflectionSampler decides whether a single (agentID, toolName,
// toolCallID) tuple should produce a reflection. Implementations MUST
// be deterministic for the contract: the same tuple MUST always yield
// the same bool so a retried call does not re-sample. Implementations
// MUST be safe for concurrent use.
//
// The interface is a function-style single-method contract so callers
// can pass a closure (`SamplerFunc`) for ad-hoc deterministic sampling
// in tests without standing up a struct.
type ReflectionSampler interface {
	// Sample returns true when the tuple should produce a reflection
	// entry. Implementations MUST NOT touch I/O — the call site
	// invokes Sample on every tool success and the cost must be
	// negligible.
	Sample(agentID, toolName, toolCallID string) bool
}

// SamplerFunc is a function adapter that satisfies [ReflectionSampler].
// Useful in tests where a closure captures a counter or a fixed
// predicate without standing up a struct.
type SamplerFunc func(agentID, toolName, toolCallID string) bool

// Sample dispatches to the underlying function.
func (f SamplerFunc) Sample(agentID, toolName, toolCallID string) bool {
	if f == nil {
		return false
	}
	return f(agentID, toolName, toolCallID)
}

// DeterministicSampler is the production [ReflectionSampler]: it
// applies a deterministic 1-in-N gate keyed by an FNV-64 hash over
// (agentID, toolName, toolCallID). The same triple always hashes to
// the same bucket modulo `rate` so a retried call cannot re-sample.
//
// Why FNV-64: cheap (no allocations per call), good distribution
// across short string keys, available in the standard library. The
// sampler is invoked on every tool success — a cryptographic hash
// would dominate the dispatch cost without buying any property the
// 1-in-50 sample needs.
//
// Concurrency: safe for concurrent use. The struct holds an
// immutable rate and an atomic counter used as fallback entropy
// when a caller omits the per-call id ([MetadataKeyToolCallID]).
// The counter advances on every Sample call where callID is empty
// so the M7.2 1-in-50 rate is preserved even for callers that do
// not populate the metadata key; the trade-off is that empty-id
// callers lose the retry-immunity contract documented on the
// interface. See [DeterministicSampler.Sample] for details.
type DeterministicSampler struct {
	rate int
	// emptyIDCounter is incremented per [Sample] call whose
	// toolCallID is empty; the post-increment value is folded
	// into the hash so each such call lands in a fresh bucket
	// and the configured rate is preserved (M7.2 iter-1 review
	// finding #1). Atomic so concurrent dispatch (the wiring's
	// 16-goroutine test in the M5.6.b harness shape) is race-
	// free without locking.
	emptyIDCounter atomic.Uint64
}

// NewDeterministicSampler constructs a [DeterministicSampler] with
// the supplied 1-in-N rate. A rate of 0 (or negative) is normalised
// to 0 — Sample always returns false (no reflection written), which
// is the documented "disabled" semantics. A rate of 1 makes every
// call a reflection.
func NewDeterministicSampler(rate int) *DeterministicSampler {
	if rate < 0 {
		rate = 0
	}
	return &DeterministicSampler{rate: rate}
}

// Sample returns true when the FNV-64 hash of
// (agentID + "\x00" + toolName + "\x00" + toolCallID) modulo `rate`
// equals 0. The `\x00` separators prevent ambiguous key collisions
// across the three fields (e.g. agentID="a", toolName="bc" vs
// agentID="ab", toolName="c"). When rate is 0 the method short-
// circuits to false — sampling is disabled. When rate is 1 every
// call samples without touching FNV.
//
// Empty-toolCallID fallback (M7.2 iter-1 review finding #1): when
// `toolCallID == ""` the sampler appends a per-instance atomic
// counter (post-increment) to the hash input so each successive
// call lands in a fresh bucket. This preserves the configured rate
// (a tool/agent pair without a per-call id still samples ≈1-in-N)
// at the cost of the retry-immunity contract: a retry of the same
// empty-id call hashes to a NEW counter slot and may flip its
// sample decision. Callers who need retry-immunity MUST populate
// [MetadataKeyToolCallID].
//
// The method allocates a single FNV-64 hasher per call (no internal
// buffer); benchmarks at the M7.2 cycle show <100 ns per call on
// commodity hardware, negligible against tool-invocation cost.
func (s *DeterministicSampler) Sample(agentID, toolName, toolCallID string) bool {
	if s.rate <= 0 {
		return false
	}
	if s.rate == 1 {
		return true
	}
	h := fnv.New64a()
	// Write returns no error for fnv.New64a; ignore explicitly.
	_, _ = h.Write([]byte(agentID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(toolName))
	_, _ = h.Write([]byte{0})
	if toolCallID == "" {
		// Empty-id fallback: advance the per-sampler counter and
		// fold its decimal representation into the hash input.
		// strconv.AppendUint avoids the allocation of strconv.FormatUint.
		n := s.emptyIDCounter.Add(1)
		var buf [20]byte // max uint64 base-10 digits
		_, _ = h.Write(strconv.AppendUint(buf[:0], n, 10))
	} else {
		_, _ = h.Write([]byte(toolCallID))
	}
	return h.Sum64()%uint64(s.rate) == 0
}

// Rate returns the configured 1-in-N rate; 0 means sampling is
// disabled. Exposed for diagnostic / observability use; the gate
// decision is the Sample method.
func (s *DeterministicSampler) Rate() int {
	return s.rate
}
