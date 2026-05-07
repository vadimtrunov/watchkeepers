package manifest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// TestLoadManifest_ChainsThroughBuildCompleteRequest asserts M5.5.b.b.c
// AC4: the Model identifier round-trips byte-for-byte through the full
// chain ManifestVersion → LoadManifest → llm.BuildCompleteRequest →
// CompleteRequest.Model. This is the integration-style proof that the
// loader's projection slot is actually wired to the LLM builder consumer
// (which already validates non-empty Model per
// `manifest_request.go:96-98`); a regression in either layer fails this
// test, not the unit tests on either side.
//
// Lives in a separate file so the `core/pkg/llm` import dependency stays
// scoped to the chain-test surface; the unit tests in loader_test.go
// remain free of cross-package imports beyond runtime + keepclient.
func TestLoadManifest_ChainsThroughBuildCompleteRequest(t *testing.T) {
	t.Parallel()

	const wantModel = "claude-sonnet-4"
	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:   "m",
		SystemPrompt: "You are X.",
		Model:        wantModel,
	}}

	m, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	req, err := llm.BuildCompleteRequest(m, []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if string(req.Model) != wantModel {
		t.Errorf("CompleteRequest.Model = %q, want %q", req.Model, wantModel)
	}
}

// TestLoadManifest_AuthorityAutonomyChain asserts M5.5.b.c.c.a AC6: the
// AuthorityMatrix jsonb and Autonomy string round-trip from the wire-format
// [keepclient.ManifestVersion] through [LoadManifest] onto
// [runtime.Manifest.AuthorityMatrix] and [runtime.Manifest.Autonomy]. This
// is the integration-style proof that the loader's projection slots are
// actually wired through the struct initialiser; a regression in either
// slot fails this test, not the unit tests on either side.
//
// Mirrors the M5.5.b.b.c chain test pattern: fakeFetcher returns a
// representative ManifestVersion, LoadManifest runs, and the resulting
// runtime.Manifest is interrogated for both projected fields in one go.
func TestLoadManifest_AuthorityAutonomyChain(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:      "m",
		SystemPrompt:    "You are X.",
		AuthorityMatrix: json.RawMessage(`{"deploy":"lead","spawn":"watchmaster"}`),
		Autonomy:        "autonomous",
	}}

	m, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got, want := m.AuthorityMatrix["deploy"], "lead"; got != want {
		t.Errorf("AuthorityMatrix[deploy] = %q, want %q", got, want)
	}
	if m.Autonomy != runtime.AutonomyAutonomous {
		t.Errorf("Autonomy = %q, want %q", m.Autonomy, runtime.AutonomyAutonomous)
	}
}

// TestLoadManifest_NotebookRecallChain asserts M5.5.c.b AC4+AC5: the
// NotebookTopK and NotebookRelevanceThreshold fields round-trip from the
// wire-format [keepclient.ManifestVersion] through [LoadManifest] onto
// [runtime.Manifest.NotebookTopK] and
// [runtime.Manifest.NotebookRelevanceThreshold]. This is the
// integration-style proof that the loader's projection slots are wired
// through the struct initialiser; a regression in either slot fails this
// test, not the unit tests on either side.
//
// Mirrors the M5.5.b.c.c.a chain test pattern.
func TestLoadManifest_NotebookRecallChain(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: &keepclient.ManifestVersion{
		ManifestID:                 "m",
		SystemPrompt:               "You are X.",
		NotebookTopK:               42,
		NotebookRelevanceThreshold: 0.6,
	}}

	m, err := LoadManifest(context.Background(), f, "m")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.NotebookTopK != 42 {
		t.Errorf("NotebookTopK = %d, want 42", m.NotebookTopK)
	}
	if m.NotebookRelevanceThreshold != 0.6 {
		t.Errorf("NotebookRelevanceThreshold = %v, want 0.6", m.NotebookRelevanceThreshold)
	}
}
