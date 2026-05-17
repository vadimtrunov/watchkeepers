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
// into a Toolset of two ToolEntry rows whose Names() projection equals
// ["echo", "sum"]. Both rows carry empty Version (legacy shape).
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
	wantNames := []string{"echo", "sum"}
	if !reflect.DeepEqual(got.Toolset.Names(), wantNames) {
		t.Errorf("Toolset.Names() = %v, want %v", got.Toolset.Names(), wantNames)
	}
	wantEntries := runtime.Toolset{{Name: "echo"}, {Name: "sum"}}
	if !reflect.DeepEqual(got.Toolset, wantEntries) {
		t.Errorf("Toolset = %v, want %v (legacy entries decode with empty Version)", got.Toolset, wantEntries)
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

// TestLoadManifest_DecodesToolsetWithVersions asserts M5.6.e.a AC3: the
// loader decodes mv.Tools from `[{"name":"echo","version":"v1.0.0"}]`
// into a Toolset whose entry carries both Name and Version. Names()
// still yields ["echo"] for the M5.5.b.a ACL gate path.
func TestLoadManifest_DecodesToolsetWithVersions(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Tools:        json.RawMessage(`[{"name":"echo","version":"v1.0.0"}]`),
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	want := runtime.Toolset{{Name: "echo", Version: "v1.0.0"}}
	if !reflect.DeepEqual(got.Toolset, want) {
		t.Errorf("Toolset = %v, want %v", got.Toolset, want)
	}
	if !reflect.DeepEqual(got.Toolset.Names(), []string{"echo"}) {
		t.Errorf("Toolset.Names() = %v, want [\"echo\"]", got.Toolset.Names())
	}
}

// TestLoadManifest_DecodesToolsetMixedNamesAndVersions asserts M5.6.e.a
// AC3 + AC5: a mixed array with a versioned entry and a legacy
// (version-less) entry decodes into the matching ToolEntry pair, the
// legacy entry's Version is empty, and Names() preserves the manifest's
// declaration order.
func TestLoadManifest_DecodesToolsetMixedNamesAndVersions(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Tools:        json.RawMessage(`[{"name":"a","version":"v1.0"},{"name":"b"}]`),
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	want := runtime.Toolset{
		{Name: "a", Version: "v1.0"},
		{Name: "b"}, // legacy entry, empty Version
	}
	if !reflect.DeepEqual(got.Toolset, want) {
		t.Errorf("Toolset = %v, want %v", got.Toolset, want)
	}
	if !reflect.DeepEqual(got.Toolset.Names(), []string{"a", "b"}) {
		t.Errorf("Toolset.Names() = %v, want [\"a\",\"b\"] (order preserved)", got.Toolset.Names())
	}
}

// TestLoadManifest_DecodesToolsetLegacyNameOnly_HasEmptyVersion asserts
// M5.6.e.a AC5: a single legacy entry without a `version` field decodes
// successfully and the projected ToolEntry carries an empty Version.
// This is the backward-compatibility regression guard for pre-M5.6.e.a
// manifest_version.tools rows.
func TestLoadManifest_DecodesToolsetLegacyNameOnly_HasEmptyVersion(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Tools:        json.RawMessage(`[{"name":"echo"}]`),
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(got.Toolset) != 1 {
		t.Fatalf("Toolset len = %d, want 1", len(got.Toolset))
	}
	if got.Toolset[0].Name != "echo" {
		t.Errorf("Toolset[0].Name = %q, want %q", got.Toolset[0].Name, "echo")
	}
	if got.Toolset[0].Version != "" {
		t.Errorf("Toolset[0].Version = %q, want empty (legacy entries have no version)", got.Toolset[0].Version)
	}
}

// TestLoadManifest_RejectsToolsetNonStringVersion asserts M5.6.e.a AC3
// new failure mode: a `version` field that is not a string (here `42`)
// returns an error wrapped with the `manifest: toolset:` prefix so
// callers can errors.Is the underlying json.Unmarshal failure.
func TestLoadManifest_RejectsToolsetNonStringVersion(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Tools:        json.RawMessage(`[{"name":"x","version":42}]`),
	}}

	_, err := LoadManifest(context.Background(), f, "m")
	if err == nil {
		t.Fatalf("LoadManifest: nil error, want wrapped non-string-version failure")
	}
	if !strings.Contains(err.Error(), "manifest: toolset:") {
		t.Errorf("err = %v, want substring %q", err, "manifest: toolset:")
	}
}

// TestLoadManifest_RejectsToolsetEmptyName asserts the existing AC3
// negative still applies after the M5.6.e.a type migration: an entry
// with an empty `name` returns the deterministic
// `manifest: toolset: entry N has empty name` sentinel.
func TestLoadManifest_RejectsToolsetEmptyName(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Tools:        json.RawMessage(`[{"name":""}]`),
	}}

	_, err := LoadManifest(context.Background(), f, "m")
	if err == nil {
		t.Fatalf("LoadManifest: nil error, want empty-name rejection")
	}
	if !strings.Contains(err.Error(), "manifest: toolset: entry 0 has empty name") {
		t.Errorf("err = %v, want substring %q", err, "manifest: toolset: entry 0 has empty name")
	}
}

// TestLoadManifest_RejectsToolsetMissingName asserts that an entry
// supplying only a `version` (no `name`) is treated identically to an
// empty-name entry per the M5.6.e.a contract — name is required, and
// json.Unmarshal leaves a missing-key field as the zero value, which
// the loader rejects via the same `entry N has empty name` path.
func TestLoadManifest_RejectsToolsetMissingName(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Tools:        json.RawMessage(`[{"version":"v1.0"}]`),
	}}

	_, err := LoadManifest(context.Background(), f, "m")
	if err == nil {
		t.Fatalf("LoadManifest: nil error, want missing-name rejection")
	}
	if !strings.Contains(err.Error(), "manifest: toolset: entry 0 has empty name") {
		t.Errorf("err = %v, want substring %q", err, "manifest: toolset: entry 0 has empty name")
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

// TestLoadManifest_ProjectsNotebookRecallFields asserts M5.5.c.b AC4: a
// non-zero [keepclient.ManifestVersion.NotebookTopK] and
// [keepclient.ManifestVersion.NotebookRelevanceThreshold] copy verbatim
// onto [runtime.Manifest.NotebookTopK] and
// [runtime.Manifest.NotebookRelevanceThreshold] — no transformation.
func TestLoadManifest_ProjectsNotebookRecallFields(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:                 "m",
		SystemPrompt:               "x",
		NotebookTopK:               20,
		NotebookRelevanceThreshold: 0.75,
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.NotebookTopK != 20 {
		t.Errorf("NotebookTopK = %d, want 20", got.NotebookTopK)
	}
	if got.NotebookRelevanceThreshold != 0.75 {
		t.Errorf("NotebookRelevanceThreshold = %v, want 0.75", got.NotebookRelevanceThreshold)
	}
}

// TestLoadManifest_ProjectsRememberInToolset asserts M5.5.d.c AC1: a
// [keepclient.ManifestVersion] whose Tools jsonb contains `{"name":"remember"}`
// projects to Toolset = ["remember"] on [runtime.Manifest]. This fixture makes
// the projection of the "remember" builtin tool name explicit as the upstream
// source fed into the harness ACL gate (M5.5.d.b) and e2e tests (M5.5.d.c).
//
// "remember" is a plain string entry — no special decode logic vs. any other
// name — so this test is a named regression guard rather than a new code path.
// It documents that decodeToolset projects the builtin tool name correctly and
// can be used as the canonical fixture reference in downstream ACL/e2e tests.
func TestLoadManifest_ProjectsRememberInToolset(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		Tools:        json.RawMessage(`[{"name":"remember"}]`),
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(got.Toolset) != 1 || got.Toolset[0].Name != "remember" {
		t.Errorf("Toolset = %v, want [{Name:\"remember\"}]", got.Toolset)
	}
}

// TestLoadManifest_NotebookRecallFields_ZeroPassThrough asserts M5.5.c.b
// AC4: zero values from [keepclient.ManifestVersion] project to zero on
// [runtime.Manifest] — no transformation, no default substitution.
func TestLoadManifest_NotebookRecallFields_ZeroPassThrough(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
		// NotebookTopK and NotebookRelevanceThreshold are zero (unset).
	}}

	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.NotebookTopK != 0 {
		t.Errorf("NotebookTopK = %d, want 0 (zero pass-through)", got.NotebookTopK)
	}
	if got.NotebookRelevanceThreshold != 0 {
		t.Errorf("NotebookRelevanceThreshold = %v, want 0 (zero pass-through)", got.NotebookRelevanceThreshold)
	}
}

// -----------------------------------------------------------------------
// M3.1 — immutable_core projection into [runtime.Manifest.ImmutableCore]
// -----------------------------------------------------------------------

// TestLoadManifest_ImmutableCore_NilByDefault asserts that a manifest
// version with no `immutable_core` jsonb (nil RawMessage) projects to a
// nil [runtime.Manifest.ImmutableCore] pointer — the "no governance
// declared yet" sentinel documented on the runtime field. Mirrors the
// pattern from [decodeAuthorityMatrix] / [decodeToolset].
func TestLoadManifest_ImmutableCore_NilByDefault(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
	}}
	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.ImmutableCore != nil {
		t.Errorf("ImmutableCore = %+v, want nil (no immutable_core declared)", got.ImmutableCore)
	}
}

// TestLoadManifest_ImmutableCore_NullLiteralProjectsNil asserts that the
// JSON literal `null` (a row that was explicitly cleared rather than
// never set) projects to a nil pointer — same shape as the absent case.
// Symmetric with the AuthorityMatrix / Toolset null-literal treatment.
func TestLoadManifest_ImmutableCore_NullLiteralProjectsNil(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:    "m",
		SystemPrompt:  "x",
		ImmutableCore: json.RawMessage(`null`),
	}}
	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.ImmutableCore != nil {
		t.Errorf("ImmutableCore = %+v, want nil for JSON null literal", got.ImmutableCore)
	}
}

// TestLoadManifest_ImmutableCore_EmptyObjectProjectsNil asserts that the
// JSON literal `{}` (an explicitly-empty but valid object) projects to a
// nil pointer — bucket-absent everywhere reads as "no overrides".
// Symmetric with the AuthorityMatrix empty-map projection.
func TestLoadManifest_ImmutableCore_EmptyObjectProjectsNil(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:    "m",
		SystemPrompt:  "x",
		ImmutableCore: json.RawMessage(`{}`),
	}}
	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.ImmutableCore != nil {
		t.Errorf("ImmutableCore = %+v, want nil for empty object", got.ImmutableCore)
	}
}

// TestLoadManifest_ImmutableCore_AllFiveBucketsProjected asserts that
// the canonical Phase 2 §M3.1 five-bucket payload projects onto the
// typed fields verbatim. Each bucket is exercised so a regression in
// any single field name (e.g. a typo on `cost_limits` →
// `cost_limit`) surfaces immediately.
func TestLoadManifest_ImmutableCore_AllFiveBucketsProjected(t *testing.T) {
	t.Parallel()

	const payload = `{
		"role_boundaries":["delete_production_data","spawn_subagents"],
		"security_constraints":{"pii_export":"forbidden","classification_floor":"internal"},
		"escalation_protocols":{"pii_leak":"#security-on-call","cost_breach":"#ops-leads"},
		"cost_limits":{"per_task_tokens":50000,"per_day_tokens":500000},
		"audit_requirements":{"manifest_changes":"retain_forever","tool_invocations":"retain_90d"}
	}`
	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:    "m",
		SystemPrompt:  "x",
		ImmutableCore: json.RawMessage(payload),
	}}
	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.ImmutableCore == nil {
		t.Fatal("ImmutableCore is nil; want non-nil typed projection")
	}
	wantRoleBoundaries := []string{"delete_production_data", "spawn_subagents"}
	if !reflect.DeepEqual(got.ImmutableCore.RoleBoundaries, wantRoleBoundaries) {
		t.Errorf("RoleBoundaries = %v, want %v", got.ImmutableCore.RoleBoundaries, wantRoleBoundaries)
	}
	if v := got.ImmutableCore.SecurityConstraints["pii_export"]; v != "forbidden" {
		t.Errorf("SecurityConstraints[pii_export] = %v, want forbidden", v)
	}
	if v := got.ImmutableCore.EscalationProtocols["pii_leak"]; v != "#security-on-call" {
		t.Errorf("EscalationProtocols[pii_leak] = %v, want #security-on-call", v)
	}
	if v, ok := got.ImmutableCore.CostLimits["per_task_tokens"].(float64); !ok || v != 50000 {
		t.Errorf("CostLimits[per_task_tokens] = %v, want 50000", got.ImmutableCore.CostLimits["per_task_tokens"])
	}
	if v := got.ImmutableCore.AuditRequirements["manifest_changes"]; v != "retain_forever" {
		t.Errorf("AuditRequirements[manifest_changes] = %v, want retain_forever", v)
	}
	if got.ImmutableCore.Extra != nil {
		t.Errorf("Extra = %v, want nil (no forward-compat buckets in this payload)", got.ImmutableCore.Extra)
	}
}

// TestLoadManifest_ImmutableCore_UnknownBucketRidesIntoExtra asserts
// the forward-compatibility contract: a sixth (M3.4+) bucket on the
// wire decodes into [runtime.ImmutableCore.Extra] verbatim rather than
// being silently dropped. The canonical M3.1 buckets still project
// onto their typed fields.
func TestLoadManifest_ImmutableCore_UnknownBucketRidesIntoExtra(t *testing.T) {
	t.Parallel()

	const payload = `{"role_boundaries":["x"],"merge_history":{"v3":"by_lead"}}`
	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:    "m",
		SystemPrompt:  "x",
		ImmutableCore: json.RawMessage(payload),
	}}
	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.ImmutableCore == nil {
		t.Fatal("ImmutableCore is nil; want non-nil typed projection")
	}
	if !reflect.DeepEqual(got.ImmutableCore.RoleBoundaries, []string{"x"}) {
		t.Errorf("RoleBoundaries = %v, want [x]", got.ImmutableCore.RoleBoundaries)
	}
	extraVal, ok := got.ImmutableCore.Extra["merge_history"]
	if !ok {
		t.Fatalf("Extra[merge_history] missing; Extra=%v", got.ImmutableCore.Extra)
	}
	var extraObj map[string]string
	if err := json.Unmarshal(extraVal, &extraObj); err != nil {
		t.Fatalf("Extra[merge_history] decode: %v", err)
	}
	if extraObj["v3"] != "by_lead" {
		t.Errorf("Extra[merge_history].v3 = %v, want by_lead", extraObj["v3"])
	}
}

// TestLoadManifest_ImmutableCore_MalformedRejected asserts that a
// non-object top-level (array / scalar) or malformed JSON surfaces as a
// wrapped `manifest: immutable_core:` error rather than silently
// coercing. The server-side CHECK constraint enforces this at the SQL
// layer; the loader is defense-in-depth for any code path that does not
// go through the validated PUT path (e.g. a test seeding rows via raw
// SQL).
func TestLoadManifest_ImmutableCore_MalformedRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  json.RawMessage
	}{
		{"array", json.RawMessage(`[1,2,3]`)},
		{"string", json.RawMessage(`"oops"`)},
		{"number", json.RawMessage(`42`)},
		{"malformed", json.RawMessage(`{not-json`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeFetcher{response: &keepclient.ManifestVersion{
				ManifestID:    "m",
				SystemPrompt:  "x",
				ImmutableCore: tc.raw,
			}}
			_, err := LoadManifest(context.Background(), f, "m")
			if err == nil {
				t.Fatalf("err = nil, want wrapped immutable_core decode error")
			}
			if !strings.Contains(err.Error(), "manifest: immutable_core") {
				t.Errorf("err = %q, want it to contain 'manifest: immutable_core'", err.Error())
			}
		})
	}
}

// TestLoadManifest_ImmutableCore_TypedBucketTypeMismatchRidesThroughExtra
// asserts the M3.1 tolerant-decode contract (codex iter-1 P1 finding):
// when a bucket's wire payload does not match the canonical typed
// shape the write path admitted (the write path enforces "top-level
// object" only), the loader leaves the typed field at its zero value
// and stashes the raw bucket payload under its canonical key on
// [runtime.ImmutableCore.Extra] — rather than turning a
// successfully-stored manifest into an unloadable runtime manifest.
// The strict per-bucket shape gate lives in M3.6's self-tuning
// validator.
func TestLoadManifest_ImmutableCore_TypedBucketTypeMismatchRidesThroughExtra(t *testing.T) {
	t.Parallel()

	const payload = `{"role_boundaries":{"oops":"object_not_array"},"cost_limits":{"per_task_tokens":1000}}`
	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:    "m",
		SystemPrompt:  "x",
		ImmutableCore: json.RawMessage(payload),
	}}
	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v (M3.1 tolerant-decode contract — must not reject)", err)
	}
	if got.ImmutableCore == nil {
		t.Fatal("ImmutableCore = nil; want non-nil projection so cost_limits + Extra survive")
	}
	// role_boundaries failed the typed decode (object-not-array) — the
	// typed field stays nil, the raw payload rides through Extra.
	if got.ImmutableCore.RoleBoundaries != nil {
		t.Errorf("RoleBoundaries = %v, want nil (typed decode failed)", got.ImmutableCore.RoleBoundaries)
	}
	extra, ok := got.ImmutableCore.Extra["role_boundaries"]
	if !ok {
		t.Fatalf("Extra[role_boundaries] missing; Extra=%v (raw payload must ride through)", got.ImmutableCore.Extra)
	}
	// Re-decode confirms the raw bytes survived verbatim.
	var rawObj map[string]string
	if err := json.Unmarshal(extra, &rawObj); err != nil {
		t.Fatalf("Extra[role_boundaries] decode: %v", err)
	}
	if rawObj["oops"] != "object_not_array" {
		t.Errorf("Extra[role_boundaries].oops = %v, want object_not_array", rawObj["oops"])
	}
	// cost_limits matched the typed shape so it lands on the typed
	// field, NOT on Extra (verifies the partial-success branch).
	if v, ok := got.ImmutableCore.CostLimits["per_task_tokens"].(float64); !ok || v != 1000 {
		t.Errorf("CostLimits[per_task_tokens] = %v, want 1000", got.ImmutableCore.CostLimits["per_task_tokens"])
	}
	if _, present := got.ImmutableCore.Extra["cost_limits"]; present {
		t.Errorf("Extra[cost_limits] present; want absent (typed decode succeeded)")
	}
}

// -----------------------------------------------------------------------
// Phase 2 §M3.3 — manifest_version metadata projection
// -----------------------------------------------------------------------

// TestLoadManifest_Metadata_Projected asserts that the three Phase 2
// §M3.3 metadata fields (reason / previous_version_id / proposer) ride
// from the wire `keepclient.ManifestVersion` onto the typed runtime
// fields verbatim. PreviousVersionID is flattened from `*string` to
// `string` at the loader boundary (empty string ⇒ root version), so the
// non-nil-pointer case must surface as a non-empty string.
func TestLoadManifest_Metadata_Projected(t *testing.T) {
	t.Parallel()

	const (
		wantReason            = "lead-approved rollback to last Friday's version"
		wantPreviousVersionID = "22222222-2222-4222-8222-222222222222"
		wantProposer          = "watchmaster"
	)
	prev := wantPreviousVersionID
	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:        "m",
		SystemPrompt:      "x",
		Reason:            wantReason,
		PreviousVersionID: &prev,
		Proposer:          wantProposer,
	}}
	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.Reason != wantReason {
		t.Errorf("Reason = %q, want %q", got.Reason, wantReason)
	}
	if got.PreviousVersionID != wantPreviousVersionID {
		t.Errorf("PreviousVersionID = %q, want %q", got.PreviousVersionID, wantPreviousVersionID)
	}
	if got.Proposer != wantProposer {
		t.Errorf("Proposer = %q, want %q", got.Proposer, wantProposer)
	}
}

// TestLoadManifest_Metadata_OmittedStaysZero asserts that a manifest
// version whose three M3.3 metadata fields are unset (nil pointer for
// PreviousVersionID, empty strings for Reason / Proposer — the legacy
// Phase 1 row OR a row that simply omitted them) projects to the zero
// values on the runtime side. Mirrors the wire-omit posture documented
// on [runtime.Manifest.Reason] / [runtime.Manifest.PreviousVersionID] /
// [runtime.Manifest.Proposer].
func TestLoadManifest_Metadata_OmittedStaysZero(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "x",
	}}
	got, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.Reason != "" {
		t.Errorf("Reason = %q, want empty", got.Reason)
	}
	if got.PreviousVersionID != "" {
		t.Errorf("PreviousVersionID = %q, want empty (root version)", got.PreviousVersionID)
	}
	if got.Proposer != "" {
		t.Errorf("Proposer = %q, want empty", got.Proposer)
	}
}
