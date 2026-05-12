package toolregistry

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/eventbus"
)

// Compile-time assertion: the concrete *eventbus.Bus satisfies the
// [Subscriber] interface. Mirrors the cron.LocalPublisher /
// outbox.LocalBus discipline so an eventbus.Bus.Subscribe signature
// change breaks this line at the same time as the [Registry.Start]
// production wiring.
var _ Subscriber = (*eventbus.Bus)(nil)

// canaryToolName is the synthetic tool name embedded in
// PII-redaction tests. The hot-reload event payload must NEVER carry
// it (the payload is supposed to be metadata-only — no manifest
// bodies, no tool names).
const canaryToolName = "CANARY_TOOL_NAME_DO_NOT_LEAK_a7be9c"

// canaryCapability is the synthetic capability id embedded in
// PII-redaction tests. Same canary discipline as canaryToolName.
const canaryCapability = "CANARY_CAPABILITY_DO_NOT_LEAK_44f10b"

func writeManifest(fakeFs *fakeFS, dataDir, sourceName, toolName, version string, caps ...string) {
	parent := filepath.Join(dataDir, "tools", sourceName)
	fakeFs.mu.Lock()
	defer fakeFs.mu.Unlock()
	// Append entry idempotently.
	entries := fakeFs.dirEntries[parent]
	already := false
	for _, e := range entries {
		if e.Name() == toolName {
			already = true
			break
		}
	}
	if !already {
		entries = append(entries, fakeDirEntry{name: toolName, isDir: true})
		fakeFs.dirEntries[parent] = entries
	}
	if len(caps) == 0 {
		caps = []string{"placeholder"}
	}
	body := fmt.Sprintf(
		`{"name":%q,"version":%q,"capabilities":[%q],"schema":{},"dry_run_mode":"none"}`,
		toolName, version, caps[0],
	)
	fakeFs.files[filepath.Join(parent, toolName, "manifest.json")] = []byte(body)
}

func newTestRegistry(t *testing.T, sources []SourceConfig, opts ...RegistryOption) (*Registry, *fakeFS, *fakeClock, *fakePublisher) {
	t.Helper()
	fakeFs := newFakeFS()
	fakeClk := newFakeClock(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC))
	pub := &fakePublisher{}
	deps := RegistryDeps{
		FS:          fakeFs,
		DataDir:     "/data",
		Clock:       fakeClk,
		GracePeriod: 100 * time.Millisecond,
	}
	allOpts := []RegistryOption{WithRegistryPublisher(pub)}
	allOpts = append(allOpts, opts...)
	r, err := NewRegistry(deps, sources, allOpts...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r, fakeFs, fakeClk, pub
}

func TestNewRegistry_PanicsOnNilFS(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil FS")
		}
		if !strings.Contains(fmt.Sprint(r), "deps.FS") {
			t.Errorf("panic message must mention deps.FS, got %q", r)
		}
	}()
	_, _ = NewRegistry(RegistryDeps{Clock: newFakeClock(time.Now()), DataDir: "/data"}, nil)
}

func TestNewRegistry_PanicsOnNilClock(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil Clock")
		}
		if !strings.Contains(fmt.Sprint(r), "deps.Clock") {
			t.Errorf("panic message must mention deps.Clock, got %q", r)
		}
	}()
	_, _ = NewRegistry(RegistryDeps{FS: newFakeFS(), DataDir: "/data"}, nil)
}

func TestNewRegistry_EmptyDataDir(t *testing.T) {
	t.Parallel()
	cases := []string{"", "   ", "\t"}
	for _, in := range cases {
		_, err := NewRegistry(
			RegistryDeps{FS: newFakeFS(), Clock: newFakeClock(time.Now()), DataDir: in},
			nil,
		)
		if !errors.Is(err, ErrInvalidDataDir) {
			t.Errorf("DataDir=%q: expected ErrInvalidDataDir, got %v", in, err)
		}
	}
}

func TestNewRegistry_NegativeGracePeriod(t *testing.T) {
	t.Parallel()
	deps := RegistryDeps{
		FS:          newFakeFS(),
		Clock:       newFakeClock(time.Now()),
		DataDir:     "/data",
		GracePeriod: -1 * time.Second,
	}
	_, err := NewRegistry(deps, nil)
	if !errors.Is(err, ErrInvalidGracePeriod) {
		t.Fatalf("expected ErrInvalidGracePeriod, got %v", err)
	}
}

func TestNewRegistry_PropagatesSourceValidation(t *testing.T) {
	t.Parallel()
	deps := RegistryDeps{FS: newFakeFS(), Clock: newFakeClock(time.Now()), DataDir: "/data"}
	_, err := NewRegistry(deps, []SourceConfig{{Name: ""}})
	if !errors.Is(err, ErrInvalidSourceName) {
		t.Fatalf("expected ErrInvalidSourceName, got %v", err)
	}
}

func TestNewRegistry_InitialEmptySnapshot(t *testing.T) {
	t.Parallel()
	r, _, _, _ := newTestRegistry(t, nil)
	snap := r.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot() returned nil; want empty snapshot at revision 0")
	}
	if snap.Revision != 0 {
		t.Errorf("Revision: got %d, want 0", snap.Revision)
	}
	if snap.Len() != 0 {
		t.Errorf("Len: got %d, want 0", snap.Len())
	}
}

func TestRecompute_HappyPath(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "platform", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	r, fakeFs, _, pub := newTestRegistry(t, sources)
	writeManifest(fakeFs, "/data", "platform", "count_open_prs", "1.0.0")

	snap, err := r.Recompute(context.Background())
	if err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	if snap.Revision != 1 {
		t.Errorf("Revision: got %d, want 1", snap.Revision)
	}
	if snap.Len() != 1 {
		t.Fatalf("Len: got %d, want 1", snap.Len())
	}
	got, ok := snap.Lookup("count_open_prs")
	if !ok || got.Source != "platform" {
		t.Errorf("Lookup: ok=%v src=%q", ok, got.Source)
	}

	events := pub.eventsForTopic(TopicEffectiveToolsetUpdated)
	if len(events) != 1 {
		t.Fatalf("effective_toolset_updated events: got %d, want 1", len(events))
	}
	ev := events[0].event.(EffectiveToolsetUpdated)
	if ev.Revision != 1 {
		t.Errorf("event.Revision: got %d, want 1", ev.Revision)
	}
	if ev.ToolCount != 1 {
		t.Errorf("event.ToolCount: got %d, want 1", ev.ToolCount)
	}
	if ev.SourceCount != 1 {
		t.Errorf("event.SourceCount: got %d, want 1", ev.SourceCount)
	}
}

func TestRecompute_MonotonicRevision(t *testing.T) {
	t.Parallel()
	r, fakeFs, _, _ := newTestRegistry(t, []SourceConfig{
		{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	})
	writeManifest(fakeFs, "/data", "x", "t1", "1")
	for want := int64(1); want <= 3; want++ {
		snap, err := r.Recompute(context.Background())
		if err != nil {
			t.Fatalf("Recompute #%d: %v", want, err)
		}
		if snap.Revision != want {
			t.Errorf("Revision: got %d, want %d", snap.Revision, want)
		}
	}
}

func TestRecompute_PublisherErrorSurfacesAsErrPublishAfterSwap(t *testing.T) {
	t.Parallel()
	r, fakeFs, _, pub := newTestRegistry(t, []SourceConfig{
		{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	})
	writeManifest(fakeFs, "/data", "x", "t1", "1")
	pub.publishErr = errSentinel

	snap, err := r.Recompute(context.Background())
	if err == nil {
		t.Fatal("publisher errors on success path MUST surface to caller")
	}
	if !errors.Is(err, ErrPublishAfterSwap) {
		t.Errorf("expected wrapping of ErrPublishAfterSwap, got %v", err)
	}
	if !errors.Is(err, errSentinel) {
		t.Errorf("expected chain through publisher error, got %v", err)
	}
	if snap == nil || snap.Revision != 1 {
		t.Errorf("snapshot still returned; got %+v", snap)
	}
	// The atomic swap committed even though the publish failed —
	// next Snapshot reads the new state.
	if r.Snapshot().Revision != 1 {
		t.Errorf("current revision after publish-fail: got %d, want 1", r.Snapshot().Revision)
	}
}

// TestRecompute_PublisherErrorAfterSwap_PreservesCtxCancelChain — when
// the publisher itself returns a ctx-cancel error AFTER the atomic
// swap committed, the caller MUST be able to distinguish
// "state moved forward, notification missed" from "scan aborted
// pre-swap" via the [ErrPublishAfterSwap] sentinel. The chain still
// carries the underlying context.Canceled so a caller looking only
// at `errors.Is(..., context.Canceled)` still sees true; the
// disambiguator is the additional ErrPublishAfterSwap match.
func TestRecompute_PublisherErrorAfterSwap_PreservesCtxCancelChain(t *testing.T) {
	t.Parallel()
	r, fakeFs, _, pub := newTestRegistry(t, []SourceConfig{
		{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	})
	writeManifest(fakeFs, "/data", "x", "t1", "1")
	pub.publishErr = context.Canceled

	_, err := r.Recompute(context.Background())
	if !errors.Is(err, ErrPublishAfterSwap) {
		t.Fatalf("expected ErrPublishAfterSwap, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected chain through context.Canceled, got %v", err)
	}
	// State moved forward.
	if r.Snapshot().Revision != 1 {
		t.Errorf("current revision: got %d, want 1 (swap committed)", r.Snapshot().Revision)
	}
}

func TestRecompute_NoPublisherIsFine(t *testing.T) {
	t.Parallel()
	deps := RegistryDeps{
		FS:      newFakeFS(),
		Clock:   newFakeClock(time.Now()),
		DataDir: "/data",
	}
	r, err := NewRegistry(deps, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute without publisher: %v", err)
	}
}

func TestRecompute_CtxCancelLeavesPreviousSnapshot(t *testing.T) {
	t.Parallel()
	r, fakeFs, _, _ := newTestRegistry(t, []SourceConfig{
		{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	})
	writeManifest(fakeFs, "/data", "x", "t1", "1")
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("first Recompute: %v", err)
	}
	prevRev := r.Snapshot().Revision

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Recompute(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if r.Snapshot().Revision != prevRev {
		t.Errorf("cancelled Recompute should not swap; got rev %d, want %d", r.Snapshot().Revision, prevRev)
	}
	// Codex M1 contract: a failed Recompute must NOT consume a
	// revision number. The next successful Recompute should
	// produce revision prevRev+1, not prevRev+2.
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("third Recompute: %v", err)
	}
	if got := r.Snapshot().Revision; got != prevRev+1 {
		t.Errorf("revcounter advanced through cancelled Recompute: got %d, want %d", got, prevRev+1)
	}
}

func TestAcquire_InFlightSnapshotSurvivesRecompute(t *testing.T) {
	t.Parallel()
	r, fakeFs, _, _ := newTestRegistry(t, []SourceConfig{
		{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	})
	writeManifest(fakeFs, "/data", "x", "t1", "1")
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("first Recompute: %v", err)
	}

	// Acquire BEFORE the next Recompute. The in-flight snapshot must
	// keep its revision-1 view even after the swap.
	oldSnap, release := r.Acquire()
	if oldSnap.Revision != 1 {
		t.Fatalf("first Acquire: got rev %d, want 1", oldSnap.Revision)
	}

	// New tool added on disk, Recompute swaps to revision 2.
	writeManifest(fakeFs, "/data", "x", "t2", "1")
	newSnap, err := r.Recompute(context.Background())
	if err != nil {
		t.Fatalf("second Recompute: %v", err)
	}
	if newSnap.Revision != 2 {
		t.Fatalf("newSnap.Revision: got %d, want 2", newSnap.Revision)
	}

	// In-flight (old) view: still revision-1's single tool.
	if oldSnap.Len() != 1 {
		t.Errorf("in-flight oldSnap.Len: got %d, want 1", oldSnap.Len())
	}
	if _, ok := oldSnap.Lookup("t2"); ok {
		t.Errorf("in-flight oldSnap should NOT see t2")
	}
	// New view sees both.
	if newSnap.Len() != 2 {
		t.Errorf("newSnap.Len: got %d, want 2", newSnap.Len())
	}

	// Retiring tracker recorded old entry while refcount > 0.
	rcs := r.RetiringRefcounts()
	if rcs[1] != 1 {
		t.Errorf("retiring refcount for rev 1: got %d, want 1", rcs[1])
	}

	release()
	// Release is idempotent.
	release()

	rcs = r.RetiringRefcounts()
	if rcs[1] != 0 {
		t.Errorf("retiring refcount for rev 1 after release: got %d, want 0", rcs[1])
	}
}

func TestAcquire_NewAfterRecomputeSeesNewSnapshot(t *testing.T) {
	t.Parallel()
	r, fakeFs, _, _ := newTestRegistry(t, []SourceConfig{
		{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	})
	writeManifest(fakeFs, "/data", "x", "t1", "1")
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("first Recompute: %v", err)
	}

	// Acquire #1 BEFORE Recompute.
	beforeSnap, releaseBefore := r.Acquire()
	defer releaseBefore()

	writeManifest(fakeFs, "/data", "x", "t2", "1")
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("second Recompute: %v", err)
	}

	// Acquire #2 AFTER Recompute.
	afterSnap, releaseAfter := r.Acquire()
	defer releaseAfter()

	if beforeSnap.Revision != 1 {
		t.Errorf("before-Recompute Acquire rev: got %d, want 1", beforeSnap.Revision)
	}
	if afterSnap.Revision != 2 {
		t.Errorf("after-Recompute Acquire rev: got %d, want 2", afterSnap.Revision)
	}
}

func TestRecompute_GracePeriodRetirementSweep(t *testing.T) {
	t.Parallel()
	deps := RegistryDeps{
		FS:          newFakeFS(),
		Clock:       newFakeClock(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)),
		DataDir:     "/data",
		GracePeriod: 5 * time.Second,
	}
	logger := &fakeLogger{}
	r, err := NewRegistry(
		deps,
		[]SourceConfig{{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot}},
		WithRegistryLogger(logger),
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	fakeFs := deps.FS.(*fakeFS)
	fakeClk := deps.Clock.(*fakeClock)
	writeManifest(fakeFs, "/data", "x", "t1", "1")
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute #1: %v", err)
	}

	// Advance the clock past the grace period.
	fakeClk.mu.Lock()
	fakeClk.now = fakeClk.now.Add(10 * time.Second)
	fakeClk.mu.Unlock()

	// Second Recompute triggers the sweep — initial rev-0 + retired
	// rev-1 should now both be past grace; rev-1 just got retired in
	// THIS call's swap so it stays.
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute #2: %v", err)
	}
	rcs := r.RetiringRefcounts()
	if _, ok := rcs[0]; ok {
		t.Errorf("rev 0 should have been swept past grace; rcs=%+v", rcs)
	}
	if _, ok := rcs[1]; !ok {
		t.Errorf("rev 1 just retired this Recompute — should still be tracked; rcs=%+v", rcs)
	}

	// One retirement log fired (for rev 0).
	found := false
	for _, e := range logger.snapshot() {
		if e.msg == "toolregistry: effective_toolset retired" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected retirement log, got: %+v", logger.snapshot())
	}
}

func TestStart_SubscribesAndDispatchesRecompute(t *testing.T) {
	t.Parallel()
	// Deterministic channel-handoff instead of a polling deadline:
	// subscribe a sentinel handler directly to
	// TopicEffectiveToolsetUpdated on the SAME bus the registry
	// publishes to. The handler signals on a buffered channel; the
	// test waits on that channel via select+timeout (the timeout is
	// a safety net, not a polling cadence).
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	fakeFs := newFakeFS()
	fakeClk := newFakeClock(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC))
	writeManifest(fakeFs, "/data", "x", "t1", "1")

	bus := eventbus.New()
	t.Cleanup(func() { _ = bus.Close() })

	deps := RegistryDeps{
		FS:          fakeFs,
		DataDir:     "/data",
		Clock:       fakeClk,
		GracePeriod: 100 * time.Millisecond,
	}
	r, err := NewRegistry(deps, sources, WithRegistryPublisher(bus))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	done := make(chan EffectiveToolsetUpdated, 1)
	unsubDone, err := bus.Subscribe(TopicEffectiveToolsetUpdated, func(_ context.Context, ev any) {
		select {
		case done <- ev.(EffectiveToolsetUpdated):
		default:
		}
	})
	if err != nil {
		t.Fatalf("bus.Subscribe sentinel: %v", err)
	}
	defer unsubDone()

	unsubStart, err := r.Start(context.Background(), bus)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer unsubStart()

	if err := bus.Publish(context.Background(), TopicSourceSynced, SourceSynced{
		SourceName:    "x",
		SyncedAt:      time.Now(),
		LocalPath:     "/data/tools/x",
		CorrelationID: "test-corr",
	}); err != nil {
		t.Fatalf("bus.Publish: %v", err)
	}

	select {
	case ev := <-done:
		if ev.Revision != 1 {
			t.Errorf("dispatched event revision: got %d, want 1", ev.Revision)
		}
		if ev.ToolCount != 1 {
			t.Errorf("dispatched event ToolCount: got %d, want 1", ev.ToolCount)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: TopicEffectiveToolsetUpdated was not dispatched after TopicSourceSynced")
	}
}

func TestStart_NilSubscriberPanics(t *testing.T) {
	t.Parallel()
	r, _, _, _ := newTestRegistry(t, nil)
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil subscriber")
		}
	}()
	_, _ = r.Start(context.Background(), nil)
}

func TestRecompute_ConcurrencySafe(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	r, fakeFs, _, _ := newTestRegistry(t, sources)
	writeManifest(fakeFs, "/data", "x", "t1", "1")

	const goroutines = 16
	const iters = 8
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				_, _ = r.Recompute(context.Background())
				snap, release := r.Acquire()
				_ = snap.Len()
				release()
			}
		}()
	}
	wg.Wait()

	// After all Recomputes, revision should equal total calls.
	if got := r.Snapshot().Revision; got != int64(goroutines*iters) {
		t.Errorf("final revision: got %d, want %d", got, goroutines*iters)
	}
}

func TestAcquire_InFlightVsNewBoundaryUnderContention(t *testing.T) {
	t.Parallel()
	// Two-phase contention test. Phase 1: writer goroutine
	// continuously Recomputes; reader goroutines continuously
	// Acquire+inspect+release. Each reader checks its captured
	// snapshot is internally consistent for the duration of its
	// "call" — Names() returns the SAME slice contents from one
	// invocation to the next on the same acquired snapshot.
	r, fakeFs, _, _ := newTestRegistry(t, []SourceConfig{
		{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	})
	writeManifest(fakeFs, "/data", "x", "t1", "1")
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("initial Recompute: %v", err)
	}

	stop := make(chan struct{})
	var writerErrs atomic.Int32
	var wg sync.WaitGroup
	// Writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			i++
			writeManifest(fakeFs, "/data", "x", fmt.Sprintf("t%d", i+1), "1")
			if _, err := r.Recompute(context.Background()); err != nil {
				writerErrs.Add(1)
			}
		}
	}()
	// Readers.
	const readers = 16
	var readerInconsistencies atomic.Int32
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap, release := r.Acquire()
				n1 := snap.Names()
				// brief work — discard both return values
				_, _ = snap.Lookup("t1")
				n2 := snap.Names()
				if len(n1) != len(n2) {
					readerInconsistencies.Add(1)
				}
				for k := range n1 {
					if n1[k] != n2[k] {
						readerInconsistencies.Add(1)
					}
				}
				release()
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	if writerErrs.Load() != 0 {
		t.Errorf("writer errors: %d", writerErrs.Load())
	}
	if readerInconsistencies.Load() != 0 {
		t.Errorf("reader saw inconsistent snapshot under contention: %d incidents", readerInconsistencies.Load())
	}
}

// Source-grep AC: registry / scanner / effective production files
// MUST stay audit-free (audit emission lives in M9.7). Mirrors the
// M9.1.a discipline.
func TestSourceGrepAC_NoAuditCallsInRegistrySources(t *testing.T) {
	t.Parallel()
	productionFiles := []string{
		"effective.go",
		"scanner.go",
		"registry.go",
	}
	for _, name := range productionFiles {
		raw, err := readFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(raw)
		for _, banned := range []string{"keeperslog.", ".Append("} {
			if containsOutsideComments(body, banned) {
				t.Errorf("%s: contains banned token %q outside comments (audit emission belongs to M9.7)", name, banned)
			}
		}
	}
}

// PII canary: the hot-reload event payload must NEVER carry
// manifest bodies (tool names, capabilities, schema bytes). A
// subscriber that logs the payload verbatim must not leak the
// canary substrings.
func TestPIIRedactionCanary_EffectiveToolsetUpdatedPayload(t *testing.T) {
	t.Parallel()
	r, fakeFs, _, pub := newTestRegistry(t, []SourceConfig{
		{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	})
	writeManifest(fakeFs, "/data", "x", canaryToolName, "1.0.0", canaryCapability)

	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	events := pub.eventsForTopic(TopicEffectiveToolsetUpdated)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	body := fmt.Sprintf("%+v", events[0].event)
	if strings.Contains(body, canaryToolName) {
		t.Errorf("EffectiveToolsetUpdated leaked tool name: %q", body)
	}
	if strings.Contains(body, canaryCapability) {
		t.Errorf("EffectiveToolsetUpdated leaked capability id: %q", body)
	}
}

// PII canary on logger: when the scanner logs a per-tool decode
// failure, the log must NOT carry the file's raw manifest contents
// (only the err_type + tool dir + source name).
func TestPIIRedactionCanary_ScannerLogger(t *testing.T) {
	t.Parallel()
	r, fakeFs, _, _ := newTestRegistry(t, []SourceConfig{
		{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	})
	logger := &fakeLogger{}
	// Override registry logger to capture the per-tool failure log.
	r.logger = logger
	parent := filepath.Join("/data", "tools", "x")
	fakeFs.dirEntries[parent] = []fs.DirEntry{fakeDirEntry{name: "bad", isDir: true}}
	// Manifest that smuggles the canary in via an unknown field —
	// would fail strict decoding. The scanner logs the decode error
	// but MUST NOT log the raw bytes.
	fakeFs.files[filepath.Join(parent, "bad", "manifest.json")] = []byte(
		`{"name":"x","version":"1","capabilities":["c"],"schema":{},"leaked":"` + canaryToolName + `"}`,
	)
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	for _, e := range logger.snapshot() {
		if strings.Contains(e.msg, canaryToolName) {
			t.Errorf("logger msg leaked canary: %q", e.msg)
		}
		for _, v := range e.kv {
			if strings.Contains(fmt.Sprint(v), canaryToolName) {
				t.Errorf("logger kv leaked canary: %v", v)
			}
		}
	}
}

func TestSnapshot_ConsistentWithCurrent(t *testing.T) {
	t.Parallel()
	r, fakeFs, _, _ := newTestRegistry(t, []SourceConfig{
		{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	})
	writeManifest(fakeFs, "/data", "x", "t1", "1")
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	a := r.Snapshot()
	b := r.Snapshot()
	if a != b {
		t.Errorf("Snapshot() should be stable between calls under no-Recompute; got %p vs %p", a, b)
	}
}

// TestRecompute_GracePeriodZeroSweepsImmediately — with grace=0 the
// just-appended entry is immediately past deadline on the same call
// and the inline sweep drops it before Recompute returns. The
// retiring list ends the call EMPTY (the just-retired entry is gone
// too). Test pins the lesson 2 contract.
func TestRecompute_GracePeriodZeroSweepsImmediately(t *testing.T) {
	t.Parallel()
	deps := RegistryDeps{
		FS:          newFakeFS(),
		Clock:       newFakeClock(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)),
		DataDir:     "/data",
		GracePeriod: 0,
	}
	r, err := NewRegistry(
		deps,
		[]SourceConfig{{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot}},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	fakeFs := deps.FS.(*fakeFS)
	writeManifest(fakeFs, "/data", "x", "t1", "1")

	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	rcs := r.RetiringRefcounts()
	if len(rcs) != 0 {
		t.Errorf("with grace=0, retiring list must be empty after Recompute; got %+v", rcs)
	}
}

// TestRecompute_NonCtxBuildErrorDoesNotSwap — although BuildEffective
// today only returns ctx-cancel errors at the top level (per-source
// readdir errors are isolated and per-tool decode errors are
// logged-then-skipped), the swap-protection invariant is critical for
// M9.2 / M9.4 which may widen BuildEffective's failure surface.
// Asserts: when BuildEffective returns ANY non-nil error, the current
// snapshot pointer is NOT swapped and the revision counter is NOT
// advanced.
func TestRecompute_NonCtxBuildErrorDoesNotSwap(t *testing.T) {
	t.Parallel()
	// Synthesize a "build error" via ctx cancellation — the only
	// error path BuildEffective surfaces today. The invariants
	// asserted (no swap, no revcounter advance) hold for ANY
	// returned err.
	r, fakeFs, _, _ := newTestRegistry(t, []SourceConfig{
		{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	})
	writeManifest(fakeFs, "/data", "x", "t1", "1")
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("warm-up Recompute: %v", err)
	}
	preRev := r.Snapshot().Revision
	preSnapPtr := r.Snapshot()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Recompute(ctx); err == nil {
		t.Fatal("expected an error from cancelled Recompute")
	}
	postSnapPtr := r.Snapshot()
	if postSnapPtr != preSnapPtr {
		t.Errorf("snapshot pointer changed despite Recompute error: pre=%p post=%p", preSnapPtr, postSnapPtr)
	}
	if r.Snapshot().Revision != preRev {
		t.Errorf("revision advanced despite Recompute error: pre=%d post=%d", preRev, r.Snapshot().Revision)
	}
}

// TestAcquire_LeakedReleaseWithGraceZero — refcount stays > 0 on a
// retired entry whose release was never called; the registry's
// time-based sweep still drops the entry from retiring on the next
// (post-grace) Recompute, surfacing the leak via the
// refcount_at_retirement log field. Pins the Acquire godoc contract
// that refcount does NOT gate retirement.
func TestAcquire_LeakedReleaseWithGraceZero(t *testing.T) {
	t.Parallel()
	deps := RegistryDeps{
		FS:          newFakeFS(),
		Clock:       newFakeClock(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)),
		DataDir:     "/data",
		GracePeriod: 0,
	}
	logger := &fakeLogger{}
	r, err := NewRegistry(
		deps,
		[]SourceConfig{{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot}},
		WithRegistryLogger(logger),
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	fakeFs := deps.FS.(*fakeFS)
	writeManifest(fakeFs, "/data", "x", "t1", "1")
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute #1: %v", err)
	}

	// Acquire BEFORE the second Recompute. Deliberately do NOT
	// release — model a leak.
	snap, _ := r.Acquire()
	if snap.Revision != 1 {
		t.Fatalf("acquired rev: got %d, want 1", snap.Revision)
	}

	writeManifest(fakeFs, "/data", "x", "t2", "1")
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute #2: %v", err)
	}
	// With grace=0, the just-retired rev-1 entry is swept on the
	// SAME Recompute call. The leak should appear in the retirement
	// log as refcount_at_retirement=1.
	found := false
	for _, e := range logger.snapshot() {
		if e.msg != "toolregistry: effective_toolset retired" {
			continue
		}
		for i := 0; i+1 < len(e.kv); i += 2 {
			if k, ok := e.kv[i].(string); ok && k == "refcount_at_retirement" {
				if v, ok := e.kv[i+1].(int32); ok && v == 1 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("expected retirement log with refcount_at_retirement=1; got: %+v", logger.snapshot())
	}
	// In-flight snapshot is still readable.
	if snap.Len() != 1 {
		t.Errorf("leaked snapshot stayed usable; got Len=%d, want 1", snap.Len())
	}
}

// TestEffectiveToolsetUpdated_FieldAllowlist — compile-time / runtime
// AC that pins the payload to a small known-PII-clean field set. A
// future PR adding e.g. `ToolNames []string` would fail this test —
// the canary tests rely on detecting specific substrings, which a
// generic field addition could slip past. The allowlist catches the
// addition itself.
func TestEffectiveToolsetUpdated_FieldAllowlist(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"Revision":      "int64",
		"BuiltAt":       "time.Time",
		"ToolCount":     "int",
		"SourceCount":   "int",
		"CorrelationID": "string",
	}
	typ := reflect.TypeOf(EffectiveToolsetUpdated{})
	if typ.NumField() != len(want) {
		t.Errorf("EffectiveToolsetUpdated has %d fields; expected %d (allowlist drift — review PII discipline before adding)", typ.NumField(), len(want))
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		wantType, ok := want[f.Name]
		if !ok {
			t.Errorf("unexpected EffectiveToolsetUpdated field %q (type %s) — add to allowlist + PII review", f.Name, f.Type)
			continue
		}
		if f.Type.String() != wantType {
			t.Errorf("field %q: type %s, want %s", f.Name, f.Type, wantType)
		}
	}
}

func TestRegistry_SourcesReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "platform", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	r, _, _, _ := newTestRegistry(t, sources)
	got := r.Sources()
	got[0].Name = "MUTATED"
	again := r.Sources()
	if again[0].Name != "platform" {
		t.Errorf("Sources() returned reference-shared slice: got %q", again[0].Name)
	}
}
