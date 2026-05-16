//go:build benchmark

// This file is gated behind the `benchmark` build tag so the default
// `go test ./...` and `make test` paths NEVER execute the 10k-entry seed.
// Run it with:
//
//	make notebook-bench
//
// or directly:
//
//	go test -tags=benchmark -bench=BenchmarkRecallAt10k \
//	    -run=TestRecallLatencyP99WithinBudget ./core/pkg/notebook/...
//
// Implements ROADMAP-phase1.md §M2b verification bullet 216:
// "Recall p99 latency at 10k entries stays under [recallP99Budget]
// (benchmark gated)." The budget was revised from the original
// sub-millisecond target after empirical measurement showed sqlite-vec's
// brute-force vec0 KNN cannot reach sub-ms at the Phase 1 1536-dim corpus;
// the sub-ms goal moves to Phase 2 M7.5. See [recallP99Budget] for detail.

package notebook

import (
	"context"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// benchEntryCount is the corpus size mandated by ROADMAP bullet 216. Held as
// a const so both `BenchmarkRecallAt10k` and `TestRecallLatencyP99WithinBudget`
// agree on the seed size; changing it here changes the bullet's contract.
const benchEntryCount = 10_000

// benchTopK is the K used for the latency measurement. 10 mirrors a realistic
// agent-side recall (top-10 nearest entries) without giving the budget an
// artificially small workload to chew through.
const benchTopK = 10

// benchSamples is the number of Recall invocations whose latencies we
// collect for the p99 assertion. 1000 samples × tens of milliseconds
// each keeps the test under a couple of minutes while giving the p99
// statistic enough resolution. With 1000 samples, the 990th-smallest
// (0-indexed: samples[989]) is the canonical p99; see the percentile
// computation below for how the index is derived.
const benchSamples = 1000

// benchSeed pins the RNG so the benchmark is reproducible across runs and
// machines. The exact value is arbitrary; the requirement is determinism so a
// regression that perturbs the seed corpus is not blamed on RNG noise.
const benchSeed uint64 = 0xCAFEF00DBADDCAFE

// recallP99Budget is the bullet's hard ceiling. p99 over `benchSamples`
// Recall calls against a 10k-entry corpus must come in below this. The
// statistic choice (p99 over 1000 samples, NOT mean) is intentional: mean
// hides tail spikes, and the bullet is about predictable per-turn latency.
//
// Why 100 ms and not the originally-aspired sub-millisecond: sqlite-vec
// currently ships only brute-force KNN through the `vec0` virtual table
// (HNSW is on their roadmap, not released). With 1536-dim float32 dense
// vectors and 10k rows that is ~15M float ops + L2-norm + sort per query,
// which lands around 25–40 ms p50 on commodity hardware. Empirical
// measurement on dev hardware (Apple M-series) gave p50 ≈ 30 ms,
// p99 ≈ 37 ms across repeated runs, with one observed max around 100 ms
// (a single outlier in 1000 samples — the assertion gates on p99 not max,
// so a one-sample spike near the ceiling does not trip the budget); CI
// runners and containerized environments typically add 1.5–2× overhead.
// The 100 ms ceiling is wide enough to absorb that variability while still
// flagging catastrophic regressions (e.g. a 3× slowdown from a missing
// index or an accidental O(n²) layer). Driving this back toward sub-ms
// is tracked under Phase 2 M7.5 (quantization / backend swap) — see
// docs/ROADMAP-phase2.md.
const recallP99Budget = 100 * time.Millisecond

// randomEmbedding draws a fresh dense vector of length [EmbeddingDim] from
// the supplied RNG. A dense random vector models the worst case for
// sqlite-vec's brute-force cosine KNN much better than the one-hot vectors
// used by the unit tests — the unit-test vectors zero-out 1535 of 1536
// dimensions, so the per-row distance work is artificially small.
func randomEmbedding(r *rand.Rand) []float32 {
	v := make([]float32, EmbeddingDim)
	for i := range v {
		// rand.Float32 returns [0,1); shift to [-1,1) so vectors span the
		// embedding space the way real model outputs do.
		v[i] = r.Float32()*2 - 1
	}
	return v
}

// seedBenchDB opens a fresh per-test Notebook under t.TempDir() and inserts
// `benchEntryCount` distinct entries via the public Remember API. Returns
// the open *DB and a query vector drawn independently from the same
// PCG-seeded RNG stream that produced the corpus — drawing from the shared
// stream gives reproducibility across runs and machines, while the
// independence (a separate fresh draw, not a copy of an entry vector)
// guarantees the brute-force index must rank all 10k rows rather than
// short-circuit on a distance-0 top hit.
//
// Uses the testing.TB interface so it serves both *testing.T (latency
// assertion test) and *testing.B (Go bench harness).
func seedBenchDB(tb testing.TB) (*DB, context.Context, []float32) {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "agent.sqlite")
	ctx, cancel := context.WithCancel(context.Background())
	tb.Cleanup(cancel)
	db, err := openAt(ctx, path)
	if err != nil {
		tb.Fatalf("openAt: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	// math/rand/v2 PCG with a fixed seed: deterministic across Go versions
	// and machines. Using NewChaCha8 would be overkill for non-crypto
	// determinism; PCG is the simpler primitive.
	rng := rand.New(rand.NewPCG(benchSeed, benchSeed^0x9E3779B97F4A7C15))

	for i := 0; i < benchEntryCount; i++ {
		if _, err := db.Remember(ctx, Entry{
			Category:  CategoryLesson,
			Content:   "bench-entry",
			Embedding: randomEmbedding(rng),
		}); err != nil {
			tb.Fatalf("seed Remember %d/%d: %v", i, benchEntryCount, err)
		}
	}

	// Independent fresh draw from the same seeded RNG stream: drawing from
	// the shared stream keeps the workload reproducible across runs, and the
	// freshness (rather than reusing an entry vector verbatim) prevents the
	// index from short-circuiting on a distance-0 top hit and forces it to
	// actually rank all 10k rows.
	query := randomEmbedding(rng)
	return db, ctx, query
}

// TestRecallLatencyP99WithinBudget is the asserting half of bullet 216. It runs
// `benchSamples` Recall calls against a fresh 10k-entry corpus and fails if
// the p99 latency exceeds [recallP99Budget].
//
// Why a *_test* (not just a Benchmark*): bench output is informational and
// never fails on regression. A proper test asserts the budget, so a future
// change that 3–5× recall latency at 10k entries will fail this assertion
// — but ONLY when the bench is actually executed. Because the whole file is
// gated by `//go:build benchmark`, default `go test ./...` (and therefore
// every PR's standard CI matrix) is unaffected; this assertion only trips
// `make notebook-bench`, which operators run manually per the operator
// runbook. Wiring this assertion into a scheduled CI job is tracked
// separately under §10 Phase D (DoD Closure Plan) in ROADMAP-phase1.md.
func TestRecallLatencyP99WithinBudget(t *testing.T) {
	db, ctx, query := seedBenchDB(t)

	// Warm-up: the first Recall pays SQLite plan-cache and OS-cache costs
	// that are not representative of steady-state agent recall. Discard the
	// first few samples so we measure hot-path performance.
	const warmup = 5
	for i := 0; i < warmup; i++ {
		if _, err := db.Recall(ctx, RecallQuery{
			Embedding: query,
			TopK:      benchTopK,
		}); err != nil {
			t.Fatalf("warmup Recall %d: %v", i, err)
		}
	}

	samples := make([]time.Duration, 0, benchSamples)
	for i := 0; i < benchSamples; i++ {
		start := time.Now()
		got, err := db.Recall(ctx, RecallQuery{
			Embedding: query,
			TopK:      benchTopK,
		})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("Recall sample %d: %v", i, err)
		}
		if len(got) != benchTopK {
			t.Fatalf("Recall sample %d: len=%d, want %d", i, len(got), benchTopK)
		}
		samples = append(samples, elapsed)
	}

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	// Canonical percentile-by-rank: for n sorted samples, the p-th
	// percentile (0..100) is samples[(n-1)*p/100]. For n=1000 this puts
	// p50 at index 499 (500th smallest) and p99 at index 989 (990th
	// smallest). Using len(samples)*p/100 instead would land one index
	// higher (samples[1000*99/100] = samples[990] = the 991st sample)
	// which is technically the p99.1.
	n := len(samples)
	p50 := samples[(n-1)*50/100]
	p99 := samples[(n-1)*99/100]
	maxLat := samples[n-1]

	t.Logf("Recall latency over %d samples at %d entries (TopK=%d, EmbeddingDim=%d):",
		benchSamples, benchEntryCount, benchTopK, EmbeddingDim)
	t.Logf("  p50 = %s", p50)
	t.Logf("  p99 = %s", p99)
	t.Logf("  max = %s", maxLat)

	if p99 >= recallP99Budget {
		t.Fatalf("p99 recall latency %s >= budget %s at %d entries",
			p99, recallP99Budget, benchEntryCount)
	}
}

// BenchmarkRecallAt10k reports ns/op for sqlite-vec recall at the
// 10k-entry corpus size. Companion to [TestRecallLatencyP99WithinBudget]: the
// test asserts the budget; this benchmark gives raw numbers for trend
// tracking. Same seed corpus, same query vector, and same warmup-discard
// strategy as the assertion test, so the two report apples-to-apples
// steady-state numbers.
func BenchmarkRecallAt10k(b *testing.B) {
	db, ctx, query := seedBenchDB(b)

	// Mirror the test's warmup discard so the first b.N=1 iteration does
	// not pay SQLite plan-cache + OS-cache costs that the test explicitly
	// excludes. Without this, short bench runs report cold-cache numbers
	// while the test reports hot-path numbers — breaking the
	// apples-to-apples claim above.
	const warmup = 5
	for i := 0; i < warmup; i++ {
		if _, err := db.Recall(ctx, RecallQuery{
			Embedding: query,
			TopK:      benchTopK,
		}); err != nil {
			b.Fatalf("warmup Recall %d: %v", i, err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := db.Recall(ctx, RecallQuery{
			Embedding: query,
			TopK:      benchTopK,
		})
		if err != nil {
			b.Fatalf("Recall iter %d: %v", i, err)
		}
		if len(got) != benchTopK {
			b.Fatalf("Recall iter %d: len=%d, want %d", i, len(got), benchTopK)
		}
	}
}
