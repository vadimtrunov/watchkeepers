package manifest

import (
	"context"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"
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
