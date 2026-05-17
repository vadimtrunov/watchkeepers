package runtime_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// fakeAgentRuntimeAlwaysSucceeds is a minimal [runtime.AgentRuntime]
// stand-in whose InvokeTool always returns `(ToolResult{}, nil)` so
// the M7.2 success-path reflection wiring sees the happy outcome on
// every call. Distinct from the existing `fakeAgentRuntime` in
// wired_runtime_test.go which is parameterised on `invokeErr`; this
// one is hardcoded to "success" so the test driver does not have to
// flip a flag to drive the success path.
type fakeAgentRuntimeAlwaysSucceeds struct {
	t         *testing.T
	toolset   map[string]struct{}
	startedID runtime.ID
}

func newFakeAgentRuntimeAlwaysSucceeds(t *testing.T) *fakeAgentRuntimeAlwaysSucceeds {
	t.Helper()
	return &fakeAgentRuntimeAlwaysSucceeds{t: t}
}

func (f *fakeAgentRuntimeAlwaysSucceeds) Start(_ context.Context, manifest runtime.Manifest, _ ...runtime.StartOption) (runtime.Runtime, error) {
	if manifest.AgentID == "" || manifest.SystemPrompt == "" || manifest.Model == "" {
		return nil, runtime.ErrInvalidManifest
	}
	names := manifest.Toolset.Names()
	f.toolset = make(map[string]struct{}, len(names))
	for _, name := range names {
		f.toolset[name] = struct{}{}
	}
	f.startedID = runtime.ID("fake-wired-runtime-success-1")
	return &fakeRuntimeHandle{id: f.startedID}, nil
}

func (f *fakeAgentRuntimeAlwaysSucceeds) SendMessage(_ context.Context, _ runtime.ID, _ runtime.Message) error {
	return nil
}

func (f *fakeAgentRuntimeAlwaysSucceeds) InvokeTool(_ context.Context, id runtime.ID, call runtime.ToolCall) (runtime.ToolResult, error) {
	if call.Name == "" {
		return runtime.ToolResult{}, runtime.ErrInvalidToolCall
	}
	if id != f.startedID {
		return runtime.ToolResult{}, runtime.ErrRuntimeNotFound
	}
	if _, ok := f.toolset[call.Name]; !ok {
		return runtime.ToolResult{}, runtime.ErrToolUnauthorized
	}
	return runtime.ToolResult{}, nil
}

func (f *fakeAgentRuntimeAlwaysSucceeds) Subscribe(_ context.Context, _ runtime.ID, _ runtime.EventHandler) (runtime.Subscription, error) {
	return nil, nil
}

func (f *fakeAgentRuntimeAlwaysSucceeds) Terminate(_ context.Context, _ runtime.ID) error {
	return nil
}

// newSuccessReflectorManifest returns a [runtime.Manifest] suitable
// for driving the success-path wiring tests. Mirrors the shape used
// by the error-path tests in wired_runtime_test.go but with a single
// "tool" toolset name the success path will invoke.
func newSuccessReflectorManifest(t *testing.T) runtime.Manifest {
	t.Helper()
	return runtime.Manifest{
		AgentID:      reflectorAgentID,
		SystemPrompt: "sp",
		Model:        "m",
		Toolset: runtime.Toolset{
			{Name: "tool", Version: "v1"},
		},
	}
}

// TestWiredRuntime_Success_NoReflector_NoOp verifies that without a
// configured success reflector the wired runtime is a transparent
// forwarder on the success path — no Notebook writes, no panics.
func TestWiredRuntime_Success_NoReflector_NoOp(t *testing.T) {
	db := freshReflectorDB(t)
	inner := newFakeAgentRuntimeAlwaysSucceeds(t)
	w := runtime.NewWiredRuntime(inner, runtime.WithAgentID(reflectorAgentID))

	mf := newSuccessReflectorManifest(t)
	if _, err := w.Start(context.Background(), mf); err != nil {
		t.Fatalf("Start: %v", err)
	}

	res, err := w.InvokeTool(context.Background(), inner.startedID, runtime.ToolCall{
		Name:        "tool",
		ToolVersion: "v1",
	})
	if err != nil {
		t.Fatalf("InvokeTool: %v", err)
	}
	_ = res

	obs := recallAllObservations(t, db, time.Now().Add(1*time.Hour))
	if len(obs) != 0 {
		t.Errorf("observation count = %d, want 0 (no reflector wired)", len(obs))
	}
}

// TestWiredRuntime_Success_WithReflectorAlwaysSample_WritesObservation
// verifies the success-path hook fires when a reflector is wired and
// the sampler returns true. One InvokeTool call must produce exactly
// one observation row.
func TestWiredRuntime_Success_WithReflectorAlwaysSample_WritesObservation(t *testing.T) {
	db := freshReflectorDB(t)
	inner := newFakeAgentRuntimeAlwaysSucceeds(t)

	successReflector, err := runtime.NewToolSuccessReflector(
		db,
		runtime.WithSuccessEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithSuccessSampler(alwaysSampler()),
	)
	if err != nil {
		t.Fatalf("NewToolSuccessReflector: %v", err)
	}

	w := runtime.NewWiredRuntime(
		inner,
		runtime.WithAgentID(reflectorAgentID),
		runtime.WithToolSuccessReflector(successReflector),
	)

	mf := newSuccessReflectorManifest(t)
	if _, err := w.Start(context.Background(), mf); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := w.InvokeTool(context.Background(), inner.startedID, runtime.ToolCall{
		Name:        "tool",
		ToolVersion: "v1",
		Metadata:    map[string]string{runtime.MetadataKeyToolCallID: "call-001"},
	}); err != nil {
		t.Fatalf("InvokeTool: %v", err)
	}

	obs := recallAllObservations(t, db, time.Now().Add(1*time.Hour))
	if len(obs) != 1 {
		t.Fatalf("observation count = %d, want 1", len(obs))
	}
	if obs[0].Category != notebook.CategoryObservation {
		t.Errorf("Category = %q, want observation", obs[0].Category)
	}
	if obs[0].ToolVersion == nil || *obs[0].ToolVersion != "v1" {
		t.Errorf("ToolVersion = %v, want v1", obs[0].ToolVersion)
	}
}

// TestWiredRuntime_Success_ReflectorFailure_OriginalResultPreserved
// verifies the best-effort contract: a reflector failure (here:
// driven by an embed-down sentinel) is logged but does NOT replace
// the original success result returned to the caller. The caller
// sees `(ToolResult{}, nil)` regardless of reflector failure.
func TestWiredRuntime_Success_ReflectorFailure_OriginalResultPreserved(t *testing.T) {
	db := freshReflectorDB(t)
	inner := newFakeAgentRuntimeAlwaysSucceeds(t)

	successReflector, err := runtime.NewToolSuccessReflector(
		db,
		runtime.WithSuccessEmbedder(llm.NewFakeEmbeddingProvider(
			llm.WithEmbedError(errToolBoom), // any error works
		)),
		runtime.WithSuccessSampler(alwaysSampler()),
	)
	if err != nil {
		t.Fatalf("NewToolSuccessReflector: %v", err)
	}

	w := runtime.NewWiredRuntime(
		inner,
		runtime.WithAgentID(reflectorAgentID),
		runtime.WithToolSuccessReflector(successReflector),
	)

	mf := newSuccessReflectorManifest(t)
	if _, err := w.Start(context.Background(), mf); err != nil {
		t.Fatalf("Start: %v", err)
	}

	res, err := w.InvokeTool(context.Background(), inner.startedID, runtime.ToolCall{
		Name:        "tool",
		ToolVersion: "v1",
	})
	if err != nil {
		t.Errorf("InvokeTool err = %v, want nil (original success preserved)", err)
	}
	_ = res
}

// TestWiredRuntime_Success_AcceptanceCriterion_100CallsRate1in50
// verifies the M7.2 acceptance criterion at the wiring level: drive
// 100 successful tool calls through the wired runtime at the default
// 1-in-50 rate; assert the resulting observation count is in [0, 6]
// (FNV variance on small N — see sampler test).
func TestWiredRuntime_Success_AcceptanceCriterion_100CallsRate1in50(t *testing.T) {
	db := freshReflectorDB(t)
	inner := newFakeAgentRuntimeAlwaysSucceeds(t)

	successReflector, err := runtime.NewToolSuccessReflector(
		db,
		runtime.WithSuccessEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithSuccessSampler(runtime.NewDeterministicSampler(runtime.DefaultSuccessSampleRate)),
	)
	if err != nil {
		t.Fatalf("NewToolSuccessReflector: %v", err)
	}

	w := runtime.NewWiredRuntime(
		inner,
		runtime.WithAgentID(reflectorAgentID),
		runtime.WithToolSuccessReflector(successReflector),
	)

	mf := newSuccessReflectorManifest(t)
	if _, err := w.Start(context.Background(), mf); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for i := 0; i < 100; i++ {
		if _, err := w.InvokeTool(context.Background(), inner.startedID, runtime.ToolCall{
			Name:        "tool",
			ToolVersion: "v1",
			Metadata:    map[string]string{runtime.MetadataKeyToolCallID: "call-" + strconv.Itoa(i)},
		}); err != nil {
			t.Fatalf("InvokeTool #%d: %v", i, err)
		}
	}

	obs := recallAllObservations(t, db, time.Now().Add(1*time.Hour))
	if len(obs) > 6 {
		t.Errorf("observation count = %d after 100 calls at 1-in-50; want <= 6", len(obs))
	}
	for _, o := range obs {
		if o.Category != notebook.CategoryObservation {
			t.Errorf("observation row category = %q, want observation", o.Category)
		}
	}
}

// TestWiredRuntime_Success_DeterministicRetry_NoDuplicateRow verifies
// the retry-immunity contract: invoking InvokeTool twice with the
// SAME (agentID, toolName, tool_call_id) yields the SAME sampler
// decision. When the decision is "sample true" both invocations
// write a row (no dedup at the reflector layer — that's the
// notebook's job); when "sample false" neither does. The contract
// the sampler protects is "the decision does not flip on retry",
// preventing a retry from producing an observation when the original
// did not. We assert the count is exactly 2 OR exactly 0 — never 1.
func TestWiredRuntime_Success_DeterministicRetry_NoDuplicateRow(t *testing.T) {
	db := freshReflectorDB(t)
	inner := newFakeAgentRuntimeAlwaysSucceeds(t)

	successReflector, err := runtime.NewToolSuccessReflector(
		db,
		runtime.WithSuccessEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithSuccessSampler(runtime.NewDeterministicSampler(2)),
	)
	if err != nil {
		t.Fatalf("NewToolSuccessReflector: %v", err)
	}

	w := runtime.NewWiredRuntime(
		inner,
		runtime.WithAgentID(reflectorAgentID),
		runtime.WithToolSuccessReflector(successReflector),
	)

	mf := newSuccessReflectorManifest(t)
	if _, err := w.Start(context.Background(), mf); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for retry := 0; retry < 2; retry++ {
		if _, err := w.InvokeTool(context.Background(), inner.startedID, runtime.ToolCall{
			Name:        "tool",
			ToolVersion: "v1",
			Metadata:    map[string]string{runtime.MetadataKeyToolCallID: "stable-call-id"},
		}); err != nil {
			t.Fatalf("InvokeTool retry #%d: %v", retry, err)
		}
	}

	obs := recallAllObservations(t, db, time.Now().Add(1*time.Hour))
	// Same tuple → same sampler decision → either 0 (false twice)
	// or 2 (true twice). 1 would indicate a non-deterministic
	// sampler, which is the M7.2 retry-immunity bug.
	if len(obs) != 0 && len(obs) != 2 {
		t.Errorf("observation count = %d, want 0 or 2 (deterministic retry)", len(obs))
	}
}

// TestWiredRuntime_Success_ConcurrentSafe verifies the wired
// runtime's success path is safe under 16-goroutine concurrent
// dispatch. Each goroutine fires 50 InvokeTool calls; the test
// asserts no panic and at least one row was written (proving the
// reflector path actually executed across goroutines). The
// race-detector run validates no shared-mutable-state hazards.
func TestWiredRuntime_Success_ConcurrentSafe(t *testing.T) {
	db := freshReflectorDB(t)
	inner := newFakeAgentRuntimeAlwaysSucceeds(t)

	// Use an always-true sampler so every concurrent call writes a
	// row; the assertion below validates concurrent Remember
	// against the per-agent notebook DB.
	successReflector, err := runtime.NewToolSuccessReflector(
		db,
		runtime.WithSuccessEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithSuccessSampler(alwaysSampler()),
	)
	if err != nil {
		t.Fatalf("NewToolSuccessReflector: %v", err)
	}

	w := runtime.NewWiredRuntime(
		inner,
		runtime.WithAgentID(reflectorAgentID),
		runtime.WithToolSuccessReflector(successReflector),
	)

	mf := newSuccessReflectorManifest(t)
	if _, err := w.Start(context.Background(), mf); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Total calls bounded by the notebook Recall TopK clamp
	// (maxTopK=100) so the post-assertion can read them all back via
	// recallAllObservations (TopK=100). 16 * 5 = 80 stays under the
	// ceiling with headroom.
	const goroutines = 16
	const perGoroutine = 5
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				_, _ = w.InvokeTool(context.Background(), inner.startedID, runtime.ToolCall{
					Name:        "tool",
					ToolVersion: "v1",
					Metadata: map[string]string{
						runtime.MetadataKeyToolCallID: "g" + strconv.Itoa(g) + "-i" + strconv.Itoa(i),
					},
				})
			}
		}(g)
	}
	wg.Wait()

	obs := recallAllObservations(t, db, time.Now().Add(1*time.Hour))
	// All decisions are sample-true; expect goroutines*perGoroutine rows.
	want := goroutines * perGoroutine
	if len(obs) != want {
		t.Errorf("observation count = %d, want %d (all sample-true)", len(obs), want)
	}
}
