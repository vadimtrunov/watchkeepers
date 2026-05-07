package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

// TestLoadManifest_ProjectsModel asserts M5.5.b.b.c AC2: a non-empty
// [keepclient.ManifestVersion.Model] copies verbatim onto
// [runtime.Manifest.Model] — no transformation, no default substitution.
func TestLoadManifest_ProjectsModel(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Model:        "claude-sonnet-4",
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.Model != "claude-sonnet-4" {
		t.Errorf("Model = %q, want %q", got.Model, "claude-sonnet-4")
	}
}

// TestLoadManifest_ModelEmptyString_ProjectsVerbatim asserts M5.5.b.b.c
// AC3: an empty [keepclient.ManifestVersion.Model] propagates as the empty
// string — the loader does NOT supply a default. The empty-Model rejection
// gate is downstream in [llm.composeBaseFields] (returns
// [llm.ErrInvalidManifest]); see [TestLoadManifest_ChainsThroughBuildCompleteRequest]
// for the round-trip proof.
func TestLoadManifest_ModelEmptyString_ProjectsVerbatim(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Model:        "",
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.Model != "" {
		t.Errorf("Model = %q, want empty (loader supplies no default)", got.Model)
	}
}

// TestLoadManifest_DecodesToolsetNames asserts the M5.5.b.a happy path
// (AC1): the loader decodes mv.Tools from `[{"name":"echo"},{"name":"sum"}]`
// into Toolset = ["echo", "sum"].
func TestLoadManifest_DecodesToolsetNames(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Tools:        json.RawMessage(`[{"name":"echo"},{"name":"sum"}]`),
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	want := []string{"echo", "sum"}
	if !reflect.DeepEqual(got.Toolset, want) {
		t.Errorf("Toolset = %v, want %v", got.Toolset, want)
	}
}

// TestLoadManifest_EmptyToolsArrayMeansNilToolset asserts AC1's empty-array
// branch: a `[]` jsonb yields a nil/empty Toolset on the runtime.Manifest
// (see runtime.go:99-103: "An empty / nil Toolset means 'no tools'").
func TestLoadManifest_EmptyToolsArrayMeansNilToolset(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Tools:        json.RawMessage(`[]`),
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(got.Toolset) != 0 {
		t.Errorf("Toolset = %v, want nil/empty", got.Toolset)
	}
}

// TestLoadManifest_NullToolsTreatedAsNilToolset asserts AC1's null branch:
// a JSON `null` jsonb (or an entirely absent column surfaced as
// json.RawMessage("null") / nil) yields a nil/empty Toolset.
func TestLoadManifest_NullToolsTreatedAsNilToolset(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Tools:        json.RawMessage(`null`),
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(got.Toolset) != 0 {
		t.Errorf("Toolset = %v, want nil/empty", got.Toolset)
	}
}

// TestLoadManifest_MalformedToolsRejected asserts AC2: a malformed Tools
// jsonb (here `[{"name":42}]`) returns an error wrapped with the
// `manifest: toolset:` prefix so callers can match the underlying
// json.Unmarshal failure mode.
func TestLoadManifest_MalformedToolsRejected(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Tools:        json.RawMessage(`[{"name":42}]`),
	}}

	_, err := LoadManifest(context.Background(), f, "m")
	if err == nil {
		t.Fatalf("LoadManifest: nil error, want wrapped toolset failure")
	}
	if !strings.Contains(err.Error(), "manifest: toolset:") {
		t.Errorf("err = %v, want substring %q", err, "manifest: toolset:")
	}
}

// TestLoadManifest_ProjectsAuthorityMatrix asserts the M5.5.b.c.c.a happy
// path (AC1): the loader decodes mv.AuthorityMatrix from
// `{"deploy":"lead","spawn":"watchmaster"}` into AuthorityMatrix =
// map[string]string{"deploy":"lead","spawn":"watchmaster"}.
func TestLoadManifest_ProjectsAuthorityMatrix(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:      "m",
		SystemPrompt:    "x",
		AuthorityMatrix: json.RawMessage(`{"deploy":"lead","spawn":"watchmaster"}`),
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	want := map[string]string{"deploy": "lead", "spawn": "watchmaster"}
	if !reflect.DeepEqual(got.AuthorityMatrix, want) {
		t.Errorf("AuthorityMatrix = %v, want %v", got.AuthorityMatrix, want)
	}
}

// TestLoadManifest_AuthorityMatrixEmptyOrNull_ProjectsNilMap asserts AC1's
// empty/null branches: a `null` jsonb and an empty `{}` jsonb both yield a
// nil/empty AuthorityMatrix on runtime.Manifest (per runtime.go:107: "Nil
// is fine"). Covers the absent-column case (json.RawMessage(`null`)) and
// the explicit empty-object case (`{}`).
func TestLoadManifest_AuthorityMatrixEmptyOrNull_ProjectsNilMap(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "null", raw: json.RawMessage(`null`)},
		{name: "empty-object", raw: json.RawMessage(`{}`)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeFetcher{response: &keepclient.ManifestVersion{
				ManifestID:      "m",
				SystemPrompt:    "x",
				AuthorityMatrix: tc.raw,
			}}
			got, err := LoadManifest(context.Background(), f, "m")
			if err != nil {
				t.Fatalf("LoadManifest: %v", err)
			}
			if len(got.AuthorityMatrix) != 0 {
				t.Errorf("AuthorityMatrix = %v, want nil/empty", got.AuthorityMatrix)
			}
		})
	}
}

// TestLoadManifest_AuthorityMatrixMalformedRejected asserts AC4: a malformed
// AuthorityMatrix jsonb (here an array shape `[1,2,3]`) returns an error
// wrapped with the `manifest: authority_matrix:` prefix so callers can
// match the underlying json.Unmarshal failure mode.
func TestLoadManifest_AuthorityMatrixMalformedRejected(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:      "m",
		SystemPrompt:    "x",
		AuthorityMatrix: json.RawMessage(`[1,2,3]`),
	}}

	_, err := LoadManifest(context.Background(), f, "m")
	if err == nil {
		t.Fatalf("LoadManifest: nil error, want wrapped authority_matrix failure")
	}
	if !strings.Contains(err.Error(), "manifest: authority_matrix:") {
		t.Errorf("err = %v, want substring %q", err, "manifest: authority_matrix:")
	}
}

// TestLoadManifest_ProjectsAutonomy asserts the M5.5.b.c.c.a happy path
// (AC2): a non-empty [keepclient.ManifestVersion.Autonomy] copies verbatim
// onto [runtime.Manifest.Autonomy] cast to runtime.AutonomyLevel — no
// transformation, no default substitution.
func TestLoadManifest_ProjectsAutonomy(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Autonomy:     "autonomous",
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.Autonomy != runtime.AutonomyAutonomous {
		t.Errorf("Autonomy = %q, want %q", got.Autonomy, runtime.AutonomyAutonomous)
	}
}

// TestLoadManifest_AutonomyEmpty_ProjectsVerbatim asserts AC2's empty-string
// branch: an empty [keepclient.ManifestVersion.Autonomy] propagates as the
// empty [runtime.AutonomyLevel] — the loader does NOT default. The
// supervised-default substitution lives in the runtime per runtime.go:97
// "An empty Autonomy defaults to AutonomySupervised".
func TestLoadManifest_AutonomyEmpty_ProjectsVerbatim(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Autonomy:     "",
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.Autonomy != runtime.AutonomyLevel("") {
		t.Errorf("Autonomy = %q, want empty (loader supplies no default)", got.Autonomy)
	}
}
