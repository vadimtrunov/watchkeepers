package runtime_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// recallAllObservations fetches every observation row using the fake
// embedder's deterministic probe vector. Used by the success-reflector
// tests to assert content / metadata round-trip without reproducing
// the production embedding source string.
func recallAllObservations(t *testing.T, db *notebook.DB, activeAt time.Time) []notebook.RecallResult {
	t.Helper()
	probe, err := llm.NewFakeEmbeddingProvider().Embed(context.Background(), "probe")
	if err != nil {
		t.Fatalf("probe embed: %v", err)
	}
	res, err := db.Recall(context.Background(), notebook.RecallQuery{
		Embedding: probe,
		TopK:      100,
		Category:  notebook.CategoryObservation,
		ActiveAt:  activeAt,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	return res
}

// alwaysSampler is a [runtime.SamplerFunc] that always returns true —
// the test analogue of "force every success into a reflection". Used
// to drive the reflector's happy / failure paths without depending on
// the production 1-in-50 rate.
func alwaysSampler() runtime.ReflectionSampler {
	return runtime.SamplerFunc(func(_, _, _ string) bool { return true })
}

// neverSampler is a [runtime.SamplerFunc] that always returns false —
// the test analogue of "never reflect". Drives the sample-false
// short-circuit branch.
func neverSampler() runtime.ReflectionSampler {
	return runtime.SamplerFunc(func(_, _, _ string) bool { return false })
}

// TestNewToolSuccessReflector_NilRememberer_Panics verifies the
// constructor panics on a nil rememberer — matches the panic
// discipline of [NewToolErrorReflector].
func TestNewToolSuccessReflector_NilRememberer_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil rememberer")
		}
	}()
	_, _ = runtime.NewToolSuccessReflector(
		nil,
		runtime.WithSuccessEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithSuccessSampler(alwaysSampler()),
	)
}

// TestNewToolSuccessReflector_MissingEmbedder_ReturnsSentinel verifies
// [ErrEmbedderRequired] surfaces when [WithSuccessEmbedder] is
// omitted.
func TestNewToolSuccessReflector_MissingEmbedder_ReturnsSentinel(t *testing.T) {
	db := freshReflectorDB(t)
	_, err := runtime.NewToolSuccessReflector(
		db,
		runtime.WithSuccessSampler(alwaysSampler()),
	)
	if !errors.Is(err, runtime.ErrEmbedderRequired) {
		t.Errorf("err = %v, want ErrEmbedderRequired", err)
	}
}

// TestNewToolSuccessReflector_MissingSampler_DefaultsTo1in50 verifies
// the M7.2 iter-1 review fix: when [WithSuccessSampler] is omitted
// the constructor wires a [NewDeterministicSampler] at the default
// 1-in-50 rate so the reflector matches the roadmap-pinned contract
// out of the box. Drives 200 Reflect calls (same agent/tool, varying
// call ids) against the default sampler and asserts an observation
// row count consistent with the 1-in-50 rate (small variance allowed
// for FNV distribution).
func TestNewToolSuccessReflector_MissingSampler_DefaultsTo1in50(t *testing.T) {
	db := freshReflectorDB(t)
	r, err := runtime.NewToolSuccessReflector(
		db,
		runtime.WithSuccessEmbedder(llm.NewFakeEmbeddingProvider()),
	)
	if err != nil {
		t.Fatalf("NewToolSuccessReflector with default sampler: %v", err)
	}
	const trials = 200
	for i := 0; i < trials; i++ {
		if err := r.Reflect(
			context.Background(),
			reflectorAgentID,
			"tool",
			"v1",
			"call-"+strconv.Itoa(i),
		); err != nil {
			t.Fatalf("Reflect #%d: %v", i, err)
		}
	}
	obs := recallAllObservations(t, db, time.Now().Add(1*time.Hour))
	// Expected ≈ 200/50 = 4; allow [0, 12] for FNV variance + the
	// recall TopK clamp (100). 12 is loose enough not to flake on
	// rare bursts.
	if len(obs) > 12 {
		t.Errorf("observation count = %d after 200 default-sampler trials; want <= 12", len(obs))
	}
}

// TestToolSuccessReflector_Reflect_HappyPath verifies a sample-true
// decision composes an `observation` Entry whose Subject contains the
// tool name + "success", Content carries the version + call id, and
// ToolVersion round-trips.
func TestToolSuccessReflector_Reflect_HappyPath(t *testing.T) {
	db := freshReflectorDB(t)
	clock := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	r, err := runtime.NewToolSuccessReflector(
		db,
		runtime.WithSuccessEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithSuccessSampler(alwaysSampler()),
		runtime.WithSuccessClock(fixedClock(clock)),
	)
	if err != nil {
		t.Fatalf("NewToolSuccessReflector: %v", err)
	}

	if err := r.Reflect(
		context.Background(),
		reflectorAgentID,
		"sandbox.exec",
		"v1.2.3",
		"call-id-001",
	); err != nil {
		t.Fatalf("Reflect: %v", err)
	}

	res := recallAllObservations(t, db, clock.Add(1*time.Hour))
	if len(res) != 1 {
		t.Fatalf("observation count = %d, want 1", len(res))
	}
	got := res[0]
	if got.Category != notebook.CategoryObservation {
		t.Errorf("Category = %q, want %q", got.Category, notebook.CategoryObservation)
	}
	if !strings.Contains(got.Subject, "sandbox.exec") {
		t.Errorf("Subject = %q, want to contain tool name", got.Subject)
	}
	if !strings.Contains(got.Subject, "success") {
		t.Errorf("Subject = %q, want to contain success marker", got.Subject)
	}
	if !strings.Contains(got.Content, "v1.2.3") {
		t.Errorf("Content = %q, want to contain tool version", got.Content)
	}
	if !strings.Contains(got.Content, "call-id-001") {
		t.Errorf("Content = %q, want to contain call id", got.Content)
	}
	if got.ToolVersion == nil || *got.ToolVersion != "v1.2.3" {
		t.Errorf("ToolVersion = %v, want v1.2.3", got.ToolVersion)
	}
	// Default cooling-off is 0, so ActiveAfter equals CreatedAt at
	// the fixed clock.
	if got.ActiveAfter != clock.UnixMilli() {
		t.Errorf("ActiveAfter = %d, want %d (no cooling-off)",
			got.ActiveAfter, clock.UnixMilli())
	}
}

// TestToolSuccessReflector_Reflect_SampleFalse_NoRow verifies the
// sample-false short-circuit: Reflect returns nil without writing a
// row, without invoking Embed, and without invoking Remember.
func TestToolSuccessReflector_Reflect_SampleFalse_NoRow(t *testing.T) {
	db := freshReflectorDB(t)
	clock := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	r, err := runtime.NewToolSuccessReflector(
		db,
		runtime.WithSuccessEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithSuccessSampler(neverSampler()),
		runtime.WithSuccessClock(fixedClock(clock)),
	)
	if err != nil {
		t.Fatalf("NewToolSuccessReflector: %v", err)
	}

	for i := 0; i < 100; i++ {
		if err := r.Reflect(
			context.Background(),
			reflectorAgentID,
			"sandbox.exec",
			"v1.2.3",
			"call-"+string(rune('a'+i%26)),
		); err != nil {
			t.Fatalf("Reflect #%d: %v", i, err)
		}
	}

	res := recallAllObservations(t, db, clock.Add(1*time.Hour))
	if len(res) != 0 {
		t.Errorf("observation count = %d, want 0 (sampler always false)", len(res))
	}
}

// TestToolSuccessReflector_Reflect_EmbedError_PropagatedAsWrappedErr
// verifies an Embed failure surfaces as a wrapped error so the wiring
// layer can log it; the entry is NOT persisted (the call site sees
// no row).
func TestToolSuccessReflector_Reflect_EmbedError_PropagatedAsWrappedErr(t *testing.T) {
	db := freshReflectorDB(t)
	sentinel := errors.New("embedder down")

	r, err := runtime.NewToolSuccessReflector(
		db,
		runtime.WithSuccessEmbedder(llm.NewFakeEmbeddingProvider(llm.WithEmbedError(sentinel))),
		runtime.WithSuccessSampler(alwaysSampler()),
	)
	if err != nil {
		t.Fatalf("NewToolSuccessReflector: %v", err)
	}

	err = r.Reflect(
		context.Background(),
		reflectorAgentID,
		"tool",
		"v1",
		"call",
	)
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want chain through sentinel %v", err, sentinel)
	}

	res := recallAllObservations(t, db, time.Now().Add(1*time.Hour))
	if len(res) != 0 {
		t.Errorf("observation count = %d, want 0 on embed failure", len(res))
	}
}

// TestToolSuccessReflector_Reflect_CoolingOff verifies the
// [WithSuccessCoolingOff] option offsets ActiveAfter from the clock.
func TestToolSuccessReflector_Reflect_CoolingOff(t *testing.T) {
	db := freshReflectorDB(t)
	clock := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	const cooling = 6 * time.Hour

	r, err := runtime.NewToolSuccessReflector(
		db,
		runtime.WithSuccessEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithSuccessSampler(alwaysSampler()),
		runtime.WithSuccessClock(fixedClock(clock)),
		runtime.WithSuccessCoolingOff(cooling),
	)
	if err != nil {
		t.Fatalf("NewToolSuccessReflector: %v", err)
	}

	if err := r.Reflect(context.Background(), reflectorAgentID, "tool", "v1", "c1"); err != nil {
		t.Fatalf("Reflect: %v", err)
	}

	res := recallAllObservations(t, db, clock.Add(7*time.Hour))
	if len(res) != 1 {
		t.Fatalf("count = %d, want 1", len(res))
	}
	if want := clock.Add(cooling).UnixMilli(); res[0].ActiveAfter != want {
		t.Errorf("ActiveAfter = %d, want %d", res[0].ActiveAfter, want)
	}
}

// TestToolSuccessReflector_Reflect_EmptyCallID_OmitsLine verifies the
// Content compaction when no call id is supplied — the body skips the
// `call_id:` line entirely so Recall queries on body do not match a
// placeholder.
func TestToolSuccessReflector_Reflect_EmptyCallID_OmitsLine(t *testing.T) {
	db := freshReflectorDB(t)
	clock := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	r, err := runtime.NewToolSuccessReflector(
		db,
		runtime.WithSuccessEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithSuccessSampler(alwaysSampler()),
		runtime.WithSuccessClock(fixedClock(clock)),
	)
	if err != nil {
		t.Fatalf("NewToolSuccessReflector: %v", err)
	}

	if err := r.Reflect(context.Background(), reflectorAgentID, "tool", "v1", ""); err != nil {
		t.Fatalf("Reflect: %v", err)
	}

	res := recallAllObservations(t, db, clock.Add(1*time.Hour))
	if len(res) != 1 {
		t.Fatalf("count = %d, want 1", len(res))
	}
	if strings.Contains(res[0].Content, "call_id:") {
		t.Errorf("Content = %q, must not contain call_id line when id is empty", res[0].Content)
	}
}

// TestToolSuccessReflector_Reflect_CtxCancel_PropagatesError verifies
// a pre-cancelled context propagates through Embed as an error (the
// fake embedder honours ctx.Err()).
func TestToolSuccessReflector_Reflect_CtxCancel_PropagatesError(t *testing.T) {
	db := freshReflectorDB(t)

	r, err := runtime.NewToolSuccessReflector(
		db,
		runtime.WithSuccessEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithSuccessSampler(alwaysSampler()),
	)
	if err != nil {
		t.Fatalf("NewToolSuccessReflector: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = r.Reflect(ctx, reflectorAgentID, "tool", "v1", "c1")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want chain through context.Canceled", err)
	}
}
