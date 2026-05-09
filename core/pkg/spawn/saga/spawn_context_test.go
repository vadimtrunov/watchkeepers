package saga_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

func TestSpawnContextFromContext_Missing_ReturnsFalse(t *testing.T) {
	t.Parallel()

	_, ok := saga.SpawnContextFromContext(context.Background())
	if ok {
		t.Fatalf("SpawnContextFromContext on background ctx returned ok=true, want false")
	}
}

func TestWithSpawnContext_RoundTrip_ReturnsValue(t *testing.T) {
	t.Parallel()

	want := saga.SpawnContext{
		ManifestVersionID: uuid.New(),
		AgentID:           uuid.New(),
		Claim: saga.SpawnClaim{
			OrganizationID:  "org-123",
			AgentID:         "agent-watchmaster",
			AuthorityMatrix: map[string]string{"slack_app_create": "lead_approval"},
		},
	}

	ctx := saga.WithSpawnContext(context.Background(), want)
	got, ok := saga.SpawnContextFromContext(ctx)
	if !ok {
		t.Fatalf("SpawnContextFromContext after WithSpawnContext returned ok=false")
	}
	if got.ManifestVersionID != want.ManifestVersionID {
		t.Errorf("ManifestVersionID = %v, want %v", got.ManifestVersionID, want.ManifestVersionID)
	}
	if got.AgentID != want.AgentID {
		t.Errorf("AgentID = %v, want %v", got.AgentID, want.AgentID)
	}
	if got.Claim.OrganizationID != want.Claim.OrganizationID {
		t.Errorf("Claim.OrganizationID = %q, want %q", got.Claim.OrganizationID, want.Claim.OrganizationID)
	}
	if got.Claim.AgentID != want.Claim.AgentID {
		t.Errorf("Claim.AgentID = %q, want %q", got.Claim.AgentID, want.Claim.AgentID)
	}
	if got.Claim.AuthorityMatrix["slack_app_create"] != "lead_approval" {
		t.Errorf("Claim.AuthorityMatrix slack_app_create = %q, want %q",
			got.Claim.AuthorityMatrix["slack_app_create"], "lead_approval")
	}
}

func TestWithSpawnContext_DifferentKey_NotConfused(t *testing.T) {
	t.Parallel()

	// Pin that the SpawnContextKey is not interchangeable with a vanilla
	// string key — guards against a future refactor that accidentally
	// uses `string("spawn_context")` as the key and breaks isolation
	// between unrelated context values.
	type strKey string
	ctx := context.WithValue(context.Background(), strKey("spawn_context"), saga.SpawnContext{})
	_, ok := saga.SpawnContextFromContext(ctx)
	if ok {
		t.Fatalf("SpawnContextFromContext picked up a vanilla string-keyed value; key isolation broken")
	}
}
