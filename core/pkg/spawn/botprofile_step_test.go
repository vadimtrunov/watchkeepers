package spawn_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// ────────────────────────────────────────────────────────────────────────
// Hand-rolled fakes (M3.6 / M6.3.e / M7.1.c.a-.b.b pattern — no mocking lib).
// ────────────────────────────────────────────────────────────────────────

// fakeBotProfileSetter records every SetBotProfile call onto a SHARED
// record set, optionally returns a configured error to drive negative
// paths. Concurrency: all mutable state lives behind a mutex / atomics
// so concurrent Execute() calls can drive the same fake without data
// races (`go test -race` clean).
type fakeBotProfileSetter struct {
	mu        sync.Mutex
	calls     []recordedSetBotProfileCall
	callCount atomic.Int32
	returnErr error
}

type recordedSetBotProfileCall struct {
	watchkeeperID uuid.UUID
	profile       messenger.BotProfile
}

func newFakeBotProfileSetter() *fakeBotProfileSetter {
	return &fakeBotProfileSetter{}
}

func (f *fakeBotProfileSetter) SetBotProfile(_ context.Context, watchkeeperID uuid.UUID, profile messenger.BotProfile) error {
	f.callCount.Add(1)
	f.mu.Lock()
	f.calls = append(f.calls, recordedSetBotProfileCall{watchkeeperID: watchkeeperID, profile: profile})
	f.mu.Unlock()
	return f.returnErr
}

func (f *fakeBotProfileSetter) recordedCalls() []recordedSetBotProfileCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedSetBotProfileCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// canonicalBotProfile returns the profile applied across the test
// suite. Mirrors a realistic Watchmaster-derived profile (DisplayName +
// StatusText + a Slack-specific Metadata key forwarded through the
// adapter's recognised-keys list).
func canonicalBotProfile() messenger.BotProfile {
	return messenger.BotProfile{
		DisplayName: "Coordinator",
		StatusText:  "watching backend team",
		Metadata: map[string]string{
			"status_emoji": ":eyes:",
			"title":        "Watchkeeper",
		},
	}
}

func newBotProfileSpawnContext(t *testing.T, watchkeeperID uuid.UUID) saga.SpawnContext {
	t.Helper()
	return saga.SpawnContext{
		ManifestVersionID: uuid.New(),
		AgentID:           watchkeeperID,
		Claim: saga.SpawnClaim{
			OrganizationID:  "org-test",
			AgentID:         "agent-watchmaster",
			AuthorityMatrix: map[string]string{"slack_app_create": "lead_approval"},
		},
	}
}

func newBotProfileStep(t *testing.T, setter spawn.BotProfileSetter, profile messenger.BotProfile) *spawn.BotProfileStep {
	t.Helper()
	return spawn.NewBotProfileStep(spawn.BotProfileStepDeps{
		Setter:  setter,
		Profile: profile,
	})
}

// ────────────────────────────────────────────────────────────────────────
// Compile-time + Name() coverage
// ────────────────────────────────────────────────────────────────────────

func TestBotProfileStep_Name_ReturnsStableIdentifier(t *testing.T) {
	t.Parallel()

	step := newBotProfileStep(t, newFakeBotProfileSetter(), canonicalBotProfile())
	if got := step.Name(); got != "bot_profile" {
		t.Errorf("Name() = %q, want %q", got, "bot_profile")
	}
	if got := step.Name(); got != spawn.BotProfileStepName {
		t.Errorf("Name() = %q, want %q (BotProfileStepName)", got, spawn.BotProfileStepName)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Constructor panics
// ────────────────────────────────────────────────────────────────────────

func TestNewBotProfileStep_PanicsOnNilSetter(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewBotProfileStep with nil Setter did not panic")
		}
	}()
	_ = spawn.NewBotProfileStep(spawn.BotProfileStepDeps{
		Setter:  nil,
		Profile: canonicalBotProfile(),
	})
}

// ────────────────────────────────────────────────────────────────────────
// Happy: Setter called once with watchkeeperID + profile; Execute returns nil
// ────────────────────────────────────────────────────────────────────────

func TestBotProfileStep_Execute_HappyPath_ForwardsWatchkeeperIDAndProfile(t *testing.T) {
	t.Parallel()

	setter := newFakeBotProfileSetter()
	profile := canonicalBotProfile()
	step := newBotProfileStep(t, setter, profile)

	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newBotProfileSpawnContext(t, watchkeeperID))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := setter.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Setter.SetBotProfile call count = %d, want 1", len(calls))
	}
	if calls[0].watchkeeperID != watchkeeperID {
		t.Errorf("call.watchkeeperID = %q, want %q", calls[0].watchkeeperID, watchkeeperID)
	}
	if calls[0].profile.DisplayName != profile.DisplayName {
		t.Errorf("call.profile.DisplayName = %q, want %q", calls[0].profile.DisplayName, profile.DisplayName)
	}
	if calls[0].profile.StatusText != profile.StatusText {
		t.Errorf("call.profile.StatusText = %q, want %q", calls[0].profile.StatusText, profile.StatusText)
	}
	if got := calls[0].profile.Metadata["status_emoji"]; got != ":eyes:" {
		t.Errorf("call.profile.Metadata[status_emoji] = %q, want :eyes:", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Edge: empty profile is permitted (messenger adapter no-ops on empty)
// ────────────────────────────────────────────────────────────────────────

func TestBotProfileStep_Execute_EmptyProfile_StillCallsSetter(t *testing.T) {
	t.Parallel()

	setter := newFakeBotProfileSetter()
	step := newBotProfileStep(t, setter, messenger.BotProfile{})

	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newBotProfileSpawnContext(t, watchkeeperID))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := setter.callCount.Load(); got != 1 {
		t.Errorf("Setter call count = %d, want 1 (empty profile is a documented no-op at the adapter; the step still dispatches)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Missing SpawnContext → wrapped error; no Setter call
// ────────────────────────────────────────────────────────────────────────

func TestBotProfileStep_Execute_MissingSpawnContext_NoSetterCall(t *testing.T) {
	t.Parallel()

	setter := newFakeBotProfileSetter()
	step := newBotProfileStep(t, setter, canonicalBotProfile())

	err := step.Execute(context.Background())
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingSpawnContext")
	}
	if !errors.Is(err, spawn.ErrMissingSpawnContext) {
		t.Errorf("errors.Is(err, ErrMissingSpawnContext) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: bot_profile step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: bot_profile step:")
	}
	if got := setter.callCount.Load(); got != 0 {
		t.Errorf("Setter call count = %d, want 0 (missing SpawnContext fails before dispatch)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: AgentID = uuid.Nil → wrapped ErrMissingAgentID; no Setter call
// ────────────────────────────────────────────────────────────────────────

func TestBotProfileStep_Execute_NilAgentID_NoSetterCall(t *testing.T) {
	t.Parallel()

	setter := newFakeBotProfileSetter()
	step := newBotProfileStep(t, setter, canonicalBotProfile())

	sc := saga.SpawnContext{
		ManifestVersionID: uuid.New(),
		AgentID:           uuid.Nil,
	}
	ctx := saga.WithSpawnContext(context.Background(), sc)

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingAgentID")
	}
	if !errors.Is(err, spawn.ErrMissingAgentID) {
		t.Errorf("errors.Is(err, ErrMissingAgentID) = false; got %v", err)
	}
	if got := setter.callCount.Load(); got != 0 {
		t.Errorf("Setter call count = %d, want 0", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Setter returns error → wrapped + returned
// ────────────────────────────────────────────────────────────────────────

func TestBotProfileStep_Execute_SetterError_WrapsAndReturns(t *testing.T) {
	t.Parallel()

	setterErr := errors.New("simulated platform failure")
	setter := newFakeBotProfileSetter()
	setter.returnErr = setterErr
	step := newBotProfileStep(t, setter, canonicalBotProfile())

	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newBotProfileSpawnContext(t, watchkeeperID))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped Setter error")
	}
	if !errors.Is(err, setterErr) {
		t.Errorf("errors.Is(err, setterErr) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: bot_profile step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: bot_profile step:")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Setter passthrough preserves typed sentinels (M7.1.c.a/.b.b
// "Reuse sentinel errors across saga steps" lesson — ErrCredsNotFound
// surfaces unchanged through the wrap chain).
// ────────────────────────────────────────────────────────────────────────

func TestBotProfileStep_Execute_PreservesCredsNotFoundSentinel(t *testing.T) {
	t.Parallel()

	setter := newFakeBotProfileSetter()
	setter.returnErr = spawn.ErrCredsNotFound
	step := newBotProfileStep(t, setter, canonicalBotProfile())

	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newBotProfileSpawnContext(t, watchkeeperID))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrCredsNotFound")
	}
	if !errors.Is(err, spawn.ErrCredsNotFound) {
		t.Errorf("errors.Is(err, ErrCredsNotFound) = false; got %v", err)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: ctx already cancelled → wrapped ctx.Err(); no Setter call
// ────────────────────────────────────────────────────────────────────────

func TestBotProfileStep_Execute_CancelledContext_NoSetterCall(t *testing.T) {
	t.Parallel()

	setter := newFakeBotProfileSetter()
	step := newBotProfileStep(t, setter, canonicalBotProfile())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx = saga.WithSpawnContext(ctx, newBotProfileSpawnContext(t, uuid.New()))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ctx.Err()")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; got %v", err)
	}
	if got := setter.callCount.Load(); got != 0 {
		t.Errorf("Setter call count = %d, want 0 (cancellation precedes dispatch)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Security: error strings do NOT leak profile content (DisplayName /
// StatusText / Metadata values) on any failure path. Mirrors
// M7.1.c.b.b's redaction harness.
// ────────────────────────────────────────────────────────────────────────

func TestBotProfileStep_Execute_ErrorPaths_DoNotLeakProfileContent(t *testing.T) {
	t.Parallel()

	//nolint:gosec // G101: synthetic redaction-harness canaries, not real credentials.
	const (
		secretDisplay  = "secret-display-NAMEXX"
		secretStatus   = "secret-status-CANARY"
		secretMetaKey  = "title"
		secretMetaVal  = "secret-title-LEAKxx"
		secretMetaKey2 = "status_emoji"
		secretMetaVal2 = ":SECRET-EMOJI:"
	)
	leakProfile := messenger.BotProfile{
		DisplayName: secretDisplay,
		StatusText:  secretStatus,
		Metadata: map[string]string{
			secretMetaKey:  secretMetaVal,
			secretMetaKey2: secretMetaVal2,
		},
	}
	leakSubstrings := []string{secretDisplay, secretStatus, secretMetaVal, secretMetaVal2}

	cases := []struct {
		name  string
		setup func() (step *spawn.BotProfileStep, ctx context.Context)
	}{
		{
			name: "setter error",
			setup: func() (*spawn.BotProfileStep, context.Context) {
				setter := newFakeBotProfileSetter()
				setter.returnErr = errors.New("platform fail")
				return newBotProfileStep(t, setter, leakProfile), saga.WithSpawnContext(
					context.Background(), newBotProfileSpawnContext(t, uuid.New()),
				)
			},
		},
		{
			name: "missing spawn context",
			setup: func() (*spawn.BotProfileStep, context.Context) {
				setter := newFakeBotProfileSetter()
				return newBotProfileStep(t, setter, leakProfile), context.Background()
			},
		},
		{
			name: "nil agent id",
			setup: func() (*spawn.BotProfileStep, context.Context) {
				setter := newFakeBotProfileSetter()
				sc := saga.SpawnContext{ManifestVersionID: uuid.New(), AgentID: uuid.Nil}
				return newBotProfileStep(t, setter, leakProfile), saga.WithSpawnContext(context.Background(), sc)
			},
		},
		{
			name: "setter returns ErrCredsNotFound",
			setup: func() (*spawn.BotProfileStep, context.Context) {
				setter := newFakeBotProfileSetter()
				setter.returnErr = spawn.ErrCredsNotFound
				return newBotProfileStep(t, setter, leakProfile), saga.WithSpawnContext(
					context.Background(), newBotProfileSpawnContext(t, uuid.New()),
				)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step, ctx := tc.setup()
			err := step.Execute(ctx)
			if err == nil {
				t.Fatalf("Execute: err = nil, want non-nil for %s", tc.name)
			}
			msg := err.Error()
			for _, secret := range leakSubstrings {
				if strings.Contains(msg, secret) {
					t.Errorf("error message %q contains secret substring %q (PII leak)", msg, secret)
				}
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────
// AC7: PII-safe audit — step source does NOT call keeperslog.Writer.Append
// ────────────────────────────────────────────────────────────────────────

// TestBotProfileStep_DoesNotCallKeepersLogAppend mirrors the
// M7.1.c.a / M7.1.c.b.b source-grep AC pin: it reads
// botprofile_step.go, strips pure comment lines, then asserts the
// non-comment source contains neither a "keeperslog." reference nor a
// ".Append(" call. Stronger than a runtime assertion because it
// catches any future edit that adds a keeperslog import or an Append
// call regardless of test wiring.
func TestBotProfileStep_DoesNotCallKeepersLogAppend(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("botprofile_step.go")
	if err != nil {
		t.Fatalf("read botprofile_step.go: %v", err)
	}

	var nonComment strings.Builder
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "//") {
			nonComment.WriteString(line)
			nonComment.WriteByte('\n')
		}
	}
	body := nonComment.String()

	if strings.Contains(body, "keeperslog.") {
		t.Errorf("botprofile_step.go references keeperslog in non-comment code — AC7 violated")
	}
	if strings.Contains(body, ".Append(") {
		t.Errorf("botprofile_step.go contains '.Append(' in non-comment code — AC7 violated")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Defensive copy: post-construction caller mutations of Metadata /
// AvatarPNG MUST NOT bleed into the Setter call (M7.1.c.c iter-1
// codex-review fix).
// ────────────────────────────────────────────────────────────────────────

func TestBotProfileStep_Execute_DefensiveCopiesProfile(t *testing.T) {
	t.Parallel()

	originalAvatar := []byte{0x89, 0x50, 0x4e, 0x47}
	originalMetadata := map[string]string{
		"status_emoji": ":eyes:",
		"title":        "Watchkeeper",
	}
	profile := messenger.BotProfile{
		DisplayName: "Coordinator",
		StatusText:  "watching",
		Metadata:    originalMetadata,
		AvatarPNG:   originalAvatar,
	}

	setter := newFakeBotProfileSetter()
	step := newBotProfileStep(t, setter, profile)

	// Mutate caller-side state AFTER construction. The step must
	// observe the construction-time snapshot, not the mutation.
	originalMetadata["title"] = "MUTATED-AFTER-CONSTRUCTION"
	originalMetadata["status_emoji"] = ":bomb:"
	originalAvatar[0] = 0xFF

	ctx := saga.WithSpawnContext(context.Background(), newBotProfileSpawnContext(t, uuid.New()))
	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := setter.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Setter call count = %d, want 1", len(calls))
	}
	got := calls[0].profile
	if got.Metadata["title"] != "Watchkeeper" {
		t.Errorf("Metadata[title] = %q, want %q (post-construction caller mutation bled into Setter call)",
			got.Metadata["title"], "Watchkeeper")
	}
	if got.Metadata["status_emoji"] != ":eyes:" {
		t.Errorf("Metadata[status_emoji] = %q, want %q", got.Metadata["status_emoji"], ":eyes:")
	}
	if len(got.AvatarPNG) == 0 || got.AvatarPNG[0] != 0x89 {
		t.Errorf("AvatarPNG[0] = %#x, want 0x89 (caller mutation must not bleed)", got.AvatarPNG)
	}

	// Setter-side mutation MUST NOT corrupt the step's stored profile
	// either (the step hands a fresh clone on every Execute).
	got.Metadata["title"] = "SETTER-MUTATION"
	got.AvatarPNG[0] = 0xAA

	setter2 := newFakeBotProfileSetter()
	step2 := step
	_ = step2
	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute (second call): %v", err)
	}
	_ = setter2
	calls2 := setter.recordedCalls()
	if len(calls2) != 2 {
		t.Fatalf("Setter call count = %d, want 2", len(calls2))
	}
	got2 := calls2[1].profile
	if got2.Metadata["title"] != "Watchkeeper" {
		t.Errorf("Metadata[title] on second call = %q, want %q (setter-side mutation corrupted step state)",
			got2.Metadata["title"], "Watchkeeper")
	}
	if got2.AvatarPNG[0] != 0x89 {
		t.Errorf("AvatarPNG[0] on second call = %#x, want 0x89", got2.AvatarPNG[0])
	}
}

// ────────────────────────────────────────────────────────────────────────
// Concurrency: 16 goroutines, distinct watchkeeperIDs, race-detector clean
// ────────────────────────────────────────────────────────────────────────

func TestBotProfileStep_Execute_Concurrency_DistinctWatchkeepers(t *testing.T) {
	t.Parallel()

	setter := newFakeBotProfileSetter()
	step := newBotProfileStep(t, setter, canonicalBotProfile())

	const n = 16
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			ctx := saga.WithSpawnContext(context.Background(), newBotProfileSpawnContext(t, id))
			if err := step.Execute(ctx); err != nil {
				t.Errorf("Execute(%v): %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	if got := setter.callCount.Load(); got != n {
		t.Errorf("Setter call count = %d, want %d", got, n)
	}
	calls := setter.recordedCalls()
	seen := make(map[uuid.UUID]bool, n)
	for _, c := range calls {
		seen[c.watchkeeperID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("watchkeeperID %v missing from recorded calls", id)
		}
	}
}
