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
	"testing"
	"time"
)

// canaryShadowedSchemaBody — synthetic capability id stamped into
// shadowed manifests. The M9.2 [TopicToolShadowed] payload carries
// the tool NAME and both VERSIONS (operator-facing identifiers) but
// MUST NOT carry the manifest's `Capabilities` or `Schema` bodies —
// those can be AI-authored under M9.4 and may contain proprietary
// blobs that a verbose subscriber log would dump.
const canaryShadowedCapability = "CANARY_SHADOWED_CAP_DO_NOT_LEAK_9b3f01"

func TestBuildEffective_DetectsShadowsAcrossSources(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	// Three sources in priority order: A < B < C. A wins for "shared".
	for _, src := range []string{"a", "b", "c"} {
		parent := filepath.Join("/data", "tools", src)
		fakeFs.dirEntries[parent] = []fs.DirEntry{
			fakeDirEntry{name: "shared", isDir: true},
		}
		ver := map[string]string{"a": "1.0", "b": "2.0", "c": "3.0"}[src]
		fakeFs.files[filepath.Join(parent, "shared", "manifest.json")] = []byte(fmt.Sprintf(
			`{"name":"shared","version":%q,"capabilities":["c"],"schema":{}}`, ver,
		))
	}
	sources := []SourceConfig{
		{Name: "a", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "b", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "c", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	snap, shadows, err := BuildEffective(context.Background(), fakeFs, "/data", sources, time.Now(), nil)
	if err != nil {
		t.Fatalf("BuildEffective: %v", err)
	}
	if snap.Len() != 1 {
		t.Fatalf("expected 1 winner, got %d", snap.Len())
	}
	got, _ := snap.Lookup("shared")
	if got.Source != "a" || got.Manifest.Version != "1.0" {
		t.Errorf("winner: got Source=%q Version=%q, want a/1.0", got.Source, got.Manifest.Version)
	}
	if len(shadows) != 2 {
		t.Fatalf("expected 2 shadow entries, got %d (%+v)", len(shadows), shadows)
	}
	for i, want := range []ShadowedTool{
		{ToolName: "shared", WinnerSource: "a", WinnerVersion: "1.0", ShadowedSource: "b", ShadowedVersion: "2.0"},
		{ToolName: "shared", WinnerSource: "a", WinnerVersion: "1.0", ShadowedSource: "c", ShadowedVersion: "3.0"},
	} {
		if shadows[i] != want {
			t.Errorf("shadows[%d]: got %+v, want %+v", i, shadows[i], want)
		}
	}
}

func TestBuildEffective_NoShadowsWhenNoConflicts(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	for i, src := range []string{"a", "b"} {
		parent := filepath.Join("/data", "tools", src)
		toolName := fmt.Sprintf("tool_%d", i)
		fakeFs.dirEntries[parent] = []fs.DirEntry{fakeDirEntry{name: toolName, isDir: true}}
		fakeFs.files[filepath.Join(parent, toolName, "manifest.json")] = []byte(fmt.Sprintf(
			`{"name":%q,"version":"1.0","capabilities":["c"],"schema":{}}`, toolName,
		))
	}
	sources := []SourceConfig{
		{Name: "a", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "b", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	snap, shadows, err := BuildEffective(context.Background(), fakeFs, "/data", sources, time.Now(), nil)
	if err != nil {
		t.Fatalf("BuildEffective: %v", err)
	}
	if snap.Len() != 2 {
		t.Errorf("expected 2 tools, got %d", snap.Len())
	}
	if len(shadows) != 0 {
		t.Errorf("expected 0 shadows, got %+v", shadows)
	}
}

// TestBuildEffective_IntraSourceDuplicateNotSurfacedAsShadow — when
// two per-tool subdirectories within the SAME source declare the
// same manifest name, the [ScanSourceDir] intra-source dedupe drops
// the loser BEFORE the precedence-flattening loop sees it. The shadow
// list captures only CROSS-source conflicts; intra-source duplicates
// land in the scanner log via [ErrIntraSourceDuplicateManifestName].
func TestBuildEffective_IntraSourceDuplicateNotSurfacedAsShadow(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	parent := filepath.Join("/data", "tools", "src")
	fakeFs.dirEntries[parent] = []fs.DirEntry{
		fakeDirEntry{name: "a_winner", isDir: true},
		fakeDirEntry{name: "b_loser", isDir: true},
	}
	fakeFs.files[filepath.Join(parent, "a_winner", "manifest.json")] = []byte(
		`{"name":"dup","version":"1","capabilities":["c"],"schema":{}}`,
	)
	fakeFs.files[filepath.Join(parent, "b_loser", "manifest.json")] = []byte(
		`{"name":"dup","version":"2","capabilities":["c"],"schema":{}}`,
	)
	sources := []SourceConfig{{Name: "src", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot}}
	logger := &fakeLogger{}
	snap, shadows, err := BuildEffective(context.Background(), fakeFs, "/data", sources, time.Now(), logger)
	if err != nil {
		t.Fatalf("BuildEffective: %v", err)
	}
	if snap.Len() != 1 {
		t.Errorf("expected 1 tool after intra-source dedupe, got %d", snap.Len())
	}
	if len(shadows) != 0 {
		t.Errorf("intra-source dup must not appear as shadow, got %+v", shadows)
	}
}

func TestRecompute_EmitsShadowedBeforeUpdated(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "a", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "b", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	r, fakeFs, _, pub := newTestRegistry(t, sources)
	writeManifest(fakeFs, "/data", "a", "shared", "1.0")
	writeManifest(fakeFs, "/data", "b", "shared", "2.0")

	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}

	events := pub.snapshot()
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(events))
	}
	// First event MUST be tool_shadowed; LAST event MUST be
	// effective_toolset_updated. This ordering lets a subscriber
	// learn about shadows BEFORE seeing the revision bump.
	if events[0].topic != TopicToolShadowed {
		t.Errorf("events[0].topic: got %q, want %q", events[0].topic, TopicToolShadowed)
	}
	if events[len(events)-1].topic != TopicEffectiveToolsetUpdated {
		t.Errorf("events[last].topic: got %q, want %q",
			events[len(events)-1].topic, TopicEffectiveToolsetUpdated)
	}
}

func TestRecompute_ShadowedPayloadFields(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "winner_src", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "loser_src", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	r, fakeFs, _, pub := newTestRegistry(t, sources)
	writeManifest(fakeFs, "/data", "winner_src", "t", "1.2.3")
	writeManifest(fakeFs, "/data", "loser_src", "t", "0.0.1")

	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	events := pub.eventsForTopic(TopicToolShadowed)
	if len(events) != 1 {
		t.Fatalf("expected 1 tool_shadowed event, got %d", len(events))
	}
	ev := events[0].event.(ToolShadowed)
	if ev.ToolName != "t" {
		t.Errorf("ToolName: got %q, want t", ev.ToolName)
	}
	if ev.WinnerSource != "winner_src" || ev.WinnerVersion != "1.2.3" {
		t.Errorf("winner: got %q/%q, want winner_src/1.2.3", ev.WinnerSource, ev.WinnerVersion)
	}
	if ev.ShadowedSource != "loser_src" || ev.ShadowedVersion != "0.0.1" {
		t.Errorf("shadowed: got %q/%q, want loser_src/0.0.1", ev.ShadowedSource, ev.ShadowedVersion)
	}
	if ev.Revision != 1 {
		t.Errorf("Revision: got %d, want 1", ev.Revision)
	}
	if ev.BuiltAt.IsZero() {
		t.Error("BuiltAt: zero value (should be the clock's Now)")
	}
	if ev.CorrelationID == "" {
		t.Error("CorrelationID: empty (should match the updated-event correlation id)")
	}

	// CorrelationID joins to the effective_toolset_updated event so a
	// subscriber observing both streams can reconcile shadow + updated
	// on the same recompute cycle.
	updEvents := pub.eventsForTopic(TopicEffectiveToolsetUpdated)
	if len(updEvents) != 1 {
		t.Fatalf("expected 1 effective_toolset_updated event, got %d", len(updEvents))
	}
	upd := updEvents[0].event.(EffectiveToolsetUpdated)
	if ev.CorrelationID != upd.CorrelationID {
		t.Errorf("CorrelationID mismatch: shadow=%q updated=%q", ev.CorrelationID, upd.CorrelationID)
	}
}

func TestRecompute_NoShadowsNoShadowEvent(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "a", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "b", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	r, fakeFs, _, pub := newTestRegistry(t, sources)
	writeManifest(fakeFs, "/data", "a", "t1", "1")
	writeManifest(fakeFs, "/data", "b", "t2", "1")

	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	if events := pub.eventsForTopic(TopicToolShadowed); len(events) != 0 {
		t.Errorf("expected 0 tool_shadowed events, got %d (%+v)", len(events), events)
	}
}

// TestRecompute_ShadowPublishFailureWrapsSentinel — when shadow
// publish fails but effective_toolset_updated publish succeeds, the
// returned error MUST wrap [ErrPublishToolShadowed] so external
// monitoring can detect "the registry committed the snapshot AND
// broadcasted the bump, but missed one or more shadow DMs".
func TestRecompute_ShadowPublishFailureWrapsSentinel(t *testing.T) {
	t.Parallel()
	// Custom publisher fails ONLY on the shadow topic.
	pub := &topicFilteredPublisher{failTopic: TopicToolShadowed, errToReturn: errSentinel}
	deps := RegistryDeps{
		FS:          newFakeFS(),
		DataDir:     "/data",
		Clock:       newFakeClock(time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)),
		GracePeriod: 100 * time.Millisecond,
	}
	sources := []SourceConfig{
		{Name: "a", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "b", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	r, err := NewRegistry(deps, sources, WithRegistryPublisher(pub))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	fakeFs := deps.FS.(*fakeFS)
	writeManifest(fakeFs, "/data", "a", "shared", "1")
	writeManifest(fakeFs, "/data", "b", "shared", "2")

	_, err = r.Recompute(context.Background())
	if !errors.Is(err, ErrPublishToolShadowed) {
		t.Fatalf("expected ErrPublishToolShadowed, got %v", err)
	}
	if !errors.Is(err, errSentinel) {
		t.Errorf("expected chain through errSentinel, got %v", err)
	}
	// The swap committed regardless of the publish failure — the new
	// snapshot is observable via Snapshot().
	if r.Snapshot().Len() != 1 {
		t.Errorf("snapshot must reflect the successful build, got Len=%d", r.Snapshot().Len())
	}
}

// TestRecompute_EffectiveUpdatedFailureOverridesShadowFailure — when
// BOTH the shadow publish AND the effective_toolset_updated publish
// fail, the returned error wraps [ErrPublishAfterSwap] rather than
// [ErrPublishToolShadowed]. The toolset-updated failure is the more
// critical signal — subscribers that only observe
// `effective_toolset_updated` would otherwise miss the revision bump
// entirely, while a missed `tool_shadowed` only loses one DM.
func TestRecompute_EffectiveUpdatedFailureOverridesShadowFailure(t *testing.T) {
	t.Parallel()
	pub := &topicFilteredPublisher{failTopic: "", errToReturn: errSentinel} // fail every topic
	deps := RegistryDeps{
		FS:          newFakeFS(),
		DataDir:     "/data",
		Clock:       newFakeClock(time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)),
		GracePeriod: 100 * time.Millisecond,
	}
	sources := []SourceConfig{
		{Name: "a", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "b", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	r, err := NewRegistry(deps, sources, WithRegistryPublisher(pub))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	fakeFs := deps.FS.(*fakeFS)
	writeManifest(fakeFs, "/data", "a", "shared", "1")
	writeManifest(fakeFs, "/data", "b", "shared", "2")

	_, err = r.Recompute(context.Background())
	if !errors.Is(err, ErrPublishAfterSwap) {
		t.Fatalf("expected ErrPublishAfterSwap precedence, got %v", err)
	}
	if errors.Is(err, ErrPublishToolShadowed) {
		t.Error("ErrPublishToolShadowed must NOT chain when ErrPublishAfterSwap wins")
	}
}

// TestPlatformReleaseIntegration_PrivateWinsPlatformShadowedEventFires
// is the M9.2 platform-release integration scenario from the roadmap:
// a customer's `private` source ships a tool, the platform's
// `main` later carries the same name, the registry must still
// resolve to private (private is listed FIRST in the priority list)
// AND emit the `tool_shadowed` event with the roadmap-text DM.
func TestPlatformReleaseIntegration_PrivateWinsPlatformShadowedEventFires(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		// Private listed FIRST — higher priority.
		{Name: "private", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		// Platform listed SECOND — lower priority, gets shadowed
		// when it ships a same-name tool.
		{Name: "platform", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	r, fakeFs, _, pub := newTestRegistry(t, sources)

	// Stage 1: private ships count_open_prs v0.4.1 alone.
	writeManifest(fakeFs, "/data", "private", "count_open_prs", "0.4.1")
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute stage 1: %v", err)
	}
	if len(pub.eventsForTopic(TopicToolShadowed)) != 0 {
		t.Fatal("stage 1: expected 0 tool_shadowed events (no conflict yet)")
	}

	// Stage 2: simulate a platform `main` release — platform now
	// carries the same name with a newer version v1.2.0.
	writeManifest(fakeFs, "/data", "platform", "count_open_prs", "1.2.0")
	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute stage 2: %v", err)
	}

	// Private still wins.
	got, ok := r.Snapshot().Lookup("count_open_prs")
	if !ok {
		t.Fatal("count_open_prs missing from snapshot")
	}
	if got.Source != "private" || got.Manifest.Version != "0.4.1" {
		t.Errorf("private must take precedence: got Source=%q Version=%q, want private/0.4.1",
			got.Source, got.Manifest.Version)
	}

	// One shadow event fired with platform as the loser.
	shadowEvents := pub.eventsForTopic(TopicToolShadowed)
	if len(shadowEvents) != 1 {
		t.Fatalf("expected 1 tool_shadowed event, got %d", len(shadowEvents))
	}
	ev := shadowEvents[0].event.(ToolShadowed)
	if ev.ToolName != "count_open_prs" {
		t.Errorf("ToolName: got %q, want count_open_prs", ev.ToolName)
	}
	if ev.WinnerSource != "private" || ev.WinnerVersion != "0.4.1" {
		t.Errorf("winner: got %q/%q, want private/0.4.1", ev.WinnerSource, ev.WinnerVersion)
	}
	if ev.ShadowedSource != "platform" || ev.ShadowedVersion != "1.2.0" {
		t.Errorf("shadowed: got %q/%q, want platform/1.2.0", ev.ShadowedSource, ev.ShadowedVersion)
	}

	// DM message must match the roadmap-text shape exactly. The
	// concrete wording is fixed on [ToolShadowed.Message] so a future
	// re-wording lands in one place.
	want := "platform now ships `count_open_prs` 1.2.0; private's `count_open_prs` 0.4.1 takes precedence. Review?"
	if got := ev.Message(); got != want {
		t.Errorf("Message: got %q, want %q", got, want)
	}
}

// TestPIIRedactionCanary_ToolShadowedPayload — the [ToolShadowed]
// payload carries ToolName + both VERSIONS (operator-facing
// identifiers) but MUST NOT leak the manifest's `Capabilities` /
// `Schema` bodies. A subscriber logging `fmt.Sprintf("%+v", ev)`
// verbatim must never expose the canary capability id.
func TestPIIRedactionCanary_ToolShadowedPayload(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "a", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "b", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	r, fakeFs, _, pub := newTestRegistry(t, sources)
	// Both sources carry the same tool name with the canary capability.
	writeManifest(fakeFs, "/data", "a", "shared", "1", canaryShadowedCapability)
	writeManifest(fakeFs, "/data", "b", "shared", "2", canaryShadowedCapability)

	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	events := pub.eventsForTopic(TopicToolShadowed)
	if len(events) != 1 {
		t.Fatalf("expected 1 tool_shadowed event, got %d", len(events))
	}
	body := fmt.Sprintf("%+v", events[0].event)
	if strings.Contains(body, canaryShadowedCapability) {
		t.Errorf("tool_shadowed payload leaked capability id: %q", body)
	}
}

// TestToolShadowed_FieldAllowlist — reflection-based AC pinning the
// [ToolShadowed] payload to a fixed 8-field allowlist. Adding a new
// field (e.g. `Capabilities []string`) breaks this test at compile
// time — forcing the author to deliberate the PII discipline before
// the field lands. Mirrors the
// [TestEffectiveToolsetUpdated_FieldAllowlist] discipline.
func TestToolShadowed_FieldAllowlist(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"ToolName":        "string",
		"WinnerSource":    "string",
		"WinnerVersion":   "string",
		"ShadowedSource":  "string",
		"ShadowedVersion": "string",
		"Revision":        "int64",
		"BuiltAt":         "time.Time",
		"CorrelationID":   "string",
	}
	typ := reflect.TypeOf(ToolShadowed{})
	if typ.NumField() != len(want) {
		t.Errorf("ToolShadowed has %d fields; expected %d (allowlist drift — review PII discipline before adding)",
			typ.NumField(), len(want))
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		wantType, ok := want[f.Name]
		if !ok {
			t.Errorf("unexpected ToolShadowed field %q (type %s) — add to allowlist + PII review", f.Name, f.Type)
			continue
		}
		if f.Type.String() != wantType {
			t.Errorf("ToolShadowed.%s: type %s, want %s", f.Name, f.Type, wantType)
		}
	}
}

// topicFilteredPublisher is a hand-rolled [Publisher] that returns
// `errToReturn` on Publish calls matching `failTopic` (empty
// `failTopic` matches every topic). All publishes are still recorded
// so tests can inspect the sequence after the call. Concurrency-safe
// via `mu` because some tests dispatch [Registry.Recompute] from
// multiple goroutines.
type topicFilteredPublisher struct {
	mu          sync.Mutex
	failTopic   string
	errToReturn error
	events      []publishedEvent
	// onPublish lets a specific test inject a side effect (e.g.
	// cancel a ctx mid-loop). Called BEFORE the failure check.
	onPublish func(topic string, event any)
}

func (p *topicFilteredPublisher) Publish(_ context.Context, topic string, event any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.onPublish != nil {
		p.onPublish(topic, event)
	}
	p.events = append(p.events, publishedEvent{topic: topic, event: event})
	if p.failTopic == "" || topic == p.failTopic {
		return p.errToReturn
	}
	return nil
}

func (p *topicFilteredPublisher) eventsForTopic(topic string) []publishedEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []publishedEvent
	for _, e := range p.events {
		if e.topic == topic {
			out = append(out, e)
		}
	}
	return out
}

// TestShadowedTool_FieldAllowlist mirrors
// [TestToolShadowed_FieldAllowlist] on the [ShadowedTool] half of
// the pair. Drift on either struct fails its allowlist at test
// time; combined with [newToolShadowedEvent] centralising the
// mapping, a future field addition forces a deliberate update on
// both sides.
func TestShadowedTool_FieldAllowlist(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"ToolName":        "string",
		"WinnerSource":    "string",
		"WinnerVersion":   "string",
		"ShadowedSource":  "string",
		"ShadowedVersion": "string",
	}
	typ := reflect.TypeOf(ShadowedTool{})
	if typ.NumField() != len(want) {
		t.Errorf("ShadowedTool has %d fields; expected %d (allowlist drift — update newToolShadowedEvent + ToolShadowed in lockstep)",
			typ.NumField(), len(want))
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		wantType, ok := want[f.Name]
		if !ok {
			t.Errorf("unexpected ShadowedTool field %q (type %s) — also update ToolShadowed + newToolShadowedEvent",
				f.Name, f.Type)
			continue
		}
		if f.Type.String() != wantType {
			t.Errorf("ShadowedTool.%s: type %s, want %s", f.Name, f.Type, wantType)
		}
	}
}

// TestToolShadowed_MessageScrubsInjection — finding #3 from iter-1.
// A hostile authoring pipeline (M9.4 will land AI-authored tools)
// could embed backticks, newlines, or Slack mention syntax in
// [Manifest.Name] / [Manifest.Version] to break out of the DM
// inline-code span or forge mentions. [ToolShadowed.Message] strips
// the dangerous characters from the dynamic parts via
// [dmInjectionScrubber] so the output keeps EXACTLY the template's
// own formatting (two backtick pairs around the tool name; no
// injected newlines / mentions / tags).
func TestToolShadowed_MessageScrubsInjection(t *testing.T) {
	t.Parallel()
	ev := ToolShadowed{
		ToolName:        "count`\n<@U_VICTIM>",
		WinnerSource:    "private\n>blockquote",
		WinnerVersion:   "0.4.1`",
		ShadowedSource:  "plat`form",
		ShadowedVersion: "1.2.0\n!channel",
	}
	got := ev.Message()
	// The template itself emits exactly 4 backticks (two pairs
	// around the tool name). Anything more means an unscrubbed
	// backtick leaked from the dynamic part.
	if n := strings.Count(got, "`"); n != 4 {
		t.Errorf("Message() backtick count: got %d, want 4 (injection leaked): %q", n, got)
	}
	// Newlines / tabs / Slack mention sigils are NEVER part of the
	// template — any occurrence is an injection leak.
	forbidden := []string{"\n", "\r", "\t", "<", ">", "&", "|"}
	for _, ch := range forbidden {
		if strings.Contains(got, ch) {
			t.Errorf("Message() output contains forbidden injection char %q: %q", ch, got)
		}
	}
}

// TestRecompute_NoPublisher_LogsShadows — finding #4 from iter-1.
// When [Registry] is constructed without [Publisher], shadows MUST
// NOT be silently dropped: each conflict surfaces via the optional
// [Logger] with the same tool_name / winner / shadowed metadata the
// event payload would have carried. M9.4 dry-run / M9.5 local-patch
// callers depending on the registry's atomic-swap pipeline get
// visibility even without bus wiring.
func TestRecompute_NoPublisher_LogsShadows(t *testing.T) {
	t.Parallel()
	logger := &fakeLogger{}
	deps := RegistryDeps{
		FS:          newFakeFS(),
		DataDir:     "/data",
		Clock:       newFakeClock(time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)),
		GracePeriod: 100 * time.Millisecond,
	}
	sources := []SourceConfig{
		{Name: "a", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "b", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	r, err := NewRegistry(deps, sources, WithRegistryLogger(logger)) // NO publisher option
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	fakeFs := deps.FS.(*fakeFS)
	writeManifest(fakeFs, "/data", "a", "shared", "1.0.0")
	writeManifest(fakeFs, "/data", "b", "shared", "2.0.0")

	if _, err := r.Recompute(context.Background()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}

	// Two log entries: one summary, one per-shadow detail.
	var sawSummary, sawDetail bool
	for _, e := range logger.snapshot() {
		switch e.msg {
		case "toolregistry: shadows dropped (no publisher wired)":
			sawSummary = true
		case "toolregistry: shadow detected":
			sawDetail = true
			// Detail must mention winner + shadowed source names.
			body := fmt.Sprintf("%v", e.kv)
			if !strings.Contains(body, "a") || !strings.Contains(body, "b") {
				t.Errorf("shadow detail log missing source identifiers: %v", e.kv)
			}
		}
	}
	if !sawSummary {
		t.Error("expected 'shadows dropped' summary log entry")
	}
	if !sawDetail {
		t.Error("expected 'shadow detected' per-shadow log entry")
	}
}

// TestBuildEffective_FourSourceThreeConflictDetectionOrder — finding
// #12 from iter-1. Four sources A B C D; B + D shadow A for tool
// `t1`, C + D shadow A for `t2`. The shadow list iteration order
// must be deterministic (by source-then-manifest within source) so
// downstream UI / DM dispatch can rely on stable presentation.
func TestBuildEffective_FourSourceThreeConflictDetectionOrder(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	// a: t1 v1, t2 v1
	// b: t1 v2 (shadows a/t1)
	// c: t2 v2 (shadows a/t2)
	// d: t1 v3 + t2 v3 (shadows a/t1 again, shadows a/t2 again)
	for _, src := range []string{"a", "b", "c", "d"} {
		parent := filepath.Join("/data", "tools", src)
		switch src {
		case "a":
			fakeFs.dirEntries[parent] = []fs.DirEntry{
				fakeDirEntry{name: "t1", isDir: true},
				fakeDirEntry{name: "t2", isDir: true},
			}
			fakeFs.files[filepath.Join(parent, "t1", "manifest.json")] = []byte(
				`{"name":"t1","version":"1","capabilities":["c"],"schema":{}}`,
			)
			fakeFs.files[filepath.Join(parent, "t2", "manifest.json")] = []byte(
				`{"name":"t2","version":"1","capabilities":["c"],"schema":{}}`,
			)
		case "b":
			fakeFs.dirEntries[parent] = []fs.DirEntry{fakeDirEntry{name: "t1", isDir: true}}
			fakeFs.files[filepath.Join(parent, "t1", "manifest.json")] = []byte(
				`{"name":"t1","version":"2","capabilities":["c"],"schema":{}}`,
			)
		case "c":
			fakeFs.dirEntries[parent] = []fs.DirEntry{fakeDirEntry{name: "t2", isDir: true}}
			fakeFs.files[filepath.Join(parent, "t2", "manifest.json")] = []byte(
				`{"name":"t2","version":"2","capabilities":["c"],"schema":{}}`,
			)
		case "d":
			fakeFs.dirEntries[parent] = []fs.DirEntry{
				fakeDirEntry{name: "t1", isDir: true},
				fakeDirEntry{name: "t2", isDir: true},
			}
			fakeFs.files[filepath.Join(parent, "t1", "manifest.json")] = []byte(
				`{"name":"t1","version":"3","capabilities":["c"],"schema":{}}`,
			)
			fakeFs.files[filepath.Join(parent, "t2", "manifest.json")] = []byte(
				`{"name":"t2","version":"3","capabilities":["c"],"schema":{}}`,
			)
		}
	}
	sources := []SourceConfig{
		{Name: "a", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "b", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "c", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "d", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	_, shadows, err := BuildEffective(context.Background(), fakeFs, "/data", sources, time.Now(), nil)
	if err != nil {
		t.Fatalf("BuildEffective: %v", err)
	}
	// Iteration order: a (winner of t1+t2), then b (shadows a/t1),
	// then c (shadows a/t2), then d (shadows a/t1 + a/t2 alpha-
	// sorted by ScanSourceDir = t1 first, then t2).
	want := []ShadowedTool{
		{ToolName: "t1", WinnerSource: "a", WinnerVersion: "1", ShadowedSource: "b", ShadowedVersion: "2"},
		{ToolName: "t2", WinnerSource: "a", WinnerVersion: "1", ShadowedSource: "c", ShadowedVersion: "2"},
		{ToolName: "t1", WinnerSource: "a", WinnerVersion: "1", ShadowedSource: "d", ShadowedVersion: "3"},
		{ToolName: "t2", WinnerSource: "a", WinnerVersion: "1", ShadowedSource: "d", ShadowedVersion: "3"},
	}
	if len(shadows) != len(want) {
		t.Fatalf("len(shadows): got %d, want %d (%+v)", len(shadows), len(want), shadows)
	}
	for i, w := range want {
		if shadows[i] != w {
			t.Errorf("shadows[%d]: got %+v, want %+v", i, shadows[i], w)
		}
	}
}

// TestRecompute_CtxCancelMidShadowEmit — finding #13 from iter-1.
// A ctx-cancel between shadow K and K+1 must not abort the loop —
// Recompute attempts every publish even when an earlier one failed
// (partial-delivery semantics, lesson Pattern #4). The FIRST
// failure is wrapped via [ErrPublishToolShadowed]; subsequent
// failures land in the logger.
func TestRecompute_CtxCancelMidShadowEmit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	pub := &topicFilteredPublisher{
		failTopic:   TopicToolShadowed,
		errToReturn: context.Canceled,
		// Cancel the ctx on the FIRST shadow publish so any
		// subsequent ones see a cancelled ctx too.
		onPublish: func(topic string, _ any) {
			if topic == TopicToolShadowed {
				cancel()
			}
		},
	}
	logger := &fakeLogger{}
	deps := RegistryDeps{
		FS:          newFakeFS(),
		DataDir:     "/data",
		Clock:       newFakeClock(time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)),
		GracePeriod: 100 * time.Millisecond,
	}
	// Three sources, all conflicting on a single name → two
	// shadow publishes, both should be attempted even after cancel.
	sources := []SourceConfig{
		{Name: "a", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "b", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "c", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	r, err := NewRegistry(deps, sources, WithRegistryPublisher(pub), WithRegistryLogger(logger))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	fakeFs := deps.FS.(*fakeFS)
	writeManifest(fakeFs, "/data", "a", "shared", "1")
	writeManifest(fakeFs, "/data", "b", "shared", "2")
	writeManifest(fakeFs, "/data", "c", "shared", "3")

	_, err = r.Recompute(ctx)
	if !errors.Is(err, ErrPublishToolShadowed) {
		t.Fatalf("expected ErrPublishToolShadowed, got %v", err)
	}
	// Both shadow publishes were attempted despite the cancel.
	shadowEvents := pub.eventsForTopic(TopicToolShadowed)
	if len(shadowEvents) != 2 {
		t.Errorf("expected 2 shadow publish attempts, got %d", len(shadowEvents))
	}
}

// TestRecompute_ConcurrentRecomputesPreserveTopicFIFO — finding #14
// from iter-1. Two concurrent Recompute calls must not interleave
// each other's shadow events on the same topic. [Registry.publishMu]
// serialises the publish phase so revision N's events all land on
// the bus before revision N+1's, preserving the per-topic FIFO
// contract subscribers expect.
func TestRecompute_ConcurrentRecomputesPreserveTopicFIFO(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "a", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "b", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "c", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	r, fakeFs, _, pub := newTestRegistry(t, sources)
	// Three sources, two shadows per Recompute.
	writeManifest(fakeFs, "/data", "a", "shared", "1")
	writeManifest(fakeFs, "/data", "b", "shared", "2")
	writeManifest(fakeFs, "/data", "c", "shared", "3")

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, err := r.Recompute(context.Background()); err != nil {
				t.Errorf("Recompute: %v", err)
			}
		}()
	}
	wg.Wait()

	// For each revision N, all shadow events for N must appear in
	// the publisher's recorded sequence BEFORE the toolset-updated
	// event for N. publishMu enforces this even though the
	// scan+swap was concurrent.
	events := pub.snapshot()
	revToUpdatedIdx := map[int64]int{}
	revToFirstShadowIdx := map[int64]int{}
	for i, e := range events {
		switch ev := e.event.(type) {
		case ToolShadowed:
			if _, ok := revToFirstShadowIdx[ev.Revision]; !ok {
				revToFirstShadowIdx[ev.Revision] = i
			}
		case EffectiveToolsetUpdated:
			revToUpdatedIdx[ev.Revision] = i
		}
	}
	for rev, updatedIdx := range revToUpdatedIdx {
		firstShadowIdx, ok := revToFirstShadowIdx[rev]
		if !ok {
			t.Errorf("revision %d has updated event but no shadow events", rev)
			continue
		}
		if firstShadowIdx > updatedIdx {
			t.Errorf("revision %d: shadow event after updated event (publishMu broken)", rev)
		}
		// Verify no OTHER revision's events appear between
		// shadow_first(rev) and updated(rev).
		for i := firstShadowIdx; i <= updatedIdx; i++ {
			switch ev := events[i].event.(type) {
			case ToolShadowed:
				if ev.Revision != rev {
					t.Errorf("revision %d emit window contains revision %d shadow at idx %d",
						rev, ev.Revision, i)
				}
			case EffectiveToolsetUpdated:
				if ev.Revision != rev {
					t.Errorf("revision %d emit window contains revision %d updated at idx %d",
						rev, ev.Revision, i)
				}
			}
		}
	}
}

// TestRecompute_ReShadowAcrossRecomputes_RevisionMonotonic — finding
// #15 from iter-1. The SAME (ToolName, ShadowedSource, ShadowedVersion)
// shadow fires on every subsequent Recompute that re-observes the
// conflict, with monotonically increasing Revision. Subscribers
// gate "DM the lead exactly once" on a max-seen revision per
// (ToolName, ShadowedSource).
func TestRecompute_ReShadowAcrossRecomputes_RevisionMonotonic(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "a", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "b", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	r, fakeFs, _, pub := newTestRegistry(t, sources)
	writeManifest(fakeFs, "/data", "a", "shared", "1")
	writeManifest(fakeFs, "/data", "b", "shared", "2")

	for i := 1; i <= 3; i++ {
		if _, err := r.Recompute(context.Background()); err != nil {
			t.Fatalf("Recompute %d: %v", i, err)
		}
	}
	shadowEvents := pub.eventsForTopic(TopicToolShadowed)
	if len(shadowEvents) != 3 {
		t.Fatalf("expected 3 shadow events (one per recompute), got %d", len(shadowEvents))
	}
	for i, e := range shadowEvents {
		ev := e.event.(ToolShadowed)
		wantRev := int64(i + 1)
		if ev.Revision != wantRev {
			t.Errorf("shadowEvents[%d].Revision: got %d, want %d", i, ev.Revision, wantRev)
		}
		if ev.ToolName != "shared" || ev.ShadowedSource != "b" {
			t.Errorf("shadowEvents[%d]: unexpected metadata %+v", i, ev)
		}
	}
}
