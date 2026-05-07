package llm

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

// FakeEmbeddingProvider is an in-process [EmbeddingProvider] for tests.
// It produces deterministic vectors without any network I/O: the vector is
// derived from the SHA-256 hash of the query, chained until 1536 float32
// values are filled. Distinct queries produce distinct vectors (SHA-256
// collision resistance). Empty queries are handled identically to non-empty
// ones — SHA-256 of "" is a valid 32-byte digest.
//
// FakeEmbeddingProvider is exported so packages that consume
// [EmbeddingProvider] (e.g. M5.5.c.d.b) can import the fake in their own
// test suites without pulling in test-only helpers.
//
// Options:
//   - [WithEmbedFunc] replaces the entire Embed implementation.
//   - [WithEmbedError] causes every Embed call to return the configured error.
type FakeEmbeddingProvider struct {
	embedFunc  func(ctx context.Context, query string) ([]float32, error)
	embedError error
}

// FakeEmbeddingProviderOption configures a [FakeEmbeddingProvider].
type FakeEmbeddingProviderOption func(*FakeEmbeddingProvider)

// WithEmbedFunc replaces the default hash-fill implementation with fn.
// fn is called after the context check, so context cancellation is still
// honoured even when a custom function is installed.
func WithEmbedFunc(fn func(ctx context.Context, query string) ([]float32, error)) FakeEmbeddingProviderOption {
	return func(f *FakeEmbeddingProvider) {
		f.embedFunc = fn
	}
}

// WithEmbedError causes every [FakeEmbeddingProvider.Embed] call to return
// err after the context check. Useful for testing error-propagation paths in
// callers. Takes precedence over [WithEmbedFunc].
func WithEmbedError(err error) FakeEmbeddingProviderOption {
	return func(f *FakeEmbeddingProvider) {
		f.embedError = err
	}
}

// NewFakeEmbeddingProvider constructs a [FakeEmbeddingProvider] with zero-arg
// defaults. The returned fake is safe for concurrent use.
func NewFakeEmbeddingProvider(opts ...FakeEmbeddingProviderOption) *FakeEmbeddingProvider {
	f := &FakeEmbeddingProvider{}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Compile-time assertion: *FakeEmbeddingProvider satisfies [EmbeddingProvider].
var _ EmbeddingProvider = (*FakeEmbeddingProvider)(nil)

// Embed implements [EmbeddingProvider]. It honours ctx cancellation first,
// then returns the configured error (if any), then delegates to the custom
// function (if set), and finally falls back to the deterministic hash-fill.
func (f *FakeEmbeddingProvider) Embed(ctx context.Context, query string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.embedError != nil {
		return nil, f.embedError
	}
	if f.embedFunc != nil {
		return f.embedFunc(ctx, query)
	}
	return hashFillEmbedding(query), nil
}

// hashFillEmbedding produces a deterministic []float32 of length
// notebook.EmbeddingDim by chaining SHA-256 digests of the query until enough
// bytes are accumulated, then converts each 4-byte group to a float32 via
// math.Float32frombits. The resulting values span the full float32 range but
// are otherwise arbitrary — tests assert determinism and length, not numeric
// properties.
func hashFillEmbedding(query string) []float32 {
	const dim = notebook.EmbeddingDim
	const bytesNeeded = dim * 4
	const digestSize = sha256.Size
	const rounds = (bytesNeeded + digestSize - 1) / digestSize

	buf := make([]byte, 0, rounds*digestSize)

	// Chain: h0 = SHA-256(query), h1 = SHA-256(h0), h2 = SHA-256(h1), ...
	seed := sha256.Sum256([]byte(query))
	buf = append(buf, seed[:]...)
	prev := seed
	for len(buf) < bytesNeeded {
		next := sha256.Sum256(prev[:])
		buf = append(buf, next[:]...)
		prev = next
	}

	out := make([]float32, dim)
	for i := range out {
		bits := binary.LittleEndian.Uint32(buf[i*4 : i*4+4])
		// Mask out NaN/Inf exponent patterns: use only the mantissa + one
		// sign bit so all values are finite (exponent field forced to a
		// normal range: 0x3F800000 = 1.0 base, mantissa adds [0, 1) offset).
		mantissa := bits & 0x007FFFFF
		out[i] = math.Float32frombits(0x3F800000 | mantissa) // in [1.0, 2.0)
	}
	return out
}
