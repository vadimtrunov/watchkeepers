package llm

import (
	"context"
)

// EmbeddingProvider converts a natural-language query string into a dense
// float32 vector suitable for cosine-similarity search against the notebook
// entry corpus.
//
// The returned slice MUST have length equal to
// [github.com/vadimtrunov/watchkeepers/core/pkg/notebook.EmbeddingDim] (1536)
// so the vector can be assigned directly to
// [github.com/vadimtrunov/watchkeepers/core/pkg/notebook.RecallQuery.Embedding]
// and passed to [github.com/vadimtrunov/watchkeepers/core/pkg/notebook.DB.Recall]
// without triggering [github.com/vadimtrunov/watchkeepers/core/pkg/notebook.ErrInvalidEntry].
//
// The concrete HTTP-backed implementation (Anthropic text-embedding-3-small or
// equivalent) lands in M5.5.c.d.b. This interface is the seam that lets the
// notebook recall path (M5.5.c) depend on an abstraction rather than a
// concrete provider.
//
// Implementations MUST honour context cancellation: if ctx is done when Embed
// is called, or becomes done during a blocking network call, Embed MUST return
// ctx.Err() promptly.
type EmbeddingProvider interface {
	// Embed converts query into a dense embedding vector of length
	// notebook.EmbeddingDim (1536). The returned slice is a fresh
	// allocation; callers may mutate it freely. Returns a non-nil error
	// when the provider is unavailable, the context is cancelled, or the
	// input fails provider-side validation.
	Embed(ctx context.Context, query string) ([]float32, error)
}
