package spawn_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// TestMemoryPendingApprovalDAO_InsertGetRoundTrip pins the AC5 happy
// path: an Insert + Get round-trips the token, tool name, params
// snapshot, and an initial `pending` state.
func TestMemoryPendingApprovalDAO_InsertGetRoundTrip(t *testing.T) {
	t.Parallel()

	clock := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	dao := spawn.NewMemoryPendingApprovalDAO(func() time.Time { return clock })

	params := json.RawMessage(`{"agent_id":"abc","new_personality":"calm"}`)
	if err := dao.Insert(context.Background(), "tok-1", spawn.PendingApprovalToolAdjustPersonality, params); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := dao.Get(context.Background(), "tok-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ApprovalToken != "tok-1" {
		t.Errorf("ApprovalToken = %q, want %q", got.ApprovalToken, "tok-1")
	}
	if got.ToolName != spawn.PendingApprovalToolAdjustPersonality {
		t.Errorf("ToolName = %q, want %q", got.ToolName, spawn.PendingApprovalToolAdjustPersonality)
	}
	if string(got.ParamsJSON) != string(params) {
		t.Errorf("ParamsJSON = %s, want %s", got.ParamsJSON, params)
	}
	if got.State != spawn.PendingApprovalStatePending {
		t.Errorf("State = %q, want %q", got.State, spawn.PendingApprovalStatePending)
	}
	if !got.RequestedAt.Equal(clock) {
		t.Errorf("RequestedAt = %v, want %v", got.RequestedAt, clock)
	}
	if !got.ResolvedAt.IsZero() {
		t.Errorf("ResolvedAt = %v, want zero", got.ResolvedAt)
	}
}

// TestMemoryPendingApprovalDAO_GetUnknown pins the AC5 negative path:
// Get on an unknown token returns [ErrPendingApprovalNotFound].
func TestMemoryPendingApprovalDAO_GetUnknown(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	_, err := dao.Get(context.Background(), "nope")
	if !errors.Is(err, spawn.ErrPendingApprovalNotFound) {
		t.Fatalf("err = %v, want ErrPendingApprovalNotFound", err)
	}
}

// TestMemoryPendingApprovalDAO_ResolveHappy pins the AC5 happy
// transition: pending → approved, resolved_at populated.
func TestMemoryPendingApprovalDAO_ResolveHappy(t *testing.T) {
	t.Parallel()

	requestedAt := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	resolvedAt := requestedAt.Add(5 * time.Minute)

	current := requestedAt
	dao := spawn.NewMemoryPendingApprovalDAO(func() time.Time { return current })

	if err := dao.Insert(context.Background(), "tok-2", spawn.PendingApprovalToolProposeSpawn, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	current = resolvedAt
	if err := dao.Resolve(context.Background(), "tok-2", spawn.PendingApprovalStateApproved); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got, err := dao.Get(context.Background(), "tok-2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != spawn.PendingApprovalStateApproved {
		t.Errorf("State = %q, want %q", got.State, spawn.PendingApprovalStateApproved)
	}
	if !got.ResolvedAt.Equal(resolvedAt) {
		t.Errorf("ResolvedAt = %v, want %v", got.ResolvedAt, resolvedAt)
	}
}

// TestMemoryPendingApprovalDAO_ResolveStale pins the AC5 negative
// path: resolve on a row whose state is already terminal returns
// [ErrPendingApprovalStaleState] and does NOT mutate the row.
func TestMemoryPendingApprovalDAO_ResolveStale(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	_ = dao.Insert(context.Background(), "tok-3", spawn.PendingApprovalToolAdjustLanguage, json.RawMessage(`{}`))
	if err := dao.Resolve(context.Background(), "tok-3", spawn.PendingApprovalStateApproved); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	err := dao.Resolve(context.Background(), "tok-3", spawn.PendingApprovalStateRejected)
	if !errors.Is(err, spawn.ErrPendingApprovalStaleState) {
		t.Fatalf("second Resolve err = %v, want ErrPendingApprovalStaleState", err)
	}
	got, _ := dao.Get(context.Background(), "tok-3")
	if got.State != spawn.PendingApprovalStateApproved {
		t.Errorf("State after stale Resolve = %q, want approved (no mutation)", got.State)
	}
}

// TestMemoryPendingApprovalDAO_ResolveUnknown pins the AC5 negative
// path: Resolve on an unknown token returns ErrPendingApprovalNotFound.
func TestMemoryPendingApprovalDAO_ResolveUnknown(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	err := dao.Resolve(context.Background(), "nope", spawn.PendingApprovalStateApproved)
	if !errors.Is(err, spawn.ErrPendingApprovalNotFound) {
		t.Fatalf("err = %v, want ErrPendingApprovalNotFound", err)
	}
}

// TestMemoryPendingApprovalDAO_ResolveInvalidDecision pins the
// closed-set vocabulary: only `approved` and `rejected` are valid
// resolution targets; `pending` is the initial state and an empty
// string is rejected as a programmer error.
func TestMemoryPendingApprovalDAO_ResolveInvalidDecision(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	_ = dao.Insert(context.Background(), "tok-4", spawn.PendingApprovalToolRetireWatchkeeper, json.RawMessage(`{}`))
	for _, decision := range []spawn.PendingApprovalDecision{
		spawn.PendingApprovalStatePending,
		spawn.PendingApprovalDecision(""),
		spawn.PendingApprovalDecision("garbage"),
	} {
		if err := dao.Resolve(context.Background(), "tok-4", decision); !errors.Is(err, spawn.ErrPendingApprovalInvalidDecision) {
			t.Errorf("Resolve(%q) err = %v, want ErrPendingApprovalInvalidDecision", decision, err)
		}
	}
}

// TestMigration018_Schema is the AC4 binding cross-link: read
// `deploy/migrations/018_pending_approvals.sql` off disk and assert
// the load-bearing column declarations + check constraint are
// present. Mirrors the M6.1.a watchmaster_seed migration-shape test
// pattern (string-match on canonical literals; no live Postgres
// stand-up).
func TestMigration018_Schema(t *testing.T) {
	t.Parallel()

	path := repoRelativeForApproval(t, "deploy/migrations/018_pending_approvals.sql")
	body, err := os.ReadFile(path) //nolint:gosec // test reads a fixed repo-relative path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	src := string(body)

	wantLiterals := []string{
		// Table name.
		"CREATE TABLE watchkeeper.pending_approvals",
		// Columns.
		"approval_token text PRIMARY KEY",
		"tool_name text NOT NULL",
		"params_json jsonb NOT NULL",
		"state text NOT NULL DEFAULT 'pending'",
		"requested_at timestamptz NOT NULL DEFAULT now()",
		"resolved_at timestamptz NULL",
		// CHECK constraint pinning the closed-set vocabulary.
		"state IN ('pending', 'approved', 'rejected')",
		// Goose down.
		"DROP TABLE IF EXISTS watchkeeper.pending_approvals",
	}
	for _, lit := range wantLiterals {
		if !strings.Contains(src, lit) {
			t.Errorf("migration missing literal %q", lit)
		}
	}
}

// repoRelativeForApproval resolves a repo-relative path to an
// absolute path by climbing from this test file up to the repo root.
// Mirrors the M6.1.a manifest seed test helper.
func repoRelativeForApproval(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile is .../core/pkg/spawn/pending_approval_test.go;
	// repo root is three directories up.
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	return filepath.Join(repoRoot, rel)
}
