package llm_test

import (
	"context"
	"errors"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

// TestFakeEmbeddingProvider_VectorLength_MatchesEmbeddingDim verifies that
// FakeEmbeddingProvider returns a vector of exactly notebook.EmbeddingDim
// elements, making the fake usable as input to notebook.DB.Recall without
// ErrInvalidEntry.
func TestFakeEmbeddingProvider_VectorLength_MatchesEmbeddingDim(t *testing.T) {
	p := llm.NewFakeEmbeddingProvider()
	vec, err := p.Embed(context.Background(), "test query")
	if err != nil {
		t.Fatalf("Embed: unexpected error: %v", err)
	}
	if got, want := len(vec), notebook.EmbeddingDim; got != want {
		t.Errorf("len(vec) = %d, want %d (notebook.EmbeddingDim)", got, want)
	}
}

// TestFakeEmbeddingProvider_Deterministic verifies that two Embed calls with
// the same query return identical vectors.
func TestFakeEmbeddingProvider_Deterministic(t *testing.T) {
	p := llm.NewFakeEmbeddingProvider()
	const query = "semantic search query"

	v1, err := p.Embed(context.Background(), query)
	if err != nil {
		t.Fatalf("first Embed: %v", err)
	}
	v2, err := p.Embed(context.Background(), query)
	if err != nil {
		t.Fatalf("second Embed: %v", err)
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatalf("v1[%d] = %v, v2[%d] = %v: vectors not deterministic", i, v1[i], i, v2[i])
		}
	}
}

// TestFakeEmbeddingProvider_DistinctQueries_DistinctVectors verifies that two
// different queries produce different vectors (collision resistance of the
// underlying hash).
func TestFakeEmbeddingProvider_DistinctQueries_DistinctVectors(t *testing.T) {
	p := llm.NewFakeEmbeddingProvider()

	v1, err := p.Embed(context.Background(), "apple")
	if err != nil {
		t.Fatalf("Embed apple: %v", err)
	}
	v2, err := p.Embed(context.Background(), "orange")
	if err != nil {
		t.Fatalf("Embed orange: %v", err)
	}
	allEqual := true
	for i := range v1 {
		if v1[i] != v2[i] {
			allEqual = false
			break
		}
	}
	if allEqual {
		t.Error("distinct queries produced identical vectors")
	}
}

// TestFakeEmbeddingProvider_EmptyQuery_StillReturnsValidVector verifies that
// Embed("") does not special-case the empty string and still returns a valid
// notebook.EmbeddingDim-length vector.
func TestFakeEmbeddingProvider_EmptyQuery_StillReturnsValidVector(t *testing.T) {
	p := llm.NewFakeEmbeddingProvider()
	vec, err := p.Embed(context.Background(), "")
	if err != nil {
		t.Fatalf("Embed empty: %v", err)
	}
	if got, want := len(vec), notebook.EmbeddingDim; got != want {
		t.Errorf("len(vec) = %d, want %d", got, want)
	}
}

// TestFakeEmbeddingProvider_WithEmbedError_PropagatesError verifies that the
// WithEmbedError option causes Embed to return the configured error.
func TestFakeEmbeddingProvider_WithEmbedError_PropagatesError(t *testing.T) {
	sentinel := errors.New("embed: boom")
	p := llm.NewFakeEmbeddingProvider(llm.WithEmbedError(sentinel))

	_, err := p.Embed(context.Background(), "any query")
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, sentinel) = false, got %v", err)
	}
}

// TestFakeEmbeddingProvider_WithEmbedFunc_UsesCustomFunction verifies that the
// WithEmbedFunc option replaces the default hash-fill logic with a caller-
// supplied function.
func TestFakeEmbeddingProvider_WithEmbedFunc_UsesCustomFunction(t *testing.T) {
	customVec := make([]float32, notebook.EmbeddingDim)
	for i := range customVec {
		customVec[i] = float32(i) / float32(notebook.EmbeddingDim)
	}

	p := llm.NewFakeEmbeddingProvider(llm.WithEmbedFunc(func(_ context.Context, _ string) ([]float32, error) {
		return customVec, nil
	}))

	got, err := p.Embed(context.Background(), "irrelevant")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for i := range customVec {
		if got[i] != customVec[i] {
			t.Fatalf("got[%d] = %v, want %v: custom func not used", i, got[i], customVec[i])
		}
	}
}

// TestFakeEmbeddingProvider_ContextCancelled_ReturnsCtxErr verifies that Embed
// respects a cancelled context and returns ctx.Err() rather than computing a
// vector.
func TestFakeEmbeddingProvider_ContextCancelled_ReturnsCtxErr(t *testing.T) {
	p := llm.NewFakeEmbeddingProvider()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling Embed

	_, err := p.Embed(ctx, "query")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, got %v", err)
	}
}

// TestFakeEmbeddingProvider_WithEmbedError_TakesPrecedenceOverEmbedFunc verifies
// the documented option-precedence rule: WithEmbedError short-circuits before
// WithEmbedFunc is consulted, so the embed function is never invoked when an
// error is configured. A future refactor that swaps the check order would be
// caught by this test.
func TestFakeEmbeddingProvider_WithEmbedError_TakesPrecedenceOverEmbedFunc(t *testing.T) {
	sentinel := errors.New("embed: precedence sentinel")
	funcInvoked := false

	p := llm.NewFakeEmbeddingProvider(
		llm.WithEmbedFunc(func(_ context.Context, _ string) ([]float32, error) {
			funcInvoked = true
			return nil, nil
		}),
		llm.WithEmbedError(sentinel),
	)

	_, err := p.Embed(context.Background(), "any query")
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, sentinel) = false, got %v", err)
	}
	if funcInvoked {
		t.Error("embed func was invoked despite WithEmbedError being set; precedence rule violated")
	}
}

// TestEmbeddingProvider_RecallQueryCompatibility is a compile-time check that
// the []float32 returned by EmbeddingProvider.Embed is assignment-compatible
// with notebook.RecallQuery.Embedding. No SQLite I/O occurs.
func TestEmbeddingProvider_RecallQueryCompatibility(t *testing.T) {
	p := llm.NewFakeEmbeddingProvider()
	vec, err := p.Embed(context.Background(), "recall check")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	// Compile-time + length compatibility: assign the vector to RecallQuery
	// and confirm the length satisfies notebook.EmbeddingDim.
	var rq notebook.RecallQuery
	rq.Embedding = vec
	_ = rq

	if got, want := len(rq.Embedding), notebook.EmbeddingDim; got != want {
		t.Errorf("rq.Embedding length = %d, want %d", got, want)
	}
}
