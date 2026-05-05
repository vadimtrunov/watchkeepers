//go:build integration

// Integration smoke tests for the keepclient read endpoints (M2.8.b). Reuse
// the seed/boot/teardown harness defined in read_integration_test.go so the
// fixtures match the patterns proven by the M2.7.b+c read tests, then drive
// the real Keep binary via the public keepclient surface (Search /
// GetManifest / LogTail). Gated by the shared KEEP_INTEGRATION_DB_URL env
// var, so it participates in `make keep-integration-test` alongside the
// rest of the integration suite without new env requirements.
package main_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// newKeepClient builds a keepclient against the spawned binary using the
// supplied scope. It mints a fresh token via the shared TestIssuer for
// every call so leakage between tests is impossible.
func newKeepClient(t *testing.T, addr, scope string, ti *auth.TestIssuer) *keepclient.Client {
	t.Helper()
	tok, err := ti.Issue(auth.Claim{Subject: "smoke-subject", Scope: scope}, 5*time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return keepclient.NewClient(
		keepclient.WithBaseURL("http://"+addr),
		keepclient.WithTokenSource(keepclient.StaticToken(tok)),
	)
}

// newKeepClientWithOrg is the org-aware sibling of newKeepClient. The
// minted token carries OrganizationID so the spawned binary's WithScope
// sets the watchkeeper.org GUC, which the manifest / manifest_version
// RLS policies (migration 013) consult. Manifest-touching smoke tests
// MUST go through this helper; non-manifest tests can stay on the
// scope-only newKeepClient since those code paths don't hit the new
// RLS policies.
func newKeepClientWithOrg(t *testing.T, addr, scope, orgID string, ti *auth.TestIssuer) *keepclient.Client {
	t.Helper()
	tok, err := ti.Issue(auth.Claim{Subject: "smoke-subject", Scope: scope, OrganizationID: orgID}, 5*time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return keepclient.NewClient(
		keepclient.WithBaseURL("http://"+addr),
		keepclient.WithTokenSource(keepclient.StaticToken(tok)),
	)
}

// TestKeepClient_Smoke_Search_AgentScope drives the real binary with an
// agent-scope token and asserts the client decodes the seeded chunk row
// (only the agent + org rows are visible to the agent scope per RLS).
func TestKeepClient_Smoke_Search_AgentScope(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	c := newKeepClient(t, addr, env.agentScope, issuerForTest(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Search(ctx, keepclient.SearchRequest{
		Embedding: queryVec1536(),
		TopK:      10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	var sawAgent, sawUser bool
	for _, r := range resp.Results {
		switch r.Subject {
		case env.subjectTag + "-agent":
			sawAgent = true
		case env.subjectTag + "-user":
			sawUser = true
		}
	}
	if !sawAgent {
		t.Errorf("agent scope did not see agent-scoped row; results=%+v", resp.Results)
	}
	if sawUser {
		t.Errorf("agent scope leaked user-scoped row; results=%+v", resp.Results)
	}
}

// TestKeepClient_Smoke_Search_RLSIsolation mirrors the M2 verification
// block "client authenticated as agent A cannot search rows scoped to
// agent B": mint a token for an unrelated agent UUID, search the same
// fixtures, and confirm the response carries only the org-scoped row
// (the unrelated agent has no chunks of its own).
func TestKeepClient_Smoke_Search_RLSIsolation(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	otherAgentScope := "agent:" + newUUID(t)
	c := newKeepClient(t, addr, otherAgentScope, issuerForTest(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Search(ctx, keepclient.SearchRequest{
		Embedding: queryVec1536(),
		TopK:      50,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range resp.Results {
		if r.Subject == env.subjectTag+"-agent" || r.Subject == env.subjectTag+"-user" {
			t.Errorf("unrelated agent saw scoped fixture row; %+v", r)
		}
	}
}

// TestKeepClient_Smoke_GetManifest_Latest asserts the highest-version row
// is returned and decodes verbatim through the typed client model.
func TestKeepClient_Smoke_GetManifest_Latest(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	// Manifest tables are RLS-gated on `watchkeeper.org` (migration 013),
	// so the smoke client must mint a tenant-bound token to see the seed.
	c := newKeepClientWithOrg(t, addr, "org", env.orgID, issuerForTest(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mv, err := c.GetManifest(ctx, env.manifestID)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if mv.VersionNo != 2 {
		t.Errorf("VersionNo = %d, want 2 (latest)", mv.VersionNo)
	}
	if mv.ID != env.manifestVersionID {
		t.Errorf("ID = %s, want %s", mv.ID, env.manifestVersionID)
	}
	if mv.ManifestID != env.manifestID {
		t.Errorf("ManifestID = %s, want %s", mv.ManifestID, env.manifestID)
	}
}

// TestKeepClient_Smoke_GetManifest_NotFound asserts that a UUID with no
// rows surfaces as a *ServerError + ErrNotFound via errors.Is.
func TestKeepClient_Smoke_GetManifest_NotFound(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	// Use the seeded org so the not-found result reflects an actual
	// missing UUID rather than RLS hiding everything (migration 013).
	c := newKeepClientWithOrg(t, addr, "org", env.orgID, issuerForTest(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.GetManifest(ctx, newUUID(t))
	if err == nil {
		t.Fatal("expected error for unknown manifest, got nil")
	}
	var se *keepclient.ServerError
	if !errors.As(err, &se) || se.Status != 404 {
		t.Errorf("ServerError = %+v, want Status 404", se)
	}
	if !errors.Is(err, keepclient.ErrNotFound) {
		t.Error("errors.Is(err, ErrNotFound) = false")
	}
}

// TestKeepClient_Smoke_LogTail_OrderingAndFilter exercises both the typed
// LogTail and the limit query string. Filters down to the seeded events
// (other parallel tests may have inserted rows of their own) and asserts
// they appear in newest-first order. Closest available proxy to the M2
// verification line "client `log_append` -> server persists -> client
// `log_tail` returns same event"; the writer side lands in M2.8.c.
func TestKeepClient_Smoke_LogTail_OrderingAndFilter(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	c := newKeepClient(t, addr, "org", issuerForTest(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.LogTail(ctx, keepclient.LogTailOptions{Limit: 200})
	if err != nil {
		t.Fatalf("LogTail: %v", err)
	}

	// Pick the first occurrence of each seeded event_type. The relative
	// order of these three indices proves newest-first ordering survived
	// the round trip without coupling to other parallel tests' rows.
	seen := map[string]int{"newest": -1, "middle": -1, "oldest": -1}
	for i, ev := range resp.Events {
		if idx, ok := seen[ev.EventType]; ok && idx == -1 {
			seen[ev.EventType] = i
		}
	}
	if seen["newest"] == -1 || seen["middle"] == -1 || seen["oldest"] == -1 {
		t.Fatalf("missing seeded events; seen=%+v len(events)=%d", seen, len(resp.Events))
	}
	if seen["newest"] >= seen["middle"] || seen["middle"] >= seen["oldest"] {
		t.Errorf("expected newest < middle < oldest by index; got %+v", seen)
	}
}

// TestKeepClient_Smoke_LogTail_ZeroLimitOmitsQuery asserts that calling
// LogTail with the zero LogTailOptions value succeeds against the real
// server (the server applies its default limit). This is the close
// analogue to the unit test that asserts the wire shape: here we only
// check the round-trip succeeds and returns at least the seeded rows
// fit inside the server default.
func TestKeepClient_Smoke_LogTail_ZeroLimitOmitsQuery(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	c := newKeepClient(t, addr, "org", issuerForTest(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.LogTail(ctx, keepclient.LogTailOptions{})
	if err != nil {
		t.Fatalf("LogTail (zero limit): %v", err)
	}
	if resp.Events == nil {
		t.Error("Events is nil; want non-nil empty or populated slice")
	}
}
