package llm

import (
	"testing"
)

// sampleMemories returns a fixed slice of [RecalledMemory] entries used by
// multiple tests. Callers must not mutate the returned slice.
func sampleMemories() []RecalledMemory {
	return []RecalledMemory{
		{Subject: "Go proverbs", Content: "Clear is better than clever.", Score: 0.92},
		{Subject: "Error handling", Content: "Don't just check errors, handle them gracefully.", Score: 0.75},
	}
}

// TestWithRecalledMemory_AppendsToSystem_NonEmptySystem verifies that when
// System is already non-empty the recalled-memory block is separated from the
// existing content by exactly two newlines.
func TestWithRecalledMemory_AppendsToSystem_NonEmptySystem(t *testing.T) {
	t.Parallel()

	m := validManifest() // System = "You are a test agent."
	msgs := validMessages()

	req, err := BuildCompleteRequest(m, msgs, WithRecalledMemory(sampleMemories()...))
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}

	want := "You are a test agent.\n\n# Recalled memory\n- Go proverbs: Clear is better than clever.  (relevance: 0.92)\n- Error handling: Don't just check errors, handle them gracefully.  (relevance: 0.75)"
	if req.System != want {
		t.Errorf("System =\n%q\nwant\n%q", req.System, want)
	}
}

// TestWithRecalledMemory_StartsSystem_EmptySystem verifies that when System is
// empty the rendered block starts the System field with no leading blank line.
func TestWithRecalledMemory_StartsSystem_EmptySystem(t *testing.T) {
	t.Parallel()

	m := validManifest()
	m.SystemPrompt = "placeholder" // must be non-empty for composeBaseFields

	// We override System via a custom option applied BEFORE WithRecalledMemory
	// so that requestParams.system is empty when WithRecalledMemory runs.
	// Use a raw RequestOption to set system to "".
	clearSystem := RequestOption(func(p *requestParams) { p.system = "" })

	msgs := validMessages()
	req, err := BuildCompleteRequest(m, msgs, clearSystem, WithRecalledMemory(sampleMemories()...))
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}

	want := "# Recalled memory\n- Go proverbs: Clear is better than clever.  (relevance: 0.92)\n- Error handling: Don't just check errors, handle them gracefully.  (relevance: 0.75)"
	if req.System != want {
		t.Errorf("System =\n%q\nwant\n%q", req.System, want)
	}
}

// TestWithRecalledMemory_BulletFormat pins the exact bullet format including
// the double-space before the relevance annotation and %g score formatting.
func TestWithRecalledMemory_BulletFormat(t *testing.T) {
	t.Parallel()

	mem := []RecalledMemory{
		{Subject: "Topic", Content: "Body text.", Score: 0.75},
	}
	got := renderRecalledMemoryBlock(mem)
	want := "# Recalled memory\n- Topic: Body text.  (relevance: 0.75)"
	if got != want {
		t.Errorf("renderRecalledMemoryBlock =\n%q\nwant\n%q", got, want)
	}
}

// TestWithRecalledMemory_PreservesOrder verifies that the render order equals
// the slice order — no sorting or deduplication occurs (AC5).
func TestWithRecalledMemory_PreservesOrder(t *testing.T) {
	t.Parallel()

	mem := []RecalledMemory{
		{Subject: "Z", Content: "last", Score: 0.1},
		{Subject: "A", Content: "first", Score: 0.9},
		{Subject: "M", Content: "middle", Score: 0.5},
	}
	got := renderRecalledMemoryBlock(mem)
	want := "# Recalled memory\n- Z: last  (relevance: 0.1)\n- A: first  (relevance: 0.9)\n- M: middle  (relevance: 0.5)"
	if got != want {
		t.Errorf("renderRecalledMemoryBlock =\n%q\nwant\n%q", got, want)
	}
}

// TestWithRecalledMemory_EmptySlice_NoOp verifies that WithRecalledMemory()
// (zero arguments) leaves System unchanged (AC4).
func TestWithRecalledMemory_EmptySlice_NoOp(t *testing.T) {
	t.Parallel()

	m := validManifest()
	msgs := validMessages()

	req, err := BuildCompleteRequest(m, msgs, WithRecalledMemory())
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if req.System != m.SystemPrompt {
		t.Errorf("System = %q, want %q (unchanged)", req.System, m.SystemPrompt)
	}
}

// TestWithRecalledMemory_NilSlice_NoOp verifies that WithRecalledMemory with
// an explicit nil-typed slice leaves System unchanged (AC4).
func TestWithRecalledMemory_NilSlice_NoOp(t *testing.T) {
	t.Parallel()

	m := validManifest()
	msgs := validMessages()

	var nilMems []RecalledMemory
	req, err := BuildCompleteRequest(m, msgs, WithRecalledMemory(nilMems...))
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if req.System != m.SystemPrompt {
		t.Errorf("System = %q, want %q (unchanged)", req.System, m.SystemPrompt)
	}
}

// TestWithRecalledMemory_EmptySubject_StillRendered verifies that a
// RecalledMemory with an empty Subject is still rendered (the bullet appears
// with an empty label) rather than being silently dropped.
func TestWithRecalledMemory_EmptySubject_StillRendered(t *testing.T) {
	t.Parallel()

	mem := []RecalledMemory{
		{Subject: "", Content: "Anonymous memory.", Score: 0.5},
	}
	got := renderRecalledMemoryBlock(mem)
	want := "# Recalled memory\n- : Anonymous memory.  (relevance: 0.5)"
	if got != want {
		t.Errorf("renderRecalledMemoryBlock =\n%q\nwant\n%q", got, want)
	}
}

// TestWithRecalledMemory_ParityAcrossBuilders verifies that identical inputs
// produce byte-identical System fields across all three builders (AC6).
func TestWithRecalledMemory_ParityAcrossBuilders(t *testing.T) {
	t.Parallel()

	m := validManifest()
	msgs := validMessages()
	opt := WithRecalledMemory(sampleMemories()...)

	complete, err := BuildCompleteRequest(m, msgs, opt)
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	stream, err := BuildStreamRequest(m, msgs, opt)
	if err != nil {
		t.Fatalf("BuildStreamRequest: %v", err)
	}
	countTokens, err := BuildCountTokensRequest(m, msgs, opt)
	if err != nil {
		t.Fatalf("BuildCountTokensRequest: %v", err)
	}

	if complete.System != stream.System {
		t.Errorf("complete.System != stream.System\ncomplete: %q\nstream:   %q", complete.System, stream.System)
	}
	if complete.System != countTokens.System {
		t.Errorf("complete.System != countTokens.System\ncomplete:    %q\ncountTokens: %q", complete.System, countTokens.System)
	}
}

// TestRecalledMemoryHeader_Constant verifies the exported constant has the
// expected value so downstream consumers that match on the heading string do
// not silently drift.
func TestRecalledMemoryHeader_Constant(t *testing.T) {
	t.Parallel()

	const want = "# Recalled memory"
	if RecalledMemoryHeader != want {
		t.Errorf("RecalledMemoryHeader = %q, want %q", RecalledMemoryHeader, want)
	}
}
