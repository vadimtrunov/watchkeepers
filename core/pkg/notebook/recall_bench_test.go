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
//	    -run=TestRecallLatencyP99Under1ms ./core/pkg/notebook/...
//
// Implements ROADMAP-phase1.md §M2b verification bullet 216:
// "Recall latency stays sub-millisecond at 10k entries (benchmark gated)".

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
// a const so both `BenchmarkRecallAt10k` and `TestRecallLatencyP99Under1ms`
// agree on the seed size; changing it here changes the bullet's contract.
const benchEntryCount = 10_000

// benchTopK is the K used for the latency measurement. 10 mirrors a realistic
// agent-side recall (top-10 nearest entries) without giving the budget an
// artificially small workload to chew through.
const benchTopK = 10

// benchSamples is the number of Recall invocations whose latencies we
// collect for the p99 assertion. 1000 samples × ~hundreds of microseconds
// each keeps the test under a couple of seconds while giving the p99
// statistic enough resolution (the 990th-of-1000 sample is the p99).
const benchSamples = 1000

// benchSeed pins the RNG so the benchmark is reproducible across runs and
// machines. The exact value is arbitrary; the requirement is determinism so a
// regression that perturbs the seed corpus is not blamed on RNG noise.
const benchSeed uint64 = 0xCAFEF00DBADDCAFE

// recallP99Budget is the bullet's hard ceiling. p99 over `benchSamples`
// Recall calls against a 10k-entry corpus must come in below this. The
// statistic choice (p99 over 1000 samples, NOT mean) is intentional: mean
// hides tail spikes, and the bullet is about predictable per-turn latency.
const recallP99Budget = time.Millisecond

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
// the open *DB and a deterministic query vector drawn from the same RNG so
// the corpus and query are correlated (else random-vs-random would
// degenerate to nearly-uniform distances and starve the index of work).
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

	// Query vector drawn from the same RNG keeps the workload reproducible.
	// Using an entry vector verbatim would give distance 0 on the top hit
	// and starve the inner loop; a fresh draw forces the index to actually
	// rank all 10k rows.
	query := randomEmbedding(rng)
	return db, ctx, query
}

// TestRecallLatencyP99Under1ms is the asserting half of bullet 216. It runs
// `benchSamples` Recall calls against a fresh 10k-entry corpus and fails if
// the p99 latency exceeds [recallP99Budget].
//
// Why a *_test* (not just a Benchmark*): bench output is informational and
// never fails CI on regression. A proper test asserts the budget so a future
// change that 5x's recall latency at 10k entries trips the build. Default
// `go test ./...` is unaffected because the file is gated by
// `//go:build benchmark`.
func TestRecallLatencyP99Under1ms(t *testing.T) {
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
	p50 := samples[len(samples)*50/100]
	p99 := samples[len(samples)*99/100]
	maxLat := samples[len(samples)-1]

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
// 10k-entry corpus size. Companion to [TestRecallLatencyP99Under1ms]: the
// test asserts the budget; this benchmark gives raw numbers for trend
// tracking. Same seed corpus and same query vector for apples-to-apples
// comparison across runs.
func BenchmarkRecallAt10k(b *testing.B) {
	db, ctx, query := seedBenchDB(b)

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
