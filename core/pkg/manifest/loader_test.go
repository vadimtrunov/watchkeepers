package manifest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// fakeFetcher is a hand-rolled [ManifestFetcher] for tests. It records the
// number of calls so the empty-manifestID guard can assert the fetcher is
// never reached, and lets each test inject the response or error verbatim.
type fakeFetcher struct {
	calls    int
	response *keepclient.ManifestVersion
	err      error
}

func (f *fakeFetcher) GetManifest(_ context.Context, _ string) (*keepclient.ManifestVersion, error) {
	f.calls++
	return f.response, f.err
}

// TestLoadManifest_TemplatesPersonalityAndLanguage exercises the happy path
// (AC1, AC2, AC3): both Personality and Language are non-empty, the
// SystemPrompt is composed in the documented order, and AgentID round-trips
// from ManifestVersion.ManifestID.
func TestLoadManifest_TemplatesPersonalityAndLanguage(t *testing.T) {
	t.Parallel()

	const manifestID = "11111111-1111-4111-8111-111111111111"
	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ID:           "row-1",
		ManifestID:   manifestID,
		SystemPrompt: "You are X.",
		Personality:  "concise",
		Language:     "en",
	}}

	got, err := LoadManifest(context.Background(), f, manifestID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	const wantPrompt = "You are X.\n\nPersonality: concise\nLanguage: en"
	if got.SystemPrompt != wantPrompt {
		t.Errorf("SystemPrompt = %q, want %q", got.SystemPrompt, wantPrompt)
	}
	if got.AgentID != manifestID {
		t.Errorf("AgentID = %q, want %q", got.AgentID, manifestID)
	}
	if got.Personality != "concise" {
		t.Errorf("Personality = %q, want %q", got.Personality, "concise")
	}
	if got.Language != "en" {
		t.Errorf("Language = %q, want %q", got.Language, "en")
	}
	if f.calls != 1 {
		t.Errorf("fetcher calls = %d, want 1", f.calls)
	}
}

// TestLoadManifest_PersonalityOnly asserts that an empty Language emits no
// `\nLanguage:` line and the suffix terminates after the Personality line
// (AC2 — empty Language produces no orphan header).
func TestLoadManifest_PersonalityOnly(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "You are X.",
		Personality:  "concise",
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	const wantPrompt = "You are X.\n\nPersonality: concise"
	if got.SystemPrompt != wantPrompt {
		t.Errorf("SystemPrompt = %q, want %q", got.SystemPrompt, wantPrompt)
	}
	if got.Language != "" {
		t.Errorf("Language = %q, want empty", got.Language)
	}
}

// TestLoadManifest_LanguageOnly asserts the precedence rule from AC2: when
// Personality is empty but Language is non-empty, the suffix still emits
// `\n\nLanguage: <l>` — the leading blank line attaches to Language alone.
func TestLoadManifest_LanguageOnly(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "You are X.",
		Language:     "en",
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	const wantPrompt = "You are X.\n\nLanguage: en"
	if got.SystemPrompt != wantPrompt {
		t.Errorf("SystemPrompt = %q, want %q", got.SystemPrompt, wantPrompt)
	}
	if got.Personality != "" {
		t.Errorf("Personality = %q, want empty", got.Personality)
	}
}

// TestLoadManifest_NoPersonalityNoLanguage asserts that with both fields
// empty, SystemPrompt equals base verbatim — no trailing whitespace, no
// orphan headers (AC2 — all four combinations).
func TestLoadManifest_NoPersonalityNoLanguage(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "You are X.",
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	const wantPrompt = "You are X."
	if got.SystemPrompt != wantPrompt {
		t.Errorf("SystemPrompt = %q, want %q", got.SystemPrompt, wantPrompt)
	}
}

// TestLoadManifest_EmptyManifestID_RejectedBeforeFetch asserts AC4: an empty
// manifestID returns runtime.ErrInvalidManifest synchronously and the
// fetcher is never invoked.
func TestLoadManifest_EmptyManifestID_RejectedBeforeFetch(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{}

	_, err := LoadManifest(context.Background(), f, "")
	if !errors.Is(err, runtime.ErrInvalidManifest) {
		t.Errorf("err = %v, want errors.Is runtime.ErrInvalidManifest", err)
	}
	if f.calls != 0 {
		t.Errorf("fetcher calls = %d, want 0", f.calls)
	}
}

// TestLoadManifest_FetcherErrorPropagated asserts AC5: a fetcher error is
// wrapped via `fmt.Errorf("manifest: load: %w", err)` so callers can match
// the underlying sentinel (here keepclient.ErrNotFound) via errors.Is
// through the wrap.
func TestLoadManifest_FetcherErrorPropagated(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{err: fmt.Errorf("server: %w", keepclient.ErrNotFound)}

	_, err := LoadManifest(context.Background(), f, "m")
	if err == nil {
		t.Fatalf("LoadManifest: nil error, want wrapped ErrNotFound")
	}
	if !errors.Is(err, keepclient.ErrNotFound) {
		t.Errorf("err = %v, want errors.Is keepclient.ErrNotFound", err)
	}
}
