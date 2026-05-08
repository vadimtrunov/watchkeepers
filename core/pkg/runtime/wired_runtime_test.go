package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// Compile-time assertion: *WiredRuntime satisfies AgentRuntime so a
// caller can drop a wired wrapper anywhere the interface is expected.
var _ runtime.AgentRuntime = (*runtime.WiredRuntime)(nil)

// fakeAgentRuntime is the minimal AgentRuntime stand-in the wiring
// tests drive directly. It records the canned manifest's toolset (so
// the unauthorized-tool branch surfaces ErrToolUnauthorized natively)
// and returns the configured invokeErr from InvokeTool. This is a
// tiny in-package_test fake — the runtime package's hand-rolled
// FakeRuntime lives in fake_runtime_test.go in `package runtime`
// (internal) and is not visible from the external test package; we
// re-implement just enough surface to drive the wiring contract.
type fakeAgentRuntime struct {
	t         *testing.T
	invokeErr error
	toolset   map[string]struct{}
	startedID runtime.ID
}

func newFakeAgentRuntime(t *testing.T, invokeErr error) *fakeAgentRuntime {
	t.Helper()
	return &fakeAgentRuntime{t: t, invokeErr: invokeErr}
}

func (f *fakeAgentRuntime) Start(_ context.Context, manifest runtime.Manifest, _ ...runtime.StartOption) (runtime.Runtime, error) {
	if manifest.AgentID == "" || manifest.SystemPrompt == "" || manifest.Model == "" {
		return nil, runtime.ErrInvalidManifest
	}
	f.toolset = make(map[string]struct{}, len(manifest.Toolset))
	for _, name := range manifest.Toolset {
		f.toolset[name] = struct{}{}
	}
	f.startedID = runtime.ID("fake-wired-runtime-1")
	return &fakeRuntimeHandle{id: f.startedID}, nil
}

func (f *fakeAgentRuntime) SendMessage(_ context.Context, _ runtime.ID, _ runtime.Message) error {
	return nil
}

func (f *fakeAgentRuntime) InvokeTool(_ context.Context, id runtime.ID, call runtime.ToolCall) (runtime.ToolResult, error) {
	if call.Name == "" {
		return runtime.ToolResult{}, runtime.ErrInvalidToolCall
	}
	if id != f.startedID {
		return runtime.ToolResult{}, runtime.ErrRuntimeNotFound
	}
	if _, ok := f.toolset[call.Name]; !ok {
		return runtime.ToolResult{}, runtime.ErrToolUnauthorized
	}
	if f.invokeErr != nil {
		return runtime.ToolResult{}, f.invokeErr
	}
	return runtime.ToolResult{}, nil
}

func (f *fakeAgentRuntime) Subscribe(_ context.Context, _ runtime.ID, _ runtime.EventHandler) (runtime.Subscription, error) {
	return nil, nil
}

func (f *fakeAgentRuntime) Terminate(_ context.Context, _ runtime.ID) error {
	return nil
}

// fakeRuntimeHandle satisfies runtime.Runtime for the wired tests.
type fakeRuntimeHandle struct{ id runtime.ID }

func (h *fakeRuntimeHandle) ID() runtime.ID { return h.id }

// errToolBoom is a sentinel error injected into the FakeRuntime to
// simulate a tool-side failure that should trigger the M5.6.b
// reflector. Distinct from ErrSandboxKilled so the wiring's
// classifyToolError fallback path (typeName) is exercised.
var errToolBoom = errors.New("wiring-test: tool boom")

// wiredHarness bundles the moving parts of the wiring tests: the
// underlying FakeRuntime, the per-agent notebook.DB, the
// ToolErrorReflector built on top, and the started runtime handle.
// Returned by setupWiredHarness so each test case stays compact.
type wiredHarness struct {
	fake      runtime.AgentRuntime // exposed via the AgentRuntime interface
	rt        runtime.Runtime
	db        *notebook.DB
	reflector *runtime.ToolErrorReflector
}

// setupWiredHarness opens a real notebook.DB rooted at t.TempDir(),
// constructs a ToolErrorReflector with a deterministic clock, and
// returns the harness. Callers wrap the FakeRuntime via
// NewWiredRuntime themselves so the per-test option set
// (with-reflector vs no-reflector) stays explicit at the call site.
func setupWiredHarness(t *testing.T, fakeInvokeErr error) *wiredHarness {
	t.Helper()
	t.Setenv("WATCHKEEPER_DATA", t.TempDir())

	db, err := notebook.Open(context.Background(), reflectorAgentID)
	if err != nil {
		t.Fatalf("notebook.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reflector, err := runtime.NewToolErrorReflector(
		db,
		runtime.WithEmbedder(llm.NewFakeEmbeddingProvider()),
	)
	if err != nil {
		t.Fatalf("NewToolErrorReflector: %v", err)
	}

	// We need to drive the FakeRuntime directly to inject a
	// canned invokeErr. Re-use the test helper indirectly: spawn a
	// FakeRuntime via the runtime package's exported AgentRuntime
	// interface. Since FakeRuntime is _test.go-only it is NOT
	// importable here; we use a tiny in-package adapter from a
	// test-only stub.
	fake := newFakeAgentRuntime(t, fakeInvokeErr)

	rt, err := fake.Start(context.Background(), runtime.Manifest{
		AgentID:      reflectorAgentID,
		SystemPrompt: "test system prompt",
		Model:        "claude-sonnet-4",
		Toolset:      []string{"sandbox.exec", "shell.run"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return &wiredHarness{fake: fake, rt: rt, db: db, reflector: reflector}
}

// TestWiredRuntime_InvokeTool_ToolError_WritesLessonAndReturnsOriginalError
// — when the inner runtime returns a tool error, the reflector writes
// a `lesson` row AND the original error reaches the caller unchanged.
// This is the headline wiring contract from the M5.6.b test plan.
func TestWiredRuntime_InvokeTool_ToolError_WritesLessonAndReturnsOriginalError(t *testing.T) {
	// no t.Parallel: t.Setenv inside setupWiredHarness.
	h := setupWiredHarness(t, errToolBoom)

	wired := runtime.NewWiredRuntime(
		h.fake,
		runtime.WithToolErrorReflector(h.reflector),
		runtime.WithAgentID(reflectorAgentID),
	)

	_, err := wired.InvokeTool(context.Background(), h.rt.ID(), runtime.ToolCall{
		Name:        "sandbox.exec",
		ToolVersion: "v1.2.3",
		Arguments:   map[string]any{"cmd": "ls"},
	})
	if !errors.Is(err, errToolBoom) {
		t.Fatalf("InvokeTool err = %v, want errors.Is errToolBoom (original tool error)", err)
	}

	res := recallAllLessons(t, h.db, time.Now().Add(48*time.Hour))
	if len(res) != 1 {
		t.Fatalf("Recall result count = %d, want exactly 1 lesson", len(res))
	}
	if res[0].ToolVersion == nil || *res[0].ToolVersion != "v1.2.3" {
		t.Errorf("Recall ToolVersion = %v, want v1.2.3 (forwarded from ToolCall)", res[0].ToolVersion)
	}
	// Sanity: Subject contains tool name; Content contains the err msg.
	if !contains(res[0].Subject, "sandbox.exec") {
		t.Errorf("Subject = %q does not contain tool name", res[0].Subject)
	}
	if !contains(res[0].Content, errToolBoom.Error()) {
		t.Errorf("Content = %q does not contain inner error message", res[0].Content)
	}
}

// TestWiredRuntime_InvokeTool_NoReflector_BehavesLikeBareRuntime — a
// WiredRuntime constructed without WithToolErrorReflector option must
// behave identically to the underlying AgentRuntime: the original
// error is returned and no notebook row is written. This is the
// regression guard from the M5.6.b test plan.
func TestWiredRuntime_InvokeTool_NoReflector_BehavesLikeBareRuntime(t *testing.T) {
	// no t.Parallel: t.Setenv inside setupWiredHarness.
	h := setupWiredHarness(t, errToolBoom)

	wired := runtime.NewWiredRuntime(h.fake)

	_, err := wired.InvokeTool(context.Background(), h.rt.ID(), runtime.ToolCall{
		Name:      "sandbox.exec",
		Arguments: map[string]any{"cmd": "ls"},
	})
	if !errors.Is(err, errToolBoom) {
		t.Fatalf("InvokeTool err = %v, want errors.Is errToolBoom", err)
	}
	res := recallAllLessons(t, h.db, time.Now().Add(48*time.Hour))
	if len(res) != 0 {
		t.Fatalf("Recall result count = %d, want 0 (no reflector wired)", len(res))
	}
}

// TestWiredRuntime_InvokeTool_ToolUnauthorized_DoesNotReflect — the
// ErrToolUnauthorized branch is a pre-tool-execution guard (no real
// tool ran) so the wiring SHOULD NOT write a lesson row. Pins the
// shouldSkipReflection short-circuit.
func TestWiredRuntime_InvokeTool_ToolUnauthorized_DoesNotReflect(t *testing.T) {
	// no t.Parallel: t.Setenv inside setupWiredHarness.
	h := setupWiredHarness(t, nil)

	wired := runtime.NewWiredRuntime(
		h.fake,
		runtime.WithToolErrorReflector(h.reflector),
		runtime.WithAgentID(reflectorAgentID),
	)

	_, err := wired.InvokeTool(context.Background(), h.rt.ID(), runtime.ToolCall{
		Name:      "delete_universe", // not in toolset
		Arguments: nil,
	})
	if !errors.Is(err, runtime.ErrToolUnauthorized) {
		t.Fatalf("InvokeTool err = %v, want errors.Is ErrToolUnauthorized", err)
	}
	res := recallAllLessons(t, h.db, time.Now().Add(48*time.Hour))
	if len(res) != 0 {
		t.Errorf("Recall result count = %d, want 0 (unauthorized errors skip reflection)", len(res))
	}
}

// contains is a tiny std-lib-free strings.Contains alias kept local to
// avoid importing strings just for two assertions.
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
