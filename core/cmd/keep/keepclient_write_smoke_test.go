//go:build integration

// Integration smoke tests for the keepclient write endpoints (M2.8.c). Reuse
// the seed/boot/teardown harness from read_integration_test.go so the
// fixtures match the patterns proven by the M2.7.d write tests, then drive
// the real Keep binary via the public keepclient surface (Store / LogAppend
// / PutManifestVersion). Gated by the shared KEEP_INTEGRATION_DB_URL env
// var, so it participates in `make keep-integration-test` alongside the
// rest of the integration suite without new env requirements.
package main_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// TestKeepClient_Smoke_Store_AgentScope round-trips Store under an agent
// scope and asserts a fresh row id is returned. The agent-scope visibility
// invariant is already proven by TestWriteAPI_Store_AgentScopeVisibility on
// the raw HTTP layer; here we focus on the typed-client wire shape.
func TestKeepClient_Smoke_Store_AgentScope(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	c := newKeepClient(t, addr, env.agentScope, issuerForTest(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Store(ctx, keepclient.StoreRequest{
		Subject:   env.subjectTag + "-client-store",
		Content:   "fresh content via keepclient",
		Embedding: queryVec1536(),
	})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if resp.ID == "" {
		t.Fatal("Store returned empty id")
	}
}

// TestKeepClient_Smoke_LogAppend_AgentScope round-trips LogAppend under an
// agent scope and asserts a fresh row id is returned. The actor-stamping
// invariant is already proven by TestWriteAPI_LogAppend_AgentActor on the
// raw HTTP layer; here we focus on the typed-client wire shape.
func TestKeepClient_Smoke_LogAppend_AgentScope(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	c := newKeepClient(t, addr, env.agentScope, issuerForTest(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.LogAppend(ctx, keepclient.LogAppendRequest{
		EventType: env.subjectTag + "-client-log",
		Payload:   json.RawMessage(`{"via":"keepclient"}`),
	})
	if err != nil {
		t.Fatalf("LogAppend: %v", err)
	}
	if resp.ID == "" {
		t.Fatal("LogAppend returned empty id")
	}
}

// TestKeepClient_Smoke_PutManifestVersion_NewAndConflict performs the M2
// verification line "client put_manifest_version -> server persists ->
// duplicate (manifest_id, version_no) returns 409". The first PUT inserts a
// version_no above the seeded max (1, 2); the second PUT with the same
// version_no must surface as ErrConflict via errors.Is.
func TestKeepClient_Smoke_PutManifestVersion_NewAndConflict(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	c := newKeepClient(t, addr, "org", issuerForTest(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const newVersion = 7
	first, err := c.PutManifestVersion(ctx, env.manifestID, keepclient.PutManifestVersionRequest{
		VersionNo:    newVersion,
		SystemPrompt: "v" + env.subjectTag + " prompt",
	})
	if err != nil {
		t.Fatalf("PutManifestVersion (first): %v", err)
	}
	if first.ID == "" {
		t.Fatal("first PutManifestVersion returned empty id")
	}

	// Second PUT with the same (manifest_id, version_no) must fail with 409.
	_, err = c.PutManifestVersion(ctx, env.manifestID, keepclient.PutManifestVersionRequest{
		VersionNo:    newVersion,
		SystemPrompt: "duplicate",
	})
	if err == nil {
		t.Fatal("duplicate PutManifestVersion succeeded; want ErrConflict")
	}
	var se *keepclient.ServerError
	if !errors.As(err, &se) || se.Status != 409 {
		t.Errorf("ServerError = %+v, want Status 409", se)
	}
	if !errors.Is(err, keepclient.ErrConflict) {
		t.Errorf("errors.Is(err, ErrConflict) = false; err = %v", err)
	}

	// The latest version returned by GET /v1/manifests/{id} must still be
	// the row inserted by the first PUT (the conflict must not overwrite
	// or shift the head pointer).
	mv, err := c.GetManifest(ctx, env.manifestID)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if mv.VersionNo != newVersion {
		t.Errorf("VersionNo = %d, want %d (latest = first PUT)", mv.VersionNo, newVersion)
	}
	if mv.ID != first.ID {
		t.Errorf("ID = %s, want %s (first PUT row)", mv.ID, first.ID)
	}
}
