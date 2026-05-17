package llm

import (
	"fmt"
	"strings"
)

// RecalledMemoryHeader is the section heading the WithRecalledMemory option
// injects into [CompleteRequest.System] / [StreamRequest.System] /
// [CountTokensRequest.System]. Exported so downstream consumers (e.g. the
// turn helper in M5.5.c.d.b.b) can strip or detect the block without
// hard-coding the heading string at every call site.
const RecalledMemoryHeader = "# Recalled memory"

// RecalledMemory is a single recalled-memory entry the LLM layer injects into
// the System prompt. The fields mirror the most relevant columns of
// [notebook.RecallResult]:
//
//   - Subject   ← notebook.RecallResult.Subject   (human-readable label)
//   - Content   ← notebook.RecallResult.Content   (body text)
//   - Score     ← caller-computed relevance score in [0, 1]; float32 to match
//     the precision the notebook's embedding layer produces
//   - Category  ← notebook.RecallResult.Category  (M7.2 weight policy)
//
// RecalledMemory is intentionally a value type so WithRecalledMemory callers
// can pass it from a stack-allocated slice without the allocator round-trip.
type RecalledMemory struct {
	// Subject is the human-readable label for the memory entry. May be empty;
	// the renderer emits an empty label rather than dropping the entry so
	// callers do not need to filter before passing.
	Subject string

	// Content is the textual body of the memory entry. Required in practice
	// (recall filters empty content at the notebook layer) but this package
	// does not validate it.
	Content string

	// Score is the caller-computed relevance score in [0, 1]. Rendered with
	// the %g verb so compact values like 0.75 appear without trailing zeros.
	// The M7.2 category-weight policy ([CategoryAutoInjectionWeights])
	// multiplies the cosine-distance-derived score by a per-category factor
	// at projection time so [BuildTurnRequest]'s
	// [runtime.Manifest.NotebookRelevanceThreshold] filter naturally drops
	// more observation rows than lesson rows when both kinds match the
	// query.
	Score float32

	// Category is the [notebook.RecallResult.Category] passed through
	// verbatim. Empty when the projection caller did not populate it;
	// downstream rendering does not use it but the field is exposed so
	// future tooling (e.g. category-aware highlight in the prompt
	// rendering) can read it without re-querying the notebook.
	Category string
}

// renderRecalledMemoryBlock formats `memories` into the canonical bullet
// block that WithRecalledMemory appends to the System prompt. Returns an
// empty string when `memories` is empty or nil so the callers can use the
// result directly in a conditional.
//
// Format (per AC3):
//
//	# Recalled memory
//	- <Subject>: <Content>  (relevance: <Score>)
//	- ...
func renderRecalledMemoryBlock(memories []RecalledMemory) string {
	if len(memories) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(RecalledMemoryHeader)
	for _, m := range memories {
		// Use %g so 0.75 renders as "0.75", not "0.750000".
		fmt.Fprintf(&b, "\n- %s: %s  (relevance: %g)", m.Subject, m.Content, m.Score)
	}
	return b.String()
}

// applyRecalledMemory appends the rendered recalled-memory block to `system`,
// handling the empty-System edge case (AC3): when system is empty the block
// starts the System string with no leading blank line; when system is
// non-empty two newlines separate the existing content from the block.
//
// When memories is nil or empty the function returns system unchanged (AC4).
func applyRecalledMemory(system string, memories []RecalledMemory) string {
	block := renderRecalledMemoryBlock(memories)
	if block == "" {
		return system
	}
	if system == "" {
		return block
	}
	return system + "\n\n" + block
}

// WithRecalledMemory returns a [RequestOption] that appends a recalled-memory
// block to [requestParams.system] after all prior options have been applied.
// Calling WithRecalledMemory with no arguments or a nil slice is a no-op (AC4).
//
// Slice order equals render order (AC5): no sorting or deduplication is
// performed. The same slice produces byte-identical output across
// [BuildCompleteRequest], [BuildStreamRequest], and [BuildCountTokensRequest]
// (AC6).
//
// Injection format (AC3):
//
//	<existing System>
//
//	# Recalled memory
//	- <Subject>: <Content>  (relevance: <Score>)
//	- ...
//
// When System is empty the block starts the System field directly with no
// leading blank line.
func WithRecalledMemory(memories ...RecalledMemory) RequestOption {
	return func(p *requestParams) {
		p.system = applyRecalledMemory(p.system, memories)
	}
}
